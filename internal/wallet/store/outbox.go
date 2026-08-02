package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ── outbox 投遞的 SQL ───────────────────────────────────────────────────────
//
// 與帳務語句同一個處理方式（見 repository.go 檔頭）：寫成套件層級的常數，
// 讓它們集中在一處被逐字審查，而且 infra 測試直接引用同一份，不會出現
// 「測試自己複製了一份 SQL，於是改了實作測試照樣綠」。

// claimPendingOutbox 撈出一批待投遞事件，並**鎖住它們**。
//
// ⭐ `FOR UPDATE SKIP LOCKED` 是本檔最重要的一行，它同時解決兩件事：
//
//	FOR UPDATE   → 這批列在本交易提交前，別的副本看不到也改不了
//	SKIP LOCKED  → 別的副本不會**卡住等鎖**，而是直接跳過去撈下一批
//
// 少了它就是 Java 版的處境：`WalletOutboxPoller` 的 javadoc 自己寫著
// 「本實作未對撈出的列加鎖，多副本同時輪詢會重複送同一筆……production 多副本
// 部署時應改用 SELECT ... FOR UPDATE SKIP LOCKED」。它沒做是因為單副本假設，
// 而本專案 Phase H 的目標就是 K8s 多副本（藍圖 §5）。
//
// ⚠️ `ORDER BY created_at, id` 的第二個排序鍵不是裝飾：created_at 是
// DATETIME(6)，同一微秒內的兩列完全可能同時發生，少了 id 這個 tie-breaker，
// 兩輪之間的順序就是未定義的——而 outbox 存在的理由之一正是「同玩家事件有序」。
const claimPendingOutbox = `
	SELECT id, topic, kafka_key, payload
	  FROM wallet_outbox
	 WHERE status = 'PENDING'
	 ORDER BY created_at, id
	 LIMIT ?
	 FOR UPDATE SKIP LOCKED`

// markOutboxSent 把**已確認送達**的列標成 SENT。
//
// ⚠️ sent_at 用 `CURRENT_TIMESTAMP(6)`（DB 時鐘）而不是 Go 這邊的 time.Now()。
// created_at 的 DEFAULT 也是 DB 時鐘，兩個欄位必須來自同一個時鐘才可以相減；
// 而清理排程比的是 `sent_at < ?`，應用程式與 DB 差幾秒就會出現
// 「sent_at 比 created_at 還早」這種看起來像資料壞掉、其實是時鐘偏移的紀錄。
//
// ⚠️ **只標 SENT，從不刪除**（地雷 #5）。刪除是清理排程的事，見 purgeSentOutbox。
const markOutboxSent = `
	UPDATE wallet_outbox
	   SET status = 'SENT', sent_at = CURRENT_TIMESTAMP(6)
	 WHERE id IN (?)`

// bumpOutboxRetry 為投遞失敗的列累加 retry_count，**維持 PENDING**。
//
// 對齊 Java（WalletOutboxPoller:102）。這個數字沒有任何控制作用（沒有上限、
// 不會轉 DLT），它純粹是觀測用的：某一列的 retry_count 一直長，代表那筆
// payload 本身有問題，而不是 Kafka 暫時不通。
const bumpOutboxRetry = `
	UPDATE wallet_outbox
	   SET retry_count = retry_count + 1
	 WHERE id IN (?)`

// purgeSentOutbox 刪掉保留期外、**且已經送出**的列。
//
// ⚠️ 三個條件缺一不可：
//   - `status = 'SENT'` —— PENDING 無論多舊都不能刪，刪掉就是無聲丟失事件，
//     正是 Outbox 要防的那件事（地雷 #5）
//   - `sent_at IS NOT NULL` —— 對齊 Java 的同一句守衛：SENT 理論上必有 sent_at，
//     但若有人手動改過 status，`sent_at < ?` 對 NULL 會是 UNKNOWN 而不是 false，
//     這一句讓那個意圖變成程式碼裡看得見的東西
//   - `LIMIT ?` —— 見 PurgeSentOutbox 的分塊說明
const purgeSentOutbox = `
	DELETE FROM wallet_outbox
	 WHERE status = 'SENT' AND sent_at IS NOT NULL AND sent_at < ?
	 LIMIT ?`

// ⭐ outboxTxOptions：投遞交易跑在 READ COMMITTED，理由與 debit/credit **不同**，
// 而且是本專案目前唯一一個「不設會擋住帳務熱路徑」的地方（AGENTS.md 地雷 #38）。
//
// 在 MySQL 預設的 REPEATABLE READ 之下，`SELECT ... FOR UPDATE` 對
// `idx_wallet_outbox_status_created` 掃到的範圍下的是 **next-key lock**——
// 它鎖的不只是命中的列，還有列與列之間的**間隙**。而「PENDING + created_at 遞增」
// 這個範圍的右端，正是每一筆下注/派彩要 INSERT 新 outbox 列的地方。
// 於是 poller 一開始撈，帳務交易的 `INSERT INTO wallet_outbox` 就要等
// insert intention lock —— **投遞器把扣款卡住了**，而症狀只是「下注偶爾很慢」。
//
// READ COMMITTED 不對搜尋下 gap lock，只鎖真正命中的列，那些列又只有 poller
// 自己會碰，所以帳務熱路徑完全不受影響。
// ⚠️ 與 debitTxOptions / creditTxOptions 一樣**刻意分成獨立變數**：隔離級別是
// per-transaction 的決定（docs/ADR-002 決策 6），共用一個變數會讓三個不同的理由
// 被壓成一行看不出來的巧合。
var outboxTxOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// PendingEvent 是 outbox 裡一筆待投遞的事件。
//
// ⚠️ KafkaKey 是 *string 而不是 string（地雷 #33）：欄位可以是 NULL，而
// 「NULL 的 key」與「空字串的 key」在 Kafka 是**兩件不同的事**——前者走
// partition 隨機分配，後者是一個確定的雜湊值，於是所有空 key 的訊息會擠在
// 同一個 partition。wallet 目前一律寫 playerID，但這個型別讓那個假設不必成立。
type PendingEvent struct {
	ID       int64   `gorm:"column:id"`
	Topic    string  `gorm:"column:topic"`
	KafkaKey *string `gorm:"column:kafka_key"`
	Payload  string  `gorm:"column:payload"`
}

// PublishFunc 由呼叫端（outbox.Poller）提供：把整批事件送出去，
// 回傳**已確認送達**的那些 id。
//
// ⚠️ 契約有兩條，兩條都不可以妥協：
//   - 回傳的 id 一律代表「broker 已經 ack」。猜測、樂觀假設、或 async 送出
//     就回報成功，等於把「事件遺失」變成「資料庫說已送出」——那比沒有 outbox 更糟，
//     因為它連查都查不出來。
//   - 回傳的 error 是**部分失敗的診斷**，不是「整批失敗」。有 error 的同時
//     仍然可以有成功的 id，兩者要分別處理。
type PublishFunc func(ctx context.Context, events []PendingEvent) ([]int64, error)

// PublishStats 是一輪投遞的結果，供呼叫端記錄與觀測。
type PublishStats struct {
	Fetched int // 這一輪撈到幾筆
	Sent    int // 其中幾筆確認送達並標成 SENT
	Failed  int // 其餘幾筆維持 PENDING 並累加 retry_count
}

// PublishPending 跑完一輪 outbox 投遞：撈一批 PENDING → 交給 publish 送出 →
// 依結果標 SENT / 累加 retry_count，全部在**同一筆交易**裡。
//
// ⚠️ publish 是在**資料庫交易還開著的時候**被呼叫的，也就是說有一段網路 I/O
// 被包在交易裡。這在一般情況下是壞味道，但這裡是**必要的**：那批列的行鎖必須
// 撐到「已經確認送達並標好 SENT」為止，否則另一個副本會撈到同一批重送。
// 代價由兩件事夾住——① SKIP LOCKED 讓別人不必等鎖，② writer 的 WriteTimeout
// 與呼叫端的單輪逾時讓這段 I/O 有上限（見 outbox.Poller）。
//
// ⚠️ 回傳的 error 分兩種，呼叫端要分得出來：
//   - 第二個回傳值非 nil 但 stats 有值 → 是 publish 的部分失敗，交易**已經提交**
//     （成功的那些必須被記錄下來，否則下一輪會重送）
//   - stats 為零值 → 是資料庫層失敗，交易已回滾，什麼都沒發生
func (r *Repository) PublishPending(ctx context.Context, batchSize int, publish PublishFunc) (PublishStats, error) {
	var stats PublishStats
	var publishErr error

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var events []PendingEvent
		if err := tx.Raw(claimPendingOutbox, batchSize).Scan(&events).Error; err != nil {
			return fmt.Errorf("撈取待投遞事件失敗: %w", err)
		}
		if len(events) == 0 {
			return nil
		}
		stats.Fetched = len(events)

		// publish 的錯誤**不往上回傳**——回傳它會讓 GORM 回滾整筆交易，
		// 於是「已經送進 Kafka 的那些」不會被標 SENT，下一輪原封不動重送一次。
		// at-least-once 容許重送，但這裡的重送是我們自己製造的，沒有必要。
		sent, err := publish(ctx, events)
		publishErr = err

		sentIDs, failedIDs, err := splitByOutcome(events, sent)
		if err != nil {
			// publish 回了不屬於這一批的 id ＝ 呼叫端有 bug。這種情況下
			// 「標 SENT」會標到別人的列上，寧可整筆回滾讓錯誤浮上來。
			return err
		}

		if len(sentIDs) > 0 {
			res := tx.Exec(markOutboxSent, sentIDs)
			if res.Error != nil {
				return fmt.Errorf("標記 outbox 已送出失敗: %w", res.Error)
			}
			if res.RowsAffected != int64(len(sentIDs)) {
				// 這批列的行鎖在我們手上，沒有人能改它們。對不上代表 SQL 被改壞了，
				// 而「以為標了其實沒標」的後果是無限重送同一批事件。
				return fmt.Errorf("%w: 標記 SENT 影響了 %d 列，預期 %d 列",
					ErrUnexpectedRowsAffected, res.RowsAffected, len(sentIDs))
			}
		}
		if len(failedIDs) > 0 {
			if err := tx.Exec(bumpOutboxRetry, failedIDs).Error; err != nil {
				return fmt.Errorf("累加 outbox retry_count 失敗: %w", err)
			}
		}

		stats.Sent, stats.Failed = len(sentIDs), len(failedIDs)
		return nil
	}, outboxTxOptions)
	if err != nil {
		return PublishStats{}, err
	}
	return stats, publishErr
}

// splitByOutcome 把這一批事件分成「已送達」與「要重試」兩組。
//
// 順序沿用撈出來的順序（created_at, id），讓日誌與測試的輸出是可預期的。
func splitByOutcome(events []PendingEvent, sent []int64) (sentIDs, failedIDs []int64, err error) {
	sentSet := make(map[int64]bool, len(sent))
	for _, id := range sent {
		sentSet[id] = true
	}

	fetched := make(map[int64]bool, len(events))
	for _, e := range events {
		fetched[e.ID] = true
	}
	for _, id := range sent {
		if !fetched[id] {
			return nil, nil, fmt.Errorf("publish 回報 id=%d 已送達，但它不在這一批撈出的事件裡", id)
		}
	}

	for _, e := range events {
		if sentSet[e.ID] {
			sentIDs = append(sentIDs, e.ID)
			continue
		}
		failedIDs = append(failedIDs, e.ID)
	}
	return sentIDs, failedIDs, nil
}

// purgeChunkSize 是清理排程單次 DELETE 的上限。
//
// ⚠️ 為什麼要分塊，而 Java 版是一句無界的 bulk DELETE：outbox 每筆下注/派彩
// 都寫一列，壓測跑滿一週之後保留期外可能是**幾百萬列**。一句刪光的話，那筆交易
// 會持有大量行鎖、產生同樣大量的 undo log，而它跑在帳務主庫上。
// 分塊之後每一塊各自是一筆短交易，中途失敗也只是「少刪了幾塊」——清理排程本來
// 就是延一天做也無害的維運工作（Java 的 javadoc 也是這麼寫的）。
const purgeChunkSize = 1000

// PurgeSentOutbox 刪掉 sent_at 早於 before 的 SENT 列，回傳刪除筆數。
//
// ⚠️ **只刪 SENT**（地雷 #5）。PENDING 代表尚未確認送達，無論多舊都要留著；
// PENDING 長期堆積該由告警與人工處理，不是靠清理排程掩蓋成「表變乾淨了」。
func (r *Repository) PurgeSentOutbox(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		// ⚠️ 每一塊都檢查 ctx：清理可能跑很久，收工訊號要能中斷它。
		// 中斷是安全的——刪到一半只是「這次少刪幾塊」，下次排程接著刪。
		if err := ctx.Err(); err != nil {
			return total, err
		}

		res := r.db.WithContext(ctx).Exec(purgeSentOutbox, before, purgeChunkSize)
		if res.Error != nil {
			return total, fmt.Errorf("清理 outbox 失敗（before=%s）: %w", before.Format(time.RFC3339), res.Error)
		}
		total += res.RowsAffected
		if res.RowsAffected < purgeChunkSize {
			return total, nil
		}
	}
}
