package outbox

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// fakeStore 讓 poller 的測試完全不必碰 MySQL 或 Kafka。
//
// ⭐ 它之所以只有一個方法，是因為介面定義在**消費端**（見 poller.go 的
// eventStore）。若介面是由 store 套件定義的「Repository 的全部方法」，
// 這個假物件就得跟著長出 Debit、Credit……而它們與投遞邏輯毫無關係。
type fakeStore struct {
	publishPending func(ctx context.Context, batchSize int, publish store.PublishFunc) (store.PublishStats, error)
}

func (f fakeStore) PublishPending(ctx context.Context, batchSize int, publish store.PublishFunc) (store.PublishStats, error) {
	return f.publishPending(ctx, batchSize, publish)
}

func testOutboxConfig() config.Outbox {
	return config.Outbox{PollInterval: time.Millisecond, BatchSize: 7, Retention: 24 * time.Hour}
}

// noopPublish 是一個什麼都不送的 PublishFunc。
func noopPublish(_ context.Context, _ []store.PendingEvent) ([]int64, error) { return nil, nil }

func TestPollerRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := fakeStore{publishPending: func(context.Context, int, store.PublishFunc) (store.PublishStats, error) {
		return store.PublishStats{}, nil
	}}

	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, testOutboxConfig(), discardLogger()).Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 沒有在 context 取消後返回——關機會卡在寬限期直到 SIGKILL")
	}
}

// ⭐ TestPollerRoundIgnoresShutdownSignal 是 context.WithoutCancel 的回歸測試。
//
// 為什麼這件事重要：一輪的後半段是「把**已經送進 Kafka** 的事件標成 SENT」。
// 收工訊號若能中斷它，那些事件會維持 PENDING、下次開機重送一次——
// at-least-once 容許重複，但這個重複是我們自己在每一次關機時製造出來的。
//
// ⚠️ 這個測試在拿掉 WithoutCancel 之後**一定會紅**：context.CancelFunc 返回前
// 就已經關掉所有子 context 的 done channel，所以 cancel() 之後再讀 roundCtx.Err()
// 必定非 nil。
func TestPollerRoundIgnoresShutdownSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var roundErr error

	s := fakeStore{publishPending: func(roundCtx context.Context, _ int, _ store.PublishFunc) (store.PublishStats, error) {
		close(entered)
		<-release // 卡在這裡，模擬「正在等 Kafka ack」
		roundErr = roundCtx.Err()
		return store.PublishStats{}, nil
	}}

	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, testOutboxConfig(), discardLogger()).Run(ctx)
		close(done)
	}()

	<-entered
	cancel()       // 等同收到 SIGTERM
	close(release) // 放行那一輪

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 沒有返回")
	}
	if roundErr != nil {
		t.Errorf("這一輪的 context 被關機訊號取消了（%v）——標記 SENT 會失敗，"+
			"已送出的事件下次開機會重送一次", roundErr)
	}
}

// TestPollerKeepsRunningAfterFailure 釘住「投遞失敗不可以讓迴圈停掉」。
//
// ⚠️ 這是最容易寫錯的地方：Go 的慣例是「錯誤往上回傳」，但這裡往上回傳
// 就是讓 goroutine 結束——服務還活著、健康檢查照過、HTTP 照收，
// 只有事件從此再也沒送出去過。典型的「每一項檢查都顯示正常」故障。
func TestPollerKeepsRunningAfterFailure(t *testing.T) {
	var calls atomic.Int64
	s := fakeStore{publishPending: func(context.Context, int, store.PublishFunc) (store.PublishStats, error) {
		calls.Add(1)
		return store.PublishStats{Fetched: 1, Failed: 1}, errors.New("kafka 掛了")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, testOutboxConfig(), discardLogger()).Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("投遞連續失敗之後 poller 停了（只跑了 %d 輪）", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// TestClassifyRound 釘住退避的**判準**：這一輪有沒有任何一則真的送出去。
//
// ⚠️ 最容易寫錯的是第三與第四格：帶著 error 但有送出去的那一輪，Kafka 是活的
// （只是某個 partition 的 leader 換屆），退避會拖慢其餘正常的事件；
// 而「撈到卻一則都沒送成」即使沒有 error 也是停滯——那代表 PublishFunc 壞了。
func TestClassifyRound(t *testing.T) {
	boom := errors.New("kafka 掛了")

	tests := []struct {
		name  string
		stats store.PublishStats
		err   error
		want  roundOutcome
	}{
		{"沒事做", store.PublishStats{}, nil, outcomeIdle},
		{"撈取本身失敗（DB 不通）", store.PublishStats{}, boom, outcomeStalled},
		{"整批送出成功", store.PublishStats{Fetched: 3, Sent: 3}, nil, outcomeProgress},
		{"部分失敗但有送出去", store.PublishStats{Fetched: 3, Sent: 2, Failed: 1}, boom, outcomeProgress},
		{"撈到了但一則都沒送成", store.PublishStats{Fetched: 3, Failed: 3}, boom, outcomeStalled},
		{"一則都沒送成且沒有 error", store.PublishStats{Fetched: 3, Failed: 3}, nil, outcomeStalled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRound(tt.stats, tt.err); got != tt.want {
				t.Errorf("classifyRound() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNextDelay 釘住退避的四條規則。
//
// ⚠️ 最後兩格是「設定的間隔大於退避上限」的情況（WALLET_OUTBOX_POLL_INTERVAL
// 是可調的）。夾錯方向的話，故障時輪詢反而變得**比設定值更密**——
// 一個為了減壓而寫的機制，結果在最需要減壓的時候加壓。
func TestNextDelay(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		current  time.Duration
		outcome  roundOutcome
		want     time.Duration
	}{
		{"沒事做 → 回到設定間隔", 200 * time.Millisecond, 3 * time.Second, outcomeIdle, 200 * time.Millisecond},
		{"有進度 → 立刻回到設定間隔", 200 * time.Millisecond, 3 * time.Second, outcomeProgress, 200 * time.Millisecond},
		{"停滯 → 加倍", 200 * time.Millisecond, 200 * time.Millisecond, outcomeStalled, 400 * time.Millisecond},
		{"停滯 → 封頂在 maxPollBackoff", 200 * time.Millisecond, 4 * time.Second, outcomeStalled, maxPollBackoff},
		{"停滯且已封頂 → 不再增加", 200 * time.Millisecond, maxPollBackoff, outcomeStalled, maxPollBackoff},
		{"設定間隔已大於上限 → 停滯時不加倍也不縮短", 30 * time.Second, 30 * time.Second, outcomeStalled, 30 * time.Second},
		{"設定間隔大於上限 → 有進度時回到設定值", 30 * time.Second, 60 * time.Second, outcomeProgress, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPoller(fakeStore{}, noopPublish,
				config.Outbox{PollInterval: tt.interval, BatchSize: 1, Retention: time.Hour}, discardLogger())
			if got := p.nextDelay(tt.current, tt.outcome); got != tt.want {
				t.Errorf("nextDelay(%v, %v) = %v, want %v", tt.current, tt.outcome, got, tt.want)
			}
		})
	}
}

// ⭐ TestPollerBacksOffWhenNothingIsDelivered 是「背景排程不可以壓垮帳務主庫」的回歸測試。
//
// 沒有退避的話，Kafka 斷線期間每一輪都是
// 「一次 SELECT ... FOR UPDATE + 一次最多 batchSize 列的 UPDATE」，
// 200ms 一輪打在**帳務主庫**上，一直打到 Kafka 回來——斷線半小時就是 9,000 輪。
// ⚠️ 而症狀只是「下注偶爾變慢」，日誌看起來完全合理（它確實每輪都失敗了）。
//
// 這個測試用 5ms 的間隔跑 400ms：沒有退避會是數十輪，有退避是個位數。
// 門檻刻意放寬到 15，量的是「有沒有退避」而不是「退避得多精準」——
// 精準的部分由 TestNextDelay 逐格釘死，這裡只證明它真的接上了迴圈。
func TestPollerBacksOffWhenNothingIsDelivered(t *testing.T) {
	var calls atomic.Int64
	s := fakeStore{publishPending: func(context.Context, int, store.PublishFunc) (store.PublishStats, error) {
		calls.Add(1)
		return store.PublishStats{Fetched: 1, Failed: 1}, errors.New("kafka 掛了")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Outbox{PollInterval: 5 * time.Millisecond, BatchSize: 7, Retention: time.Hour}
	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, cfg, discardLogger()).Run(ctx)
		close(done)
	}()

	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	got := calls.Load()
	if got > 15 {
		t.Errorf("400ms 內跑了 %d 輪，看起來沒有退避（5ms 間隔不退避約是 80 輪）——"+
			"Kafka 斷線期間這些輪次全部是打在帳務主庫上的寫入", got)
	}
	if got == 0 {
		t.Error("一輪都沒跑，這個測試沒有量到任何東西")
	}
}

// TestPollerWarnsWhenBatchIsFull 釘住唯一一個「outbox 積壓」的訊號。
//
// ⚠️ 為什麼需要它：poller 沒事做時**刻意什麼都不印**（200ms 一輪，印一行就是
// 每天 43 萬行雜訊），成功時走 Debug。於是在 INFO 層級上，「積了 50 萬列」
// 與「一切正常」長得一模一樣。撈滿一整批＝這一輪之後還有得撈＝投遞追不上寫入。
func TestPollerWarnsWhenBatchIsFull(t *testing.T) {
	tests := []struct {
		name     string
		fetched  int
		wantWarn bool
	}{
		{"撈滿整批 → 警示", 7, true},
		{"沒撈滿 → 不警示（正常運轉不該吵）", 6, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			s := fakeStore{publishPending: func(context.Context, int, store.PublishFunc) (store.PublishStats, error) {
				return store.PublishStats{Fetched: tt.fetched, Sent: tt.fetched}, nil
			}}

			p := NewPoller(s, noopPublish, testOutboxConfig(),
				slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			p.runOnce(context.Background())

			if gotWarn := strings.Contains(logs.String(), "level=WARN"); gotWarn != tt.wantWarn {
				t.Errorf("有 WARN = %v, want %v；日誌內容:\n%s", gotWarn, tt.wantWarn, logs.String())
			}
		})
	}
}

// TestPollerPassesBatchSizeAndPublish 釘住設定值真的有走到 store。
//
// ⚠️ 這條看起來瑣碎，但 batchSize 傳錯（例如傳成 0）的症狀是
// 「LIMIT 0 永遠撈不到事件」——outbox 只進不出，而服務完全正常。
func TestPollerPassesBatchSizeAndPublish(t *testing.T) {
	gotBatchSize := make(chan int, 1)
	publishCalled := make(chan struct{}, 1)

	publish := store.PublishFunc(func(context.Context, []store.PendingEvent) ([]int64, error) {
		select {
		case publishCalled <- struct{}{}:
		default:
		}
		return nil, nil
	})

	s := fakeStore{publishPending: func(ctx context.Context, batchSize int, p store.PublishFunc) (store.PublishStats, error) {
		select {
		case gotBatchSize <- batchSize:
		default:
		}
		// store 會在交易裡呼叫 publish；這裡照做，證明傳進來的就是我們給的那一個。
		_, _ = p(ctx, nil)
		return store.PublishStats{}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewPoller(s, publish, testOutboxConfig(), discardLogger()).Run(ctx)

	select {
	case got := <-gotBatchSize:
		if got != testOutboxConfig().BatchSize {
			t.Errorf("batchSize = %d, want %d", got, testOutboxConfig().BatchSize)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poller 沒有呼叫 PublishPending")
	}
	select {
	case <-publishCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("poller 傳給 store 的不是我們給的 PublishFunc")
	}
}
