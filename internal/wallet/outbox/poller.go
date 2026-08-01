package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// roundTimeout 是**單輪**投遞的總預算（撈取 + 送出 + 標記）。
//
// ⚠️ 它必須小於 `docker stop` 的 10 秒寬限期，因為收工時我們會等當前這一輪
// 跑完（見 runOnce 對 context.WithoutCancel 的說明）。5 秒的 writerWriteTimeout
// 加上兩三句 SQL，8 秒是有餘裕的上限而不是預期值。
const roundTimeout = 8 * time.Second

// eventStore 是 poller 需要的儲存能力。
//
// ⭐ 介面定義在**消費端**（CLAUDE.md §2）：`*store.Repository` 完全不知道
// 這個介面存在，它只是剛好有這個方法。這與 Java 的
// 「先寫 WalletOutboxRepository 介面、再寫 Impl」是相反的方向——
// 好處很具體：這裡只需要一個方法，測試的假物件就只要一個方法，
// 而 store.Repository 之後長到二十個方法也不會影響這裡。
type eventStore interface {
	PublishPending(ctx context.Context, batchSize int, publish store.PublishFunc) (store.PublishStats, error)
}

// Poller 定時把 wallet_outbox 的 PENDING 事件投遞出去。
//
// 對齊 Java 的 `WalletOutboxPoller`（`@Scheduled(fixedDelayString=...)`），
// 差別只在 Go 沒有 scheduler 容器——一個 goroutine 加一個 timer 就是全部，
// 而且「它在哪個執行緒上跑、由誰啟動、什麼時候停」在 main.go 裡一眼看得完。
type Poller struct {
	store     eventStore
	publish   store.PublishFunc
	interval  time.Duration
	batchSize int
	logger    *slog.Logger
}

// NewPoller 組出投遞器。
//
// ⚠️ publish 是一個**函式**而不是 *Publisher：poller 的職責是「多久跑一次、
// 一次撈多少、錯了怎麼記」，它完全不需要知道下游是 Kafka。
// 這也讓測試不必碰 Kafka——傳一個回傳固定結果的函式進來就好。
func NewPoller(s eventStore, publish store.PublishFunc, cfg config.Outbox, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		store:     s,
		publish:   publish,
		interval:  cfg.PollInterval,
		batchSize: cfg.BatchSize,
		logger:    logger,
	}
}

// Run 跑投遞迴圈直到 ctx 被取消。它會阻塞，呼叫端自己開 goroutine。
//
// ⚠️ 這是 **fixed delay**（上一輪跑完才起算下一輪）而不是 fixed rate，
// 對齊 Java 的 `fixedDelayString`。差別在負載高的時候：fixed rate 會在
// 「一輪跑了 300ms、間隔只有 200ms」時讓下一輪立刻開始，於是 poller 開始
// 追著自己跑、把 DB 連線吃光；fixed delay 則自然退讓。
// ⚠️ 也因此**不要**改用 time.Ticker：Ticker 是 fixed rate，而且它在錯過幾拍
// 之後只補送一次，看起來像「有在跑」但實際節奏已經跟設定值無關了。
func (p *Poller) Run(ctx context.Context) {
	p.logger.Info("outbox 投遞器啟動", "interval", p.interval.String(), "batchSize", p.batchSize)

	timer := time.NewTimer(p.interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("outbox 投遞器停止")
			return
		case <-timer.C:
		}

		p.runOnce(ctx)
		timer.Reset(p.interval)
	}
}

// runOnce 跑一輪投遞。錯誤只記錄不往外拋——投遞器停下來比慢一點嚴重得多。
func (p *Poller) runOnce(ctx context.Context) {
	// ⚠️ context.WithoutCancel：這一輪**刻意不吃收工訊號**。
	//
	// 理由是這輪的後半段是「把已經送進 Kafka 的事件標成 SENT」。取消它不會
	// 丟資料（下次開機會重送，at-least-once 本來就容許），但那是我們自己
	// 製造出來的重複投遞，而且每次關機都來一次。
	// 上限由 roundTimeout 夾住，所以「不吃取消」不等於「可以拖很久」——
	// 這與收工時 Kafka commit offset 要用 WithoutCancel 是同一個判斷（地雷 #20）。
	roundCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), roundTimeout)
	defer cancel()

	stats, err := p.store.PublishPending(roundCtx, p.batchSize, p.publish)
	switch {
	case err != nil:
		// ⚠️ 這裡**不區分**「DB 失敗」與「部分投遞失敗」，因為兩者的處置相同：
		// 記下來、等下一輪。真正的差別（交易有沒有提交）已經反映在 stats 上。
		p.logger.Error("outbox 投遞失敗",
			"fetched", stats.Fetched,
			"sent", stats.Sent,
			"failed", stats.Failed,
			"err", err,
		)
	case stats.Fetched == 0:
		// ⚠️ 沒事做的時候**什麼都不印**。200ms 一輪，印一行就是每天 43 萬行
		// 「沒有待送事件」，而真正的錯誤會被埋在裡面。
	default:
		// 成功路徑走 Debug：正常運轉時這是每秒五行的雜訊，
		// 但查「事件到底有沒有送出去」時它又是唯一有用的東西。
		p.logger.Debug("outbox 投遞完成", "fetched", stats.Fetched, "sent", stats.Sent)
	}
}
