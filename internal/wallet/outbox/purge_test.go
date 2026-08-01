package outbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakePurgeStore struct {
	before  chan time.Time
	deleted int64
	err     error
}

func (f *fakePurgeStore) PurgeSentOutbox(_ context.Context, before time.Time) (int64, error) {
	select {
	case f.before <- before:
	default:
	}
	return f.deleted, f.err
}

func TestNextPurgeAt(t *testing.T) {
	tests := []struct {
		name string
		now  string
		want string
		why  string
	}{
		{
			name: "還沒到今天的時刻",
			now:  "2026-08-02T09:30:00Z", want: "2026-08-02T20:00:00Z",
			why: "排今天",
		},
		{
			name: "已經過了今天的時刻",
			now:  "2026-08-02T20:00:01Z", want: "2026-08-03T20:00:00Z",
			why: "排明天",
		},
		{
			name: "剛好等於排程時刻",
			now:  "2026-08-02T20:00:00Z", want: "2026-08-03T20:00:00Z",
			why: "等於要視同已過，否則 time.Until 會是 0，迴圈立刻再跑一次形成忙碌迴圈",
		},
		{
			name: "跨月",
			now:  "2026-08-31T23:00:00Z", want: "2026-09-01T20:00:00Z",
			why: "AddDate 會自己處理月底，手動 +24h 也可以但意圖不明顯",
		},
		{
			name: "非 UTC 的輸入要先換算",
			// 2026-08-02T23:00:00+08:00 ＝ 15:00 UTC，所以下一次是同一天的 20:00 UTC。
			now: "2026-08-02T23:00:00+08:00", want: "2026-08-02T20:00:00Z",
			why: "傳進來的 time.Time 帶什麼時區都不影響結果，排程一律以 UTC 計算",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatalf("測試資料寫錯: %v", err)
			}
			want, err := time.Parse(time.RFC3339, tt.want)
			if err != nil {
				t.Fatalf("測試資料寫錯: %v", err)
			}
			if got := nextPurgeAt(now, purgeHourUTC); !got.Equal(want) {
				t.Errorf("got %s, want %s（%s）", got.Format(time.RFC3339), want.Format(time.RFC3339), tt.why)
			}
		})
	}
}

// ⭐ TestPurgeHourIsOffPeakInTaipei 釘住「20 不是隨便挑的」。
//
// Java 寫的是 cron `0 0 4 * * *`，而 Spring 的 cron 跑在容器的本地時區
// （Asia/Taipei）。本專案一律 UTC，照抄那個 4 會變成**台北中午十二點**跑
// 批次刪除——玩家最活躍的時段，而「排在離峰」正是這個排程唯一的排程理由。
// ⚠️ 沒有任何錯誤訊息，只會看到「每天中午 DB 有個尖峰」。
func TestPurgeHourIsOffPeakInTaipei(t *testing.T) {
	const taipeiOffsetHours = 8
	if got := (purgeHourUTC + taipeiOffsetHours) % 24; got != 4 {
		t.Errorf("purgeHourUTC=%d 換算成台北時間是 %d 點，want 4 點（對齊 Java 的 cron，離峰時段）",
			purgeHourUTC, got)
	}
}

// TestPurgerRunOnceUsesRetentionWindow 釘住「刪的是保留期之外的列」。
//
// ⚠️ 這裡算錯方向（把 before 算成 now+retention）不會有任何錯誤訊息，
// 只會把**剛剛才送出去的事件**刪光，而事故排查時 outbox 是唯一能回答
// 「這則事件到底有沒有發出去」的證據。
func TestPurgerRunOnceUsesRetentionWindow(t *testing.T) {
	const retention = 7 * 24 * time.Hour
	s := &fakePurgeStore{before: make(chan time.Time, 1), deleted: 3}

	start := time.Now().UTC()
	NewPurger(s, retention, discardLogger()).RunOnce(context.Background())

	select {
	case got := <-s.before:
		want := start.Add(-retention)
		// 允許幾秒誤差：兩個 time.Now() 之間隔了一次函式呼叫。
		if diff := got.Sub(want); diff < -5*time.Second || diff > 5*time.Second {
			t.Errorf("before = %s, want ≈ %s（now - 保留期）", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	default:
		t.Fatal("RunOnce 沒有呼叫 PurgeSentOutbox")
	}
}

// TestPurgerRunOnceSurvivesFailure 釘住「清理失敗不可以讓排程死掉」。
//
// 對齊 Java 的 catch：清理是純維運工作，延一天做完全無害，
// 但讓迴圈死掉的話就再也不會有人清了——而這件事沒有任何指標看得出來。
func TestPurgerRunOnceSurvivesFailure(t *testing.T) {
	s := &fakePurgeStore{before: make(chan time.Time, 1), err: errors.New("DB 掛了")}
	purger := NewPurger(s, 24*time.Hour, discardLogger())

	// 連跑兩次：第一次失敗之後，第二次還是要照樣呼叫下去。
	purger.RunOnce(context.Background())
	<-s.before
	purger.RunOnce(context.Background())

	select {
	case <-s.before:
	default:
		t.Error("第一次清理失敗之後就不再嘗試了")
	}
}

// TestPurgerRunOnceTreatsCancelAsNormal 釘住「關機中斷不是故障」。
//
// ⚠️ 記成 Error 的話，每次關機都會留一行紅的——然後真正的清理失敗
// 就沒有人會注意到了。告警要能被相信，前提是它不會固定為某件正常的事而響。
func TestPurgerRunOnceTreatsCancelAsNormal(t *testing.T) {
	// 這裡不必加鎖：RunOnce 是同步的，只有這一個 goroutine 在寫。
	logs := &bytes.Buffer{}
	s := &fakePurgeStore{before: make(chan time.Time, 1), err: context.Canceled}

	NewPurger(s, 24*time.Hour, slog.New(slog.NewTextHandler(logs, nil))).RunOnce(context.Background())

	if got := logs.String(); strings.Contains(got, "level=ERROR") {
		t.Errorf("關機中斷被記成 ERROR:\n%s", got)
	}
}

func TestPurgerRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakePurgeStore{before: make(chan time.Time, 1)}

	done := make(chan struct{})
	go func() {
		NewPurger(s, 24*time.Hour, discardLogger()).Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// ⚠️ 這個測試若逾時，代表 Run 卡在「等到明天 20:00」的 timer 上——
		// 也就是關機時會整整卡到寬限期結束被 SIGKILL。
		t.Fatal("Run 沒有在 context 取消後返回")
	}
}
