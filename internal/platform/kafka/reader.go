// Package kafka 是**消費端**的共用基礎設施：把 kafka-go 的 Reader 設定與
// 消費迴圈收成一份，讓 wallet / rank / admin / member 的每一個 consumer
// 都踩在同一組已經驗證過的預設值上。
//
// ⚠️ 為什麼投遞側（outbox 的 Writer）**沒有**一起搬進來：那是 wallet 專屬的
// Transactional Outbox 的一部分，它的設定與 outbox 的批次大小、輪詢間隔綁在一起
// （見 internal/wallet/outbox/publisher.go 的 BatchSize 註解）。
// 消費端則相反——每個服務都要一份、而且每一份的正確性判準完全相同。
// **有第二個實作才抽象**（CLAUDE.md §2），消費端已經確定會有四個。
//
// 這一整包最重要的兩句話：
//
//	① consumer 比 topic 早啟動時，kafka-go 會讓它永遠收不到訊息（地雷 #19）
//	② 處理成功或失敗都要 commit offset，否則一則壞訊息就能讓整個 topic 停擺（地雷 #20）
//
// 兩者的共同點是**每一項檢查都顯示正常**：健康檢查過、日誌無錯誤、group 也在，
// 只是訊息不會動。
package kafka

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	// ⭐ partitionWatchInterval 與 WatchPartitionChanges 成對，是地雷 #19 的處方。
	//
	// kafka-go 的 `WatchPartitionChanges` **預設是 false**。加入 group 的那一刻
	// topic 還不存在（本專案的 topic 是 producer 第一次投遞時自動建立的），
	// 這個 member 就被分配到 **0 個 partition**——而且之後**沒有任何事件會觸發
	// rebalance**，它會一直是 0 個，直到重啟。
	//
	// ⚠️ 症狀最惡劣的地方是每一項檢查都顯示正常：healthcheck 過、日誌無錯誤、
	// `kafka-consumer-groups --list` 看得到這個 group——但 `--describe` 是空的。
	// （Spring Kafka 沒這問題：它靠 `metadata.max.age.ms` 每 5 分鐘自己好，
	// 所以 Java 版最多慢 5 分鐘，不會永遠壞掉。）
	partitionWatchInterval = 5 * time.Second

	// readerMaxWait 是「broker 沒有新訊息時，一次 fetch 最多掛多久」。
	//
	// kafka-go 預設 10 秒。調成 1 秒的理由**不是**延遲（有訊息時 broker 會立刻
	// 回應，MinBytes=1），而是**關機**：FetchMessage 是靠 ctx 取消中斷的，
	// 而 kafka-go 在收工時還要跟 coordinator 走完 LeaveGroup。把單次等待縮短，
	// 收工的最壞情況就從「10 秒 + LeaveGroup」降到「1 秒 + LeaveGroup」，
	// 穩穩落在 `docker stop` 的 10 秒寬限期內。
	//
	// ⚠️ 寬限期內沒收完會被 SIGKILL，而 SIGKILL 不會送 LeaveGroup ——
	// coordinator 要等 SessionTimeout（30 秒）才踢掉這個幽靈 member，
	// 這段期間新容器可能一個 partition 都分不到（地雷 #25）。
	readerMaxWait = time.Second

	// goGroupSuffix 是 Go 版 consumer group 的強制後綴（地雷 #22）。
	//
	// ⭐ Go 版與 Java 版並存時，用同一個 group id 會讓 Kafka 把 partition
	// **分給兩邊**——每則事件只有其中一版收到。症狀看起來像「Go 版隨機漏訊息」，
	// 但兩邊的日誌都正常，因為每一則它們真的收到的訊息都處理成功了。
	//
	// ⚠️ 這裡選擇「檢查並拒絕」而不是「自動補上後綴」：自動補的話，
	// 呼叫端傳 `wallet-read-sync-go` 會變成 `wallet-read-sync-go-go`，
	// 而那是一個**全新的 group**，會從頭重放整個 topic。
	// 規則要看得見，錯了要在啟動時就擋下來。
	goGroupSuffix = "-go"
)

// NewReader 組出一個 consumer group 的 Reader。
//
// ⚠️ 與 outbox 的 NewWriter 一樣，每一個欄位都是**顯式設定**的。
// kafka-go 的預設值是給示範程式用的，不是給生產環境用的。
func NewReader(brokers []string, topic, groupID string, logger *slog.Logger) (*kafka.Reader, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !strings.HasSuffix(groupID, goGroupSuffix) {
		return nil, fmt.Errorf(
			"consumer group %q 必須以 %q 結尾：與 Java 版共用 group id 會讓 partition 分給兩邊，"+
				"每則事件只有其中一版收到（地雷 #22）", groupID, goGroupSuffix)
	}

	return kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: groupID,

		// ⭐ 這兩行是本檔存在的最大理由，見 partitionWatchInterval 的說明。
		WatchPartitionChanges:  true,
		PartitionWatchInterval: partitionWatchInterval,

		// ⭐ CommitInterval = 0 代表**同步 commit**：CommitMessages 會等
		// broker 回應才返回。這是刻意的，對齊 Java 的 `ack.acknowledge()`
		// （AckMode.MANUAL_IMMEDIATE）。
		//
		// ⚠️ 設成非 0 會變成「背景每隔 N 秒批次 commit」，於是
		// CommitMessages 只是把 offset 丟進 channel 就回傳 nil ——
		// 關機時那些還沒送出的 commit 全部消失，下次開機重播。
		// 這與 Writer 的 `Async: true` 是同一種坑：**函式回傳 nil 不代表事情做完了**。
		CommitInterval: 0,

		// ⭐ StartOffset 只在「這個 group 還沒有已存 offset」時生效，
		// 也就是**每個 group 一輩子只用得到一次**——但那一次決定了讀模型是否完整。
		//
		// FirstOffset（earliest）是 kafka-go 的預設值，這裡明寫是因為
		// **Spring Kafka 的預設是 latest**，兩邊剛好相反。
		// 對讀模型投影而言 earliest 才是對的：從 latest 開始的話，
		// 新 group 啟動之前的所有流水永遠不會出現在讀端，而且沒有任何錯誤訊息——
		// 只有「舊資料查不到」這種會被當成前端 bug 的症狀。
		//
		// ⚠️ 也因此 group id 打錯字的代價是「整個 topic 重放一次」，
		// 而不是「收不到訊息」。冪等的消費端（地雷 #6）扛得住，非冪等的扛不住。
		StartOffset: kafka.FirstOffset,

		// MinBytes = 1：有一則就回，不要為了湊批次而等。
		// MaxBytes = 1MB：與 kafka-go 預設相同，明寫是為了讓「一次 fetch 的
		// 記憶體上限」看得見——這個值乘上 partition 數才是真正的峰值。
		MinBytes: 1,
		MaxBytes: 1e6,
		MaxWait:  readerMaxWait,

		// ⚠️ 刻意**不設** IsolationLevel（預設 ReadUncommitted）。
		// 它只影響「交易式 producer 寫入的未提交訊息看不看得到」，
		// 而本專案的 producer 是 outbox poller，沒有用 Kafka 交易
		// （kafka-go 也沒有實作）。設 ReadCommitted 不會錯，但會讓讀的人
		// 以為這裡有交易語義要顧——**沒有的東西不要假裝有**。

		// ⚠️ 只接 ErrorLogger，不接 Logger：後者會為每一次 fetch、每一輪
		// heartbeat 印一行，等於把日誌變成 Kafka 的 debug 輸出。
		ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
			logger.Error("kafka reader", "topic", topic, "group", groupID,
				"msg", fmt.Sprintf(msg, args...))
		}),
	}), nil
}
