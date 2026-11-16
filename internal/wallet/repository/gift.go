package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/enum"
)

// 贈禮狀態單（gift_transfers）的存取。設計見 docs/ADR-005 與 migration 00003。
//
// ⭐ 這個檔裡**沒有任何一筆交易、也沒有任何一把列鎖**，這是刻意的：
// 每個狀態轉移都是一條「WHERE status = 舊狀態」的條件 UPDATE，受影響列數為 0
// 就代表別人（另一個請求、另一個副本的背景重試）已經先轉走了。
// 而真正動錢的三步各自帶固定冪等鍵，兩邊同時做同一步只會有一邊真的記帳。
// 所以這裡不需要 `FOR UPDATE`，也就不會踩到地雷 #37 的 gap lock。

var (
	// ErrGiftDuplicateKey 表示同一把冪等鍵的狀態單已經存在（1062）。
	ErrGiftDuplicateKey = errors.New("贈禮冪等鍵已存在")
	// ErrGiftNotFound 表示查無此狀態單。
	ErrGiftNotFound = errors.New("贈禮狀態單不存在")
)

// GiftTransfer 是一張贈禮狀態單。
//
// 四個指標欄位在對應步驟完成前是 NULL（地雷 #33）：用 int64 的話「還沒入帳」
// 會靜靜變成「入帳交易編號 0」。
type GiftTransfer struct {
	ID                   int64           `gorm:"column:id;primaryKey"`
	IdempotencyKey       string          `gorm:"column:idempotency_key"`
	SenderID             int64           `gorm:"column:sender_id"`
	ReceiverID           int64           `gorm:"column:receiver_id"`
	Amount               int64           `gorm:"column:amount"`
	Status               enum.GiftStatus `gorm:"column:status"`
	Attempts             int             `gorm:"column:attempts"`
	NextAttemptAt        *time.Time      `gorm:"column:next_attempt_at"`
	DebitTransactionID   *int64          `gorm:"column:debit_transaction_id"`
	CreditTransactionID  *int64          `gorm:"column:credit_transaction_id"`
	SenderBalanceAfter   *int64          `gorm:"column:sender_balance_after"`
	ReceiverBalanceAfter *int64          `gorm:"column:receiver_balance_after"`
}

// TableName 讓 GORM 對到 gift_transfers。
func (GiftTransfer) TableName() string { return "gift_transfers" }

const giftColumns = `id, idempotency_key, sender_id, receiver_id, amount, status, attempts, next_attempt_at,
	debit_transaction_id, credit_transaction_id, sender_balance_after, receiver_balance_after`

const (
	insertGift = `INSERT INTO gift_transfers
		(idempotency_key, sender_id, receiver_id, amount, status, attempts, next_attempt_at)
		VALUES (?, ?, ?, ?, 'PENDING', 0, ?)`

	selectGiftByKey = `SELECT ` + giftColumns + ` FROM gift_transfers WHERE idempotency_key = ?`
	selectGiftByID  = `SELECT ` + giftColumns + ` FROM gift_transfers WHERE id = ?`

	// ⚠️ 只刪 PENDING：DEBITED 之後錢已經動了，刪掉狀態單就再也沒有人知道要入帳或退款。
	deletePendingGift = `DELETE FROM gift_transfers WHERE id = ? AND status = 'PENDING'`

	markGiftDebited = `UPDATE gift_transfers
		SET status = 'DEBITED', attempts = 0, next_attempt_at = ?, last_error = NULL,
		    debit_transaction_id = ?, sender_balance_after = ?
		WHERE id = ? AND status = 'PENDING'`

	markGiftCompleted = `UPDATE gift_transfers
		SET status = 'COMPLETED', next_attempt_at = NULL, last_error = NULL,
		    credit_transaction_id = ?, receiver_balance_after = ?
		WHERE id = ? AND status = 'DEBITED'`

	markGiftRefunding = `UPDATE gift_transfers
		SET status = 'REFUNDING', attempts = 0, next_attempt_at = ?, last_error = ?
		WHERE id = ? AND status = 'DEBITED'`

	// ⚠️ sender_balance_after 一併更新成退款後的餘額：冪等重送時回的是「現在的結果」。
	markGiftRefunded = `UPDATE gift_transfers
		SET status = 'REFUNDED', next_attempt_at = NULL, sender_balance_after = ?
		WHERE id = ? AND status = 'REFUNDING'`

	// 失敗後排下一次重試。status 也放進 WHERE：這次失敗期間若別人已經把它轉走，
	// 就不該再把 attempts 加上去。
	scheduleGiftRetry = `UPDATE gift_transfers
		SET attempts = ?, next_attempt_at = ?, last_error = ?
		WHERE id = ? AND status = ?`

	selectDueGifts = `SELECT ` + giftColumns + ` FROM gift_transfers
		WHERE status IN ('PENDING', 'DEBITED', 'REFUNDING') AND next_attempt_at <= ?
		ORDER BY next_attempt_at LIMIT ?`
)

// maxGiftErrorLen 對應 last_error 的 VARCHAR(255)。
const maxGiftErrorLen = 255

// InsertGift 立一張 PENDING 的狀態單。冪等鍵已存在時回 ErrGiftDuplicateKey。
//
// ⚠️ 走 UNIQUE 衝突而不是先查後寫（地雷 #3）：先查後寫在兩個同鍵請求同時到達時
// 兩邊都查不到、兩邊都往下做。
// ⚠️ 認 1062 而不是用 INSERT IGNORE（地雷 #26）。
func (r *Repository) InsertGift(ctx context.Context, key string, senderID, receiverID int64, amount domain.Amount, nextAttemptAt time.Time) (GiftTransfer, error) {
	res := r.db.WithContext(ctx).Exec(insertGift, key, senderID, receiverID, int64(amount), nextAttemptAt.UTC())
	if res.Error != nil {
		if isDupEntry(res.Error) {
			return GiftTransfer{}, fmt.Errorf("%w: key=%q", ErrGiftDuplicateKey, key)
		}
		return GiftTransfer{}, fmt.Errorf("建立贈禮狀態單失敗: %w", res.Error)
	}
	return r.GiftByKey(ctx, key)
}

// GiftByKey 以冪等鍵查狀態單，查無回 ErrGiftNotFound。
func (r *Repository) GiftByKey(ctx context.Context, key string) (GiftTransfer, error) {
	return r.oneGift(ctx, selectGiftByKey, key)
}

// GiftByID 以主鍵查狀態單，查無回 ErrGiftNotFound。
func (r *Repository) GiftByID(ctx context.Context, id int64) (GiftTransfer, error) {
	return r.oneGift(ctx, selectGiftByID, id)
}

func (r *Repository) oneGift(ctx context.Context, query string, arg any) (GiftTransfer, error) {
	var rows []GiftTransfer
	if err := r.db.WithContext(ctx).Raw(query, arg).Scan(&rows).Error; err != nil {
		return GiftTransfer{}, fmt.Errorf("查詢贈禮狀態單失敗: %w", err)
	}
	if len(rows) == 0 {
		return GiftTransfer{}, fmt.Errorf("%w: %v", ErrGiftNotFound, arg)
	}
	return rows[0], nil
}

// DeletePendingGift 刪掉一張還沒扣款的狀態單，回報是否真的刪了。
// 用在「扣款被明確拒絕」：什麼都沒動，冪等鍵應該釋放出來讓玩家補足餘額後再送一次。
func (r *Repository) DeletePendingGift(ctx context.Context, id int64) (bool, error) {
	return r.giftTransition(ctx, deletePendingGift, id)
}

// MarkGiftDebited 把 PENDING 轉成 DEBITED，回報是否真的轉了。
func (r *Repository) MarkGiftDebited(ctx context.Context, id, debitTxID, senderBalanceAfter int64, nextAttemptAt time.Time) (bool, error) {
	return r.giftTransition(ctx, markGiftDebited, nextAttemptAt.UTC(), debitTxID, senderBalanceAfter, id)
}

// MarkGiftCompleted 把 DEBITED 轉成 COMPLETED，回報是否真的轉了。
func (r *Repository) MarkGiftCompleted(ctx context.Context, id, creditTxID, receiverBalanceAfter int64) (bool, error) {
	return r.giftTransition(ctx, markGiftCompleted, creditTxID, receiverBalanceAfter, id)
}

// MarkGiftRefunding 把 DEBITED 轉成 REFUNDING（放棄入帳、開始退款），回報是否真的轉了。
func (r *Repository) MarkGiftRefunding(ctx context.Context, id int64, nextAttemptAt time.Time, lastErr string) (bool, error) {
	return r.giftTransition(ctx, markGiftRefunding, nextAttemptAt.UTC(), truncateGiftError(lastErr), id)
}

// MarkGiftRefunded 把 REFUNDING 轉成 REFUNDED，回報是否真的轉了。
func (r *Repository) MarkGiftRefunded(ctx context.Context, id, senderBalanceAfter int64) (bool, error) {
	return r.giftTransition(ctx, markGiftRefunded, senderBalanceAfter, id)
}

// ScheduleGiftRetry 記下這個階段又失敗一次，並排定下一次重試時間。
// status 是呼叫端看到的狀態；已經被別人轉走時不做任何事並回 false。
func (r *Repository) ScheduleGiftRetry(ctx context.Context, id int64, status enum.GiftStatus, attempts int, nextAttemptAt time.Time, lastErr string) (bool, error) {
	return r.giftTransition(ctx, scheduleGiftRetry, attempts, nextAttemptAt.UTC(), truncateGiftError(lastErr), id, string(status))
}

// giftTransition 執行一條條件式寫入並回報「是否真的改到那一列」。
//
// ⚠️ RowsAffected == 0 不是錯誤，是**別人已經先轉走了**（地雷 #3）。
// 回 false 讓呼叫端重讀狀態單、照新的狀態繼續，而不是當成成功往下走。
func (r *Repository) giftTransition(ctx context.Context, query string, args ...any) (bool, error) {
	res := r.db.WithContext(ctx).Exec(query, args...)
	if res.Error != nil {
		return false, fmt.Errorf("更新贈禮狀態單失敗: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// DueGifts 撈出「未完成、且已到重試時間」的狀態單，最多 limit 張。
//
// ⚠️ 沒有 FOR UPDATE（理由見檔頭）。兩個副本撈到同一張只會各做一次同樣的冪等步驟。
func (r *Repository) DueGifts(ctx context.Context, now time.Time, limit int) ([]GiftTransfer, error) {
	var rows []GiftTransfer
	if err := r.db.WithContext(ctx).Raw(selectDueGifts, now.UTC(), limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("撈取待重試的贈禮狀態單失敗: %w", err)
	}
	return rows, nil
}

// truncateGiftError 把錯誤訊息截到欄位長度以內。
// ⚠️ 以 rune 為單位截：VARCHAR(255) 算的是字元，而錯誤訊息是中文，
// 以 byte 截會切在一個字的中間，寫進去就是 1366（Incorrect string value）。
func truncateGiftError(msg string) string {
	runes := []rune(msg)
	if len(runes) <= maxGiftErrorLen {
		return msg
	}
	return string(runes[:maxGiftErrorLen])
}

// CreditLanded 回報某把冪等鍵在 wallet_transactions 裡是否已有流水。
//
// 用在「放棄入帳、改退款」之前：入帳的結果可能不明（commit 逾時），也可能被另一個副本做完。
// ⚠️ 這是檢查而非鎖：檢查到退款之間，另一個**已經在途**的入帳仍可能提交，視窗很窄但存在；
// 完全消除需要把入帳與狀態單放進同一個交易（見 ADR-005 的「已知限制」）。
func (r *Repository) CreditLanded(ctx context.Context, idempotencyKey string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).
		Raw("SELECT COUNT(*) FROM wallet_transactions WHERE idempotency_key = ?", idempotencyKey).
		Row().Scan(&n)
	if err != nil {
		return false, fmt.Errorf("查詢入帳流水失敗: %w", err)
	}
	return n > 0, nil
}
