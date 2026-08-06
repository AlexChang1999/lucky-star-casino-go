package kafka

import (
	"context"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	// handleTimeout 是**單則**訊息的處理預算。
	//
	// ⚠️ 它的用途與 Java 的 `max.poll.interval.ms` **不一樣**，這個差異值得記住：
	// Spring Kafka 是「poll 迴圈同時負責 heartbeat」，所以處理太久會被 coordinator
	// 判定為死亡而踢出 group；kafka-go 的 heartbeat 跑在**獨立的 goroutine**，
	// handler 慢不會讓 member 掉出 group。
	// 也就是說這裡的逾時不是為了保住 group 成員資格，而是為了不讓**一則**訊息
	// 無限期卡住這個 consumer 的進度——沒有它的話，一個忘了設逾時的 DB 查詢
	// 就能讓整個 partition 停在原地，而 group 看起來完全健康。
	handleTimeout = 10 * time.Second

	// commitTimeout 是 commit offset 的上限。
	//
	// 短是刻意的：commit 失敗的代價只是「下次重播這一則」，而消費端本來就必須
	// 冪等（at-least-once，地雷 #6）。拿收工時間去換一次 commit 不划算。
	commitTimeout = 3 * time.Second

	// fetchBackoffMin / fetchBackoffMax 是 fetch 連續失敗時的退避區間。
	//
	// ⚠️ 沒有退避的話，Kafka 不通時這個迴圈會變成 **tight loop**：
	// FetchMessage 立刻回錯、立刻再試，把一顆 CPU 燒滿並且每秒印上千行日誌。
	// 與 outbox poller 的退避（maxPollBackoff）是同一個判斷，
	// 差別在那邊保護的是資料庫、這邊保護的是 CPU 與日誌。
	fetchBackoffMin = 200 * time.Millisecond
	fetchBackoffMax = 5 * time.Second
)

// Handler 處理一則訊息。回傳 error 代表這一則沒處理成功。
//
// ⚠️ **回傳 error 不會讓訊息被重送**——offset 一樣會 commit（見 Consumer.Run
// 的說明與地雷 #20）。要重試的話，重試邏輯必須寫在 handler 自己裡面。
type Handler func(ctx context.Context, msg kafka.Message) error

// messageReader 是消費迴圈需要的能力。
//
// ⭐ 介面定義在**消費端**（CLAUDE.md §2）：`*kafka.Reader` 完全不知道它存在，
// 只是剛好有這三個方法。好處很具體——測試的假物件只要實作三個方法，
// 不必模擬 kafka.Reader 那二十幾個。
type messageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Consumer 是「一個 topic、一個 handler」的消費迴圈。
//
// 對齊 Java 的一個 `@KafkaListener` 方法。差別在 Go 這邊「它在哪個 goroutine 上跑、
// 由誰啟動、什麼時候停」在 main.go 裡一眼看得完，而 Spring 那邊要知道
// `ConcurrentMessageListenerContainer` 的行為才答得出來。
type Consumer struct {
	reader messageReader
	handle Handler
	name   string
	logger *slog.Logger
}

// NewConsumer 組出消費迴圈。name 只用於日誌，取「服務-用途」的形式（read-sync）。
func NewConsumer(reader messageReader, name string, handle Handler, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{reader: reader, handle: handle, name: name, logger: logger}
}

// Run 消費到 ctx 被取消為止。它會阻塞，呼叫端自己開 goroutine。
//
// ⭐ 這個迴圈最重要的一條規矩：**不論 handler 成功或失敗，都要 commit offset**。
//
// 對齊 Java 的 `finally { ack.acknowledge(); }`。不 commit 的話 offset 會卡在
// 那一則壞訊息上，而 kafka-go 下次 fetch 拿到的還是它——**一則壞訊息就能讓
// 整個 topic 停擺**，而從外面看只是「某個功能突然沒了」：group 還在、lag 在漲、
// 服務健康檢查全過。
//
// ⚠️ 這與「要不要重試」是兩件事（地雷 #20）。commit 的語義是「我處理過了」，
// 不是「我處理成功了」。
//
// ⚠️ **已知與 Java 版的分歧**：Spring 那邊處理失敗會由 `DefaultErrorHandler` +
// `DeadLetterPublishingRecoverer` 把訊息送進 DLT（團隊有 5 個 DLT topic），
// 這裡目前只記一行 ERROR 就往下走——也就是**失敗的訊息會被靜靜丟掉**。
// 這是刻意的暫時狀態（第一個真的 consumer 還沒接上，DLT 的 topic 命名與
// payload 形狀要照 Java 版查證），但它必須在接第一個 consumer 時補上，
// 否則就是無聲的資料遺失。
func (c *Consumer) Run(ctx context.Context) error {
	c.logger.Info("kafka consumer 啟動", "name", c.name)
	defer c.logger.Info("kafka consumer 停止", "name", c.name)

	backoff := fetchBackoffMin
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			// ⚠️ 判斷收工要看 **ctx**，不是看 error 的型別。kafka-go 在 ctx 取消時
			// 回的可能是 context.Canceled，也可能是底層連線被關掉產生的 io 錯誤
			// （取消會連帶關閉 fetch 中的連線）。用 errors.Is 判斷會讓後者被當成
			// 真正的故障，於是每次關機都印一行看起來很嚇人的 ERROR。
			if ctx.Err() != nil {
				return nil
			}
			c.logger.Error("kafka fetch 失敗，退避後重試",
				"name", c.name, "backoff", backoff.String(), "err", err)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextFetchBackoff(backoff)
			continue
		}
		// 拿到訊息就代表連線是活的，退避立刻歸零——這裡量的不是負載，
		// 是「Kafka 通不通」，它通了就是通了（同 poller.nextDelay）。
		backoff = fetchBackoffMin

		c.handleOne(ctx, msg)
	}
}

// handleOne 處理一則訊息並 commit 它的 offset。
//
// 拆成獨立方法是為了讓那兩個 context 的差異看得清楚：**處理**吃收工訊號、
// **commit** 不吃。
func (c *Consumer) handleOne(ctx context.Context, msg kafka.Message) {
	handleCtx, cancelHandle := context.WithTimeout(ctx, handleTimeout)
	err := c.handle(handleCtx, msg)
	cancelHandle()

	if err != nil {
		// ⚠️ 這一行是失敗訊息**唯一**的痕跡（DLT 尚未實作，見 Run 的說明），
		// 所以要帶齊足以定位那一則的座標：topic / partition / offset。
		// 少了 offset 的話，事後只知道「有一則失敗了」而找不回是哪一則。
		c.logger.Error("kafka 訊息處理失敗，仍會 commit offset（對齊 Java 的 finally ack）",
			"name", c.name,
			"topic", msg.Topic,
			"partition", msg.Partition,
			"offset", msg.Offset,
			"key", string(msg.Key),
			"err", err,
		)
	}

	// ⚠️ context.WithoutCancel：收工時 ctx **已經被取消了**，直接傳下去會讓
	// 最後一則的 commit 立刻失敗，下次開機重播（地雷 #20）。
	// 上限由 commitTimeout 夾住，所以「不吃取消」不等於「可以拖到寬限期結束」——
	// 與 outbox poller 的 runOnce、HTTP 的 Shutdown 是同一個判斷。
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancelCommit()

	if cerr := c.reader.CommitMessages(commitCtx, msg); cerr != nil {
		// commit 失敗不是致命的：這一則會被重播，而消費端本來就必須冪等。
		// 但它一定要看得見——連續出現代表 coordinator 有問題，
		// 而症狀會是「同一批訊息一直被重複處理」。
		c.logger.Error("kafka commit offset 失敗，這一則會被重播",
			"name", c.name,
			"topic", msg.Topic,
			"partition", msg.Partition,
			"offset", msg.Offset,
			"err", cerr,
		)
	}
}

// Close 關掉 reader。
//
// ⚠️ 必須在 Run 返回**之後**才呼叫：reader 關掉之後的 FetchMessage 會回
// io.ErrClosedPipe，那會被上面的迴圈當成故障而印一行 ERROR 再退避。
// （同 outbox 的 Publisher.Close 與 poller 的先後順序。）
//
// ⭐ 它同時是「優雅地離開 consumer group」的唯一時機：Close 會送 LeaveGroup，
// coordinator 立刻重新分配 partition。被 SIGKILL 的話這一步不會發生，
// 幽靈 member 要等 SessionTimeout（30 秒）才被踢掉（地雷 #25）。
func (c *Consumer) Close() error {
	if err := c.reader.Close(); err != nil {
		return err
	}
	return nil
}

// nextFetchBackoff 把退避加倍並夾在上限。
//
// 拆成純函式是為了測得動：寫在迴圈裡就只能靠「等它退避完」來驗證，
// 而那種測試又慢又 flaky（同 poller.nextDelay 的理由）。
func nextFetchBackoff(current time.Duration) time.Duration {
	return min(current*2, fetchBackoffMax)
}

// sleepCtx 等待 d，中途 ctx 被取消就提早返回 false。
//
// ⚠️ 不要用 time.Sleep：關機訊號來的時候它會把整個收工流程壓在那裡等滿，
// 而退避到 5 秒時那就是半個寬限期。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
