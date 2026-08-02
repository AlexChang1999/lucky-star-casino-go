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

// ⭐ maxPollBackoff 是「連續整輪失敗」時輪詢間隔的上限。
//
// **為什麼需要退避（Java 版沒有這個東西）**：投遞整批失敗最常見的原因是
// Kafka 整個不通。此時每一輪都會做一次
// `SELECT ... FOR UPDATE` + 一次最多 batchSize 列的 `UPDATE retry_count + 1`，
// 而預設是 200ms 一輪——也就是**每秒五次、每次最多 500 列的寫入，打在帳務主庫上**，
// 一直打到 Kafka 回來為止。Kafka 斷線半小時，那是 9,000 輪。
// ⚠️ 症狀沒有錯誤訊息可以指認：帳務交易只是「偶爾變慢」，而 poller 的日誌
// 看起來完全合理（它確實每一輪都失敗了）。這與地雷 #37 同一類——
// **背景排程在帳務主庫上加壓**，只是這次的原因是重試而不是鎖。
//
// **為什麼上限只有 5 秒**：退避越長，Kafka 復原後第一則事件的延遲就越長，
// 而那是對外可觀測的行為（Java 版永遠是 200ms）。5 秒把寫入放大降到 1/25，
// 同時讓復原延遲維持在「一次下注的等待都不到」的量級。
//
// ⚠️ **部分失敗不退避**：一批事件打散到多個 partition，其中一個 leader 換屆
// 只會讓落在它身上的那幾則失敗——Kafka 是活的，退避只會拖慢其他事件。
// 判準寫在 classifyRound：**這一輪有沒有送出去任何一則**。
const maxPollBackoff = 5 * time.Second

// roundOutcome 是一輪投遞的結果，它唯一的用途是決定「下一輪要等多久」。
//
// ⚠️ 刻意不用 `error` 或 bool 表達：這裡要區分的是**三種**狀態，
// 而「沒事做」與「做了但全失敗」在 bool 裡會被壓成同一格——
// 那正好是退避判斷最不能搞混的兩格（閒置時退避＝新事件被莫名其妙延遲）。
type roundOutcome int

const (
	outcomeIdle     roundOutcome = iota // 沒有待送事件（常態）
	outcomeProgress                     // 至少送出一則
	outcomeStalled                      // 撈到了卻一則都沒送出，或撈取本身就失敗
)

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

	delay := p.interval
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("outbox 投遞器停止")
			return
		case <-timer.C:
		}

		next := p.nextDelay(delay, p.runOnce(ctx))
		// ⚠️ 只在「間隔真的變了」時記一行。連續失敗一小時的話，這裡總共只印
		// 五行（200ms→400→800→1.6s→3.2s→5s 封頂），而不是每輪一行。
		// 退避本身要看得見，但看得見不等於要用同一則訊息把日誌淹掉。
		if next != delay {
			p.logger.Warn("outbox 投遞間隔改變", "from", delay.String(), "to", next.String())
		}
		delay = next
		timer.Reset(delay)
	}
}

// nextDelay 依這一輪的結果決定下一輪要等多久。
//
// 拆成純函式是為了測得動：時間相關的邏輯寫在迴圈裡就只能靠等待來驗證，
// 而「等到它退避完」的測試又慢又 flaky（同 purge.go 的 nextPurgeAt）。
func (p *Poller) nextDelay(current time.Duration, outcome roundOutcome) time.Duration {
	if outcome != outcomeStalled {
		// ⚠️ 任何一則送出去就**立刻**回到設定的間隔，不是慢慢遞減。
		// 理由是這裡量的不是負載而是「Kafka 通不通」——它通了就是通了。
		return p.interval
	}

	// ⚠️ 上限取 max(maxPollBackoff, p.interval)，兩個方向都要顧到：
	//   - 設定間隔 < 5s（預設 200ms）→ 一路加倍到 5s 封頂
	//   - 設定間隔 ≥ 5s → 上限就是設定值本身，等於**不退避**。那是對的：
	//     30 秒一輪本來就只有每分鐘兩次寫入，沒有壓力要減；而夾到 5 秒
	//     會變成**故障時輪詢反而比平常更密**，完全違反這個機制的目的。
	limit := max(maxPollBackoff, p.interval)
	return min(current*2, limit)
}

// runOnce 跑一輪投遞，回傳這一輪的結果供 Run 決定下一輪的間隔。
//
// 錯誤只記錄不往外拋——投遞器停下來比慢一點嚴重得多。
func (p *Poller) runOnce(ctx context.Context) roundOutcome {
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
	if err != nil {
		// ⚠️ 這裡**不區分**「DB 失敗」與「部分投遞失敗」，因為兩者的處置相同：
		// 記下來、等下一輪。真正的差別（交易有沒有提交）已經反映在 stats 上。
		p.logger.Error("outbox 投遞失敗",
			"fetched", stats.Fetched,
			"sent", stats.Sent,
			"failed", stats.Failed,
			"err", err,
		)
	}

	switch outcome := classifyRound(stats, err); outcome {
	case outcomeIdle:
		// ⚠️ 沒事做的時候**什麼都不印**。200ms 一輪，印一行就是每天 43 萬行
		// 「沒有待送事件」，而真正的錯誤會被埋在裡面。
		return outcome

	case outcomeStalled:
		// 失敗那一行已經在上面印過了（err != nil）。err == nil 卻走到這裡代表
		// publish 回報「一則都沒送成、但也沒有錯誤」——那是 PublishFunc 的實作
		// 有問題，不印的話它會完全隱形。
		if err == nil {
			p.logger.Error("outbox 撈到事件卻一則都沒送出，且沒有回報錯誤",
				"fetched", stats.Fetched)
		}
		return outcome

	default:
		// ⭐ 撈滿一整批＝**這一輪之後還有得撈**，也就是投遞追不上寫入。
		// 這是目前唯一能看到「outbox 積壓」的訊號：poller 沒事做時刻意不印，
		// 於是「積了 50 萬列」與「一切正常」在 INFO 層級長得一模一樣。
		// ⚠️ 真正的解法是 Phase H 的積壓 gauge（Java 有 WalletOutboxMetrics），
		// 但那要等指標管線；在那之前這一行是零成本的替代品。
		if stats.Fetched >= p.batchSize {
			p.logger.Warn("outbox 撈滿整批，投遞可能追不上寫入",
				"fetched", stats.Fetched, "sent", stats.Sent, "batchSize", p.batchSize)
			return outcome
		}
		// 成功路徑走 Debug：正常運轉時這是每秒五行的雜訊，
		// 但查「事件到底有沒有送出去」時它又是唯一有用的東西。
		p.logger.Debug("outbox 投遞完成", "fetched", stats.Fetched, "sent", stats.Sent)
		return outcome
	}
}

// classifyRound 把一輪的結果分成三格。
//
// ⚠️ 判準是「**這一輪有沒有任何一則真的送出去**」，不是「有沒有 error」：
// 部分失敗（一批打散到多個 partition，其中一個掛了）帶著 error 但 Kafka 是活的，
// 那時退避只會拖慢其餘正常的事件。
func classifyRound(stats store.PublishStats, err error) roundOutcome {
	switch {
	case stats.Fetched == 0:
		// 撈不到東西有兩種：真的沒事做（常態），或撈取語句本身就失敗
		// （DB 不通）。後者一樣要退避——對一個連不上的資料庫每秒重試五次
		// 不會讓它早一點回來。
		if err != nil {
			return outcomeStalled
		}
		return outcomeIdle
	case stats.Sent == 0:
		return outcomeStalled
	default:
		return outcomeProgress
	}
}
