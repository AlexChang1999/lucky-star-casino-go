package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// ⭐ purgeHourUTC 是每天跑清理的時刻，**20:00 UTC = 04:00 台北**。
	//
	// ⚠️ Java 寫的是 `cron = "0 0 4 * * *"`，而 Spring 的 cron 跑在**容器的本地
	// 時區**（Asia/Taipei）。本專案一律用 UTC（compose 的 --default-time-zone=+00:00、
	// DSN 的 loc=UTC），照抄那個 4 會變成**台北中午十二點**跑批次刪除——
	// 那是玩家最活躍的時段，而排在離峰正是這個排程唯一的排程理由。
	// ⚠️ 沒有任何錯誤訊息會告訴你這件事，只會看到「每天中午 DB 有個尖峰」。
	//
	// 刻意是常數而不是環境變數：本專案只有一個部署時區，
	// 加一個沒人會去改的設定項只是多一個要驗證的輸入（CLAUDE.md §2）。
	purgeHourUTC = 20

	// purgeTimeout 是單次清理的上限。分塊刪除（store.purgeChunkSize）之下，
	// 積了幾百萬列的第一次清理會跑很多塊，所以這裡給得比投遞的一輪寬鬆得多。
	purgeTimeout = 10 * time.Minute
)

// purgeStore 是清理排程需要的儲存能力（同樣定義在消費端）。
type purgeStore interface {
	PurgeSentOutbox(ctx context.Context, before time.Time) (int64, error)
}

// Purger 每天清掉保留期外、已投遞的 outbox 列。
//
// ⭐ 為什麼需要它：poller 投遞成功只把 status 標成 SENT，**從不刪除**
// （AGENTS.md 地雷 #5），而每一筆下注、派彩、贈禮都寫一列——這張表是
// 單向成長的。⚠️ 而且它**不會讓投遞變慢**：撈取走
// `idx_wallet_outbox_status_created`，status 在複合索引第一欄，掃不到 SENT 的資料。
// 所以這是純粹的**維運問題**（磁碟、備份時間），而那正是它在 Java 版一直沒被
// 發現的原因——沒有任何指標會變差，直到磁碟滿。
type Purger struct {
	store     purgeStore
	retention time.Duration
	logger    *slog.Logger
}

// NewPurger 組出清理排程。
func NewPurger(s purgeStore, retention time.Duration, logger *slog.Logger) *Purger {
	if logger == nil {
		logger = slog.Default()
	}
	return &Purger{store: s, retention: retention, logger: logger}
}

// Run 每天在 purgeHourUTC 跑一次清理，直到 ctx 被取消。它會阻塞。
//
// ⚠️ 這裡**不是**「啟動時先跑一次」。服務在滾動更新時可能一天重啟很多次，
// 開機就清理會讓「離峰時段跑」的設計失效——而 bulk DELETE 撞上帳務熱路徑
// 正是唯一要避免的事。少清一天完全無害。
func (p *Purger) Run(ctx context.Context) {
	p.logger.Info("outbox 清理排程啟動",
		"retention", p.retention.String(),
		"dailyAtUTC", purgeHourUTC,
		"nextRun", nextPurgeAt(time.Now(), purgeHourUTC).Format(time.RFC3339),
	)

	for {
		timer := time.NewTimer(time.Until(nextPurgeAt(time.Now(), purgeHourUTC)))
		select {
		case <-ctx.Done():
			timer.Stop()
			p.logger.Info("outbox 清理排程停止")
			return
		case <-timer.C:
		}
		p.RunOnce(ctx)
	}
}

// RunOnce 跑一次清理。
//
// ⚠️ 失敗只記 log **不中斷排程**，對齊 Java 的 catch：清理是純維運工作，
// 延一天做完全無害，但讓迴圈死掉的話就再也不會有人清了——
// 而這件事沒有任何指標看得出來。
//
// ⚠️ 與 poller 的一輪不同，這裡**吃** ctx 的取消訊號：清理刪到一半被中斷只是
// 「這次少刪幾塊」，下次排程接著刪；而投遞被中斷會造成重複投遞（見 poller.runOnce）。
// **同一個專案裡兩個相反的決定，理由各自成立**——這正是為什麼 WithoutCancel
// 不能當成一條「關機時一律這樣寫」的通則。
func (p *Purger) RunOnce(ctx context.Context) {
	before := time.Now().UTC().Add(-p.retention)

	purgeCtx, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()

	deleted, err := p.store.PurgeSentOutbox(purgeCtx, before)
	switch {
	case errors.Is(err, context.Canceled):
		// ⚠️ 收工時剛好在清理，不是故障。記成 Error 會讓「每次關機都有一行紅的」
		// 變成常態，然後真正的清理失敗就沒有人會注意到了。
		p.logger.Info("outbox 清理被關機中斷，下次排程接著刪", "deleted", deleted)
		return
	case err != nil:
		p.logger.Error("outbox 清理失敗，下次排程再試",
			"before", before.Format(time.RFC3339),
			"deleted", deleted,
			"err", err,
		)
		return
	}
	if deleted > 0 {
		p.logger.Info("outbox 清理完成", "deleted", deleted, "before", before.Format(time.RFC3339))
	}
}

// nextPurgeAt 回答「下一次該在什麼時候跑」。
//
// 拆成純函式是為了測得動：時間相關的邏輯若寫在迴圈裡，就只能靠等待來驗證，
// 而「等一天」的測試不會有人跑。
func nextPurgeAt(now time.Time, hourUTC int) time.Time {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), hourUTC, 0, 0, 0, time.UTC)
	if !next.After(utc) {
		// 已經過了今天的時刻（含剛好等於），排到明天。
		// ⚠️ 用 AddDate 而不是 Add(24h)：兩者在 UTC 下等價，但 AddDate 表達的是
		// 「明天的同一時刻」這個意圖，換成有日光節約的時區時也還是對的。
		next = next.AddDate(0, 0, 1)
	}
	return next
}
