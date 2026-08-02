// Package outbox 是 Transactional Outbox 的**投遞側**：把 wallet_outbox 裡的
// PENDING 事件送進 Kafka、標成 SENT，並定期清掉保留期外的 SENT 列。
//
// 寫入側在 internal/wallet/store（appendOutbox，與帳務異動同一筆交易）。
// 兩側刻意分在不同套件，因為它們的失敗模式完全不同：
//
//	寫入側失敗 → 帳務跟著回滾，「錢動了但事件沒了」不可能發生
//	投遞側失敗 → 事件留在表裡，下一輪重送（at-least-once）
//
// ⚠️ 這一整包最重要的一句話：**投遞側絕不可以「先標 SENT 再送」，也不可以
// 「送出去就當成功」**。兩者都會讓資料庫說「已送出」而事件其實不在 Kafka 裡，
// 而那正是 Outbox 這個模式唯一要防的事——防不住的話，整個模式只剩下成本。
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

const (
	// ⭐ writerFlushDelay 是 kafka-go 的 BatchTimeout（＝Java 的 linger.ms）。
	//
	// **預設是 1 秒**（AGENTS.md 地雷 #21），而且它是「批次沒裝滿時最多等多久」——
	// 也就是說一輪只有 3 筆事件時，producer 會**整整壓一秒**才送出去。
	// ⚠️ 這一秒**日誌與指標都看不出來**：延遲指標量的是訊息時間戳之後的事，
	// 而日誌只會看到「送出成功」。只有拿碼表量端到端才會發現。
	writerFlushDelay = 10 * time.Millisecond

	// writerWriteTimeout 是單次 produce 請求的上限。
	//
	// Java 那邊是 `ack.get(10, SECONDS)`，這裡取 5 秒是為了讓「一輪」能塞進
	// docker stop 的 10 秒寬限期（見 poller.go 的 roundTimeout）。
	// 送不出去的列會維持 PENDING、下一輪重送，所以短一點的代價只是多重試一次。
	writerWriteTimeout = 5 * time.Second

	// writerMaxAttempts 對齊 Java producer 的 `retries: 3`。
	//
	// ⚠️ kafka-go **沒有實作 idempotent producer**（沒有 PID／序號），
	// 所以這裡的重試在「送到了但 ack 掉了」時會產生重複訊息。這不是新的問題：
	// outbox 的投遞保證本來就是 at-least-once，下游本來就必須冪等（地雷 #6）。
	// 值得知道的是**順序不受影響**：kafka-go 每個 partition 只有一個 writer
	// goroutine，一次送一個 batch 並等它結束才送下一個（見 partitionWriter.writeBatches），
	// 等效於 max.in.flight.requests=1，所以重試不會把同 key 的訊息重排。
	writerMaxAttempts = 3
)

// NewWriter 組出 outbox 專用的 Kafka writer。
//
// ⚠️ 每一個欄位都是**顯式設定**的，而且每一個預設值都會出事——這是 kafka-go
// 與 Spring Kafka 最大的體感差異：Spring 的預設值是給生產環境用的，
// kafka-go 的預設值是給「示範程式」用的。
func NewWriter(brokers []string, batchSize int, logger *slog.Logger) *kafka.Writer {
	if logger == nil {
		logger = slog.Default()
	}
	return &kafka.Writer{
		Addr: kafka.TCP(brokers...),
		// ⚠️ 刻意**不設** Topic：outbox 的每一列自己帶 topic（wallet.debit /
		// wallet.credit），writer 層級寫死一個 topic 會讓其中一種事件靜靜地
		// 送錯地方。Topic 由每則 Message 各自指定。

		// ⭐ Balancer 必須是 Murmur2，不能用 kafka-go 的預設（&Hash{}，FNV-1a）。
		//
		// Java 的 DefaultPartitioner 用的是 murmur2(key) % partitions，
		// kafka-go 的預設是 FNV-1a——同一個 playerId 會被算到**不同的 partition**。
		//
		// ⚠️ 這條的理由在 2026-08-02 修正過一次，因為原本寫的情境不成立：
		// 開發期間兩版的**設定是完全錯開的**（Java 的 Kafka 在 9092、本專案在 9095，
		// 兩個獨立叢集），所以「同一則事件被兩版寫進同一個 topic」平常不會發生。
		// 真正要防的是**切換當下那個窗口**：灰度、雙寫、或切過去又回退時，
		// 兩版會有一段時間對**同一個 topic** 產訊息。那時 balancer 不同就等於
		// 同一個玩家的事件橫跨兩個 partition——而 partition 內有序是 Kafka
		// **唯一**的順序保證，跨 partition 之後下游看到的「先扣款後派彩」是隨機順序。
		// ⚠️ Kafka、producer、consumer 三邊都不會報錯，症狀只是下游偶爾算錯。
		//
		// ⚠️ 而且**光是 balancer 一致還不夠**：murmur2 之後要對 partition 數取模，
		// 所以同叢集的前提還包括「兩邊看到的 partition 數相同」。
		// 本專案的 topic 一律 6 個 partition（compose 的 KAFKA_NUM_PARTITIONS），
		// 與 Java 的高流量 topic 相同，但 Java 的低流量 topic 是 3、DLT 是 1——
		// **真的要走同叢集切換，那些 topic 要先對齊 partition 數**（見 compose 註解）。
		// 選 Murmur2 的成本是零，而發現上面那件事的成本是「下游偶爾算錯」。
		// （Consistent 保持 false：與 librdkafka 的 murmur2_random 一致，
		// nil key 走隨機而不是全部擠進同一個 partition。wallet 的 key 一律是
		// playerID，走不到那條路。）
		Balancer: &kafka.Murmur2Balancer{},

		// acks=all：所有 in-sync replica 都寫入才算送達。本機是單節點 broker，
		// 這與 acks=1 沒有實際差別，但**保證的語義**要現在就寫對——
		// 等到有第二個 broker 才想起來改，中間那段時間的事件已經丟過了。
		RequiredAcks: kafka.RequireAll,

		// ⭐ Async 必須是 false（這裡明寫是為了讓「不能改成 true」看得見）。
		//
		// Async: true 會讓 WriteMessages **立刻回傳 nil**，錯誤只送進 Completion
		// 回呼。於是 poller 會把「還沒送出去、甚至可能永遠送不出去」的列標成 SENT，
		// 而清理排程七天後把它刪掉——事件無聲蒸發，資料庫卻說已送出。
		Async: false,

		// ⭐ BatchSize 與 poller 的單輪批次大小相同，這是刻意的。
		//
		// ⚠️ 這裡與地雷 #21 的處方**方向相反**，值得想清楚：那條說「低頻單則寫入
		// 要設 BatchSize: 1」，因為單則訊息永遠裝不滿預設的 100 而被壓一秒。
		// 但 poller 是**成批**寫入，設成 1 會讓每則訊息各自成為一個 batch，
		// 而每個 partition 一次只送一個 batch 並等 ack —— 那就退化成 Java 舊版
		// 那個 O(N) 循序阻塞（團隊 T-090 壓測發現的瓶頸，100 筆要 500ms）。
		// 正解是**大 BatchSize + 小 BatchTimeout**：整輪最多一個 produce 請求，
		// 而沒裝滿時也只多等 10ms。
		BatchSize:    batchSize,
		BatchTimeout: writerFlushDelay,
		WriteTimeout: writerWriteTimeout,
		MaxAttempts:  writerMaxAttempts,

		// topic 不存在時自動建立。本專案的 topic 名稱全部來自 domain 的常數，
		// 所以「打錯字建出一個沒人消費的 topic」的風險是可控的；
		// 換來的是 compose 起來就能跑，不必額外維護一支建 topic 的腳本。
		// ⚠️ broker 那側也要開（compose 的 KAFKA_AUTO_CREATE_TOPICS_ENABLE），
		// 兩邊少一邊都會讓建立請求被拒絕。partition 數由 broker 的
		// KAFKA_NUM_PARTITIONS 決定，compose 已設成 6 以對齊 Java 版的 kafka-init.sh。
		//
		// ⚠️ **自動建立是非同步的**：對一個全新的 topic，第一次投遞必定失敗一次
		// （`Unknown Topic Or Partition`——metadata 請求觸發了建立，但那一次 produce
		// 已經來不及了），下一輪就成功。實測（2026-08-02）第一則事件延遲 484ms、
		// retry_count=1，之後穩定在 200~380ms。
		// 也就是說**啟動後第一則事件的那行 ERROR 是預期的**，不是故障——
		// 而每個 topic 一輩子只會發生一次（除非 `down -v` 砍掉 Kafka volume）。
		AllowAutoTopicCreation: true,

		// ⚠️ 只接 ErrorLogger，不接 Logger：後者會為**每一個 batch** 印一行
		// 「writing N messages to ...」，200ms 一輪的 poller 會把日誌淹掉。
		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
			logger.Error("kafka writer", "msg", fmt.Sprintf(msg, args...))
		}),
	}
}

// Publisher 把一批 outbox 事件送進 Kafka，並回報**確認送達**的那些。
type Publisher struct {
	writer *kafka.Writer
	logger *slog.Logger
}

// NewPublisher 包裝一個已組好的 writer。
func NewPublisher(writer *kafka.Writer, logger *slog.Logger) *Publisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{writer: writer, logger: logger}
}

// Publish 實作 store.PublishFunc：送出整批事件，回傳已 ack 的 id。
//
// ⭐ 部分成功是**正常情況**而不是例外：一批事件會依 key 打散到多個 partition，
// 其中一個 partition 的 leader 掛掉時，只有落在它身上的那些會失敗。
// kafka-go 的 WriteMessages 為此回傳 `kafka.WriteErrors`——一個與傳入訊息
// **索引對齊**的 error 切片（成功的位置是 nil）。
// ⚠️ 把它當成「整批失敗」處理的話，已經送達的那些會維持 PENDING 並被重送，
// 下游每次投遞失敗都會多吃一批重複事件。
func (p *Publisher) Publish(ctx context.Context, events []store.PendingEvent) ([]int64, error) {
	if len(events) == 0 {
		return nil, nil
	}

	messages := messagesFor(events)
	err := p.writer.WriteMessages(ctx, messages...)
	if err == nil {
		sent := make([]int64, len(events))
		for i, e := range events {
			sent[i] = e.ID
		}
		return sent, nil
	}

	var writeErrs kafka.WriteErrors
	if !errors.As(err, &writeErrs) {
		// 不是 per-message 的錯誤（context 取消、topic metadata 拿不到、
		// 連不上 broker）→ 整批都沒送出去。
		return nil, fmt.Errorf("投遞 %d 則 outbox 事件失敗: %w", len(events), err)
	}

	var (
		sent      []int64
		firstErr  error
		failedIDs []int64
	)
	for i, e := range events {
		// 防禦：WriteErrors 的長度應該恆等於訊息數，但長度對不上時寧可
		// 把該筆當成失敗（重送一次），也不要當成成功（那是無聲丟失）。
		if i >= len(writeErrs) || writeErrs[i] != nil {
			failedIDs = append(failedIDs, e.ID)
			if firstErr == nil && i < len(writeErrs) {
				firstErr = writeErrs[i]
			}
			continue
		}
		sent = append(sent, e.ID)
	}
	if firstErr == nil {
		firstErr = err
	}

	// ⚠️ 只印第一個錯誤與失敗的 id 清單，不是每筆一行：一批 500 筆全失敗時
	// （Kafka 整個掛掉就是這樣），逐筆印會在每一輪產生 500 行日誌。
	p.logger.Error("部分 outbox 事件投遞失敗，維持 PENDING 等下一輪重送",
		"failed", len(failedIDs),
		"sent", len(sent),
		"failedIDs", failedIDs,
		"err", firstErr,
	)
	return sent, fmt.Errorf("%d/%d 則 outbox 事件投遞失敗: %w", len(failedIDs), len(events), firstErr)
}

// Close 關閉底層 writer，把還在緩衝區裡的訊息 flush 出去。
//
// ⚠️ 必須在 poller 停下來**之後**才呼叫：writer 關掉之後的 WriteMessages
// 會回 io.ErrClosedPipe，那會讓最後一輪的事件被誤判成投遞失敗。
func (p *Publisher) Close() error {
	if err := p.writer.Close(); err != nil {
		return fmt.Errorf("關閉 Kafka writer 失敗: %w", err)
	}
	return nil
}

// messagesFor 把 outbox 的列翻譯成 Kafka 訊息。
//
// ⚠️ Payload **原封不動**轉成 bytes，不重新序列化。outbox 存的是帳務交易當下
// 產生的那一份 JSON 字串（見 store.appendOutbox），中途解開再包回去會讓欄位順序、
// 空白、數字格式全部變成「Go 的 encoding/json 說了算」，而契約測試是逐位元組比對的。
func messagesFor(events []store.PendingEvent) []kafka.Message {
	messages := make([]kafka.Message, len(events))
	for i, e := range events {
		var key []byte
		if e.KafkaKey != nil {
			// ⚠️ nil 與空 []byte 在 Kafka 不是同一件事（見 store.PendingEvent 的
			// 註解），所以這裡靠 *string 是否為 nil 來決定，而不是靠字串長度。
			key = []byte(*e.KafkaKey)
		}
		messages[i] = kafka.Message{
			Topic: e.Topic,
			Key:   key,
			Value: []byte(e.Payload),
		}
	}
	return messages
}
