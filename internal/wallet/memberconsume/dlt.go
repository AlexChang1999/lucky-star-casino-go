package memberconsume

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// dltTopic 是 member.registered 重試耗盡後訊息送去的地方。
//
// ⚠️ 對齊 Java 的 DeadLetterPublishingRecoverer：它是「泛用地在失敗的 topic
// 後面加上 .DLT」，不是每個 topic 各自寫死一個字串，但 wallet 這裡只消費
// member.registered 這一個 topic，直接寫死比引入一個「泛用 DLT 命名函式」
// 划算——那個抽象要等第二個 consumer 出現才有意義（CLAUDE.md §2）。
//
// ⚠️ 已知並刻意保留的落差（AGENTS.md 地雷 #40）：Java 的 DeadLetterListener
// 對 wallet.credit.DLT / wallet.debit.DLT / wallet.credit.request.DLT 都有
// 對應 listener，唯獨沒有 member.registered.DLT 的——訊息送進去之後沒有
// 任何東西再讀它。Go 版照抄這個行為，不建一個沒人查的 DLT reader，那是
// 這個切片之外的過度設計；但這件事要能在告警/查詢工具做出來之前一直被看見。
const dltTopic = "member.registered.DLT"

// newDLTWriter 組出 member.registered 專用的 DLT writer。
//
// 跟 internal/wallet/outbox.NewWriter 是同一個模式（顯式設定每一個會咬人的
// 預設值），但簡化成單則發送：這裡不是批次寫入，一次只送一則失敗訊息，
// 所以 BatchSize 給 1（地雷 #21——低頻單則寫入不設 1 會被預設的 1 秒
// BatchTimeout 卡住）。
func newDLTWriter(brokers []string, logger *slog.Logger) *kafka.Writer {
	if logger == nil {
		logger = slog.Default()
	}
	return &kafka.Writer{
		Addr:  kafka.TCP(brokers...),
		Topic: dltTopic, // 固定 topic：這個 writer 只送 DLT，不像 outbox 那樣按列切換

		// Murmur2：與 outbox writer 同一個理由（地雷 #38），DLT 沒有下游會按
		// partition 消費，但保持一致比「這裡為什麼不一樣」更省後續的疑惑。
		Balancer: &kafka.Murmur2Balancer{},

		RequiredAcks: kafka.RequireAll,

		// ⭐ Async 必須是 false：改成 true 會讓 WriteMessages 立刻回傳 nil，
		// 送不出去的 DLT 訊息就無聲蒸發（地雷 #38）——而 DLT 訊息本來就是
		// 系統唯一的失敗痕跡，蒸發掉的話連「有人失敗過」都不知道。
		Async: false,

		BatchSize:    1,
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: 5 * time.Second,
		MaxAttempts:  3,

		AllowAutoTopicCreation: true,

		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
			logger.Error("member.registered DLT writer", "msg", fmt.Sprintf(msg, args...))
		}),
	}
}

// dltWriter 包一層讓 Handler 好測（假物件只要實作這個小介面）。
type dltWriter struct {
	writer *kafka.Writer
	logger *slog.Logger
}

// newDLT 組裝 dltWriter。
func newDLT(brokers []string, logger *slog.Logger) *dltWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &dltWriter{writer: newDLTWriter(brokers, logger), logger: logger}
}

// publish 把原始訊息位元組原封不動送進 DLT topic。
//
// ⚠️ 跟 outbox 的 messagesFor 是同一個判準：Payload 不重新序列化，避免欄位
// 順序/空白被 Go 的 encoding/json 洗過一輪，讓事後排查時看到的 DLT 內容
// 跟原始訊息逐位元組相同。
func (d *dltWriter) publish(ctx context.Context, key, value []byte) error {
	if err := d.writer.WriteMessages(ctx, kafka.Message{Key: key, Value: value}); err != nil {
		return fmt.Errorf("送進 %s 失敗: %w", dltTopic, err)
	}
	return nil
}

// close 關閉底層 writer，順序上要在 Consumer.Close 之後（跟 outbox.Publisher.Close
// 的先後理由一致：writer 先關的話，還沒送完的最後一筆會被誤判成失敗）。
func (d *dltWriter) close() error {
	if err := d.writer.Close(); err != nil {
		return fmt.Errorf("關閉 member.registered DLT writer 失敗: %w", err)
	}
	return nil
}
