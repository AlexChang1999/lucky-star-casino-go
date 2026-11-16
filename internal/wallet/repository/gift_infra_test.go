//go:build infra

package repository

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/enum"
)

var giftNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func mustInsertGift(t *testing.T, e *testEnv, key string, next time.Time) GiftTransfer {
	t.Helper()
	g, err := e.repo.InsertGift(e.ctx, key, 1, 2, 300, next)
	if err != nil {
		t.Fatalf("InsertGift(%q): %v", key, err)
	}
	return g
}

func TestGiftInsertAndLookup(t *testing.T) {
	e := newTestEnv(t)
	g := mustInsertGift(t, e, "k1", giftNow)
	if g.Status != enum.GiftPending || g.Attempts != 0 || g.Amount != 300 {
		t.Fatalf("新單 = %+v", g)
	}
	// 四個指標欄位在對應步驟完成前是 NULL，不是 0（地雷 #33）。
	if g.DebitTransactionID != nil || g.CreditTransactionID != nil ||
		g.SenderBalanceAfter != nil || g.ReceiverBalanceAfter != nil {
		t.Fatalf("尚未發生的欄位應為 nil: %+v", g)
	}
	byID, err := e.repo.GiftByID(e.ctx, g.ID)
	if err != nil || byID.IdempotencyKey != "k1" {
		t.Fatalf("GiftByID = %+v, %v", byID, err)
	}
	if _, err := e.repo.GiftByKey(e.ctx, "nope"); !errors.Is(err, ErrGiftNotFound) {
		t.Fatalf("查無應回 ErrGiftNotFound, got %v", err)
	}
}

// 同鍵走 UNIQUE 衝突；而且 utf8mb4_bin 讓 "K1" 與 "k1" 是**兩把不同的鍵**（地雷 #30）。
func TestGiftIdempotencyKeyIsCaseSensitive(t *testing.T) {
	e := newTestEnv(t)
	mustInsertGift(t, e, "k1", giftNow)

	if _, err := e.repo.InsertGift(e.ctx, "k1", 1, 2, 300, giftNow); !errors.Is(err, ErrGiftDuplicateKey) {
		t.Fatalf("同鍵應回 ErrGiftDuplicateKey, got %v", err)
	}
	if _, err := e.repo.InsertGift(e.ctx, "K1", 1, 2, 300, giftNow); err != nil {
		t.Fatalf("大小寫不同是另一把鍵，不該被當成重複: %v", err)
	}
	if _, err := e.repo.InsertGift(e.ctx, "k1é", 1, 2, 300, giftNow); err != nil {
		t.Fatalf("ai 定序下 e 與 é 會被當成同一把鍵: %v", err)
	}
}

// 條件式轉移：只有目前狀態對得上才改得到；重做一次回 false 而不是 true。
func TestGiftTransitionsAreConditional(t *testing.T) {
	e := newTestEnv(t)
	g := mustInsertGift(t, e, "k1", giftNow)
	next := giftNow.Add(time.Minute)

	// PENDING 不能直接跳到 COMPLETED / REFUNDING / REFUNDED。
	for name, fn := range map[string]func() (bool, error){
		"MarkGiftCompleted": func() (bool, error) { return e.repo.MarkGiftCompleted(e.ctx, g.ID, 1, 1) },
		"MarkGiftRefunding": func() (bool, error) { return e.repo.MarkGiftRefunding(e.ctx, g.ID, next, "x") },
		"MarkGiftRefunded":  func() (bool, error) { return e.repo.MarkGiftRefunded(e.ctx, g.ID, 1) },
	} {
		if ok, err := fn(); err != nil || ok {
			t.Fatalf("%s 在 PENDING 上應回 (false, nil), got (%v, %v)", name, ok, err)
		}
	}

	if ok, err := e.repo.MarkGiftDebited(e.ctx, g.ID, 10, 700, next); err != nil || !ok {
		t.Fatalf("MarkGiftDebited = (%v, %v)", ok, err)
	}
	if ok, _ := e.repo.MarkGiftDebited(e.ctx, g.ID, 10, 700, next); ok {
		t.Fatal("同一個轉移做第二次應回 false（別人已經轉走了）")
	}
	got, _ := e.repo.GiftByID(e.ctx, g.ID)
	if got.Status != enum.GiftDebited || got.DebitTransactionID == nil || *got.DebitTransactionID != 10 ||
		got.SenderBalanceAfter == nil || *got.SenderBalanceAfter != 700 {
		t.Fatalf("DEBITED 後的單 = %+v", got)
	}

	if ok, err := e.repo.MarkGiftCompleted(e.ctx, g.ID, 11, 350); err != nil || !ok {
		t.Fatalf("MarkGiftCompleted = (%v, %v)", ok, err)
	}
	got, _ = e.repo.GiftByID(e.ctx, g.ID)
	if got.Status != enum.GiftCompleted || got.NextAttemptAt != nil || got.ReceiverBalanceAfter == nil || *got.ReceiverBalanceAfter != 350 {
		t.Fatalf("COMPLETED 後的單 = %+v", got)
	}
	// 終態不能再被退款。
	if ok, _ := e.repo.MarkGiftRefunding(e.ctx, g.ID, next, "x"); ok {
		t.Fatal("COMPLETED 不可轉成 REFUNDING")
	}
}

func TestGiftRefundPath(t *testing.T) {
	e := newTestEnv(t)
	g := mustInsertGift(t, e, "k1", giftNow)
	next := giftNow.Add(time.Minute)
	if ok, _ := e.repo.MarkGiftDebited(e.ctx, g.ID, 10, 700, next); !ok {
		t.Fatal("MarkGiftDebited")
	}
	if ok, _ := e.repo.MarkGiftRefunding(e.ctx, g.ID, next, "wallet locked"); !ok {
		t.Fatal("MarkGiftRefunding")
	}
	if ok, _ := e.repo.MarkGiftRefunded(e.ctx, g.ID, 1000); !ok {
		t.Fatal("MarkGiftRefunded")
	}
	got, _ := e.repo.GiftByID(e.ctx, g.ID)
	if got.Status != enum.GiftRefunded || got.NextAttemptAt != nil || got.SenderBalanceAfter == nil || *got.SenderBalanceAfter != 1000 {
		t.Fatalf("REFUNDED 後的單 = %+v", got)
	}
}

// ⚠️ 只刪 PENDING：DEBITED 之後刪單，錢就沒人知道要入帳或退款。
func TestGiftDeleteOnlyPending(t *testing.T) {
	e := newTestEnv(t)
	pending := mustInsertGift(t, e, "p", giftNow)
	debited := mustInsertGift(t, e, "d", giftNow)
	if ok, _ := e.repo.MarkGiftDebited(e.ctx, debited.ID, 1, 1, giftNow); !ok {
		t.Fatal("MarkGiftDebited")
	}

	if ok, err := e.repo.DeletePendingGift(e.ctx, debited.ID); err != nil || ok {
		t.Fatalf("DEBITED 不可刪: (%v, %v)", ok, err)
	}
	if ok, err := e.repo.DeletePendingGift(e.ctx, pending.ID); err != nil || !ok {
		t.Fatalf("PENDING 應可刪: (%v, %v)", ok, err)
	}
	// 刪掉之後鍵被釋放，可以重新立單。
	if _, err := e.repo.InsertGift(e.ctx, "p", 1, 2, 300, giftNow); err != nil {
		t.Fatalf("刪單後同鍵應可再立: %v", err)
	}
}

func TestGiftDueSelection(t *testing.T) {
	e := newTestEnv(t)
	due := mustInsertGift(t, e, "due", giftNow.Add(-time.Second))
	mustInsertGift(t, e, "future", giftNow.Add(time.Hour))
	done := mustInsertGift(t, e, "done", giftNow.Add(-time.Hour))
	if ok, err := e.repo.MarkGiftDebited(e.ctx, done.ID, 1, 1, giftNow.Add(-time.Hour)); err != nil || !ok {
		t.Fatalf("MarkGiftDebited = (%v, %v)", ok, err)
	}
	if ok, err := e.repo.MarkGiftCompleted(e.ctx, done.ID, 2, 2); err != nil || !ok { // 終態 next_attempt_at = NULL
		t.Fatalf("MarkGiftCompleted = (%v, %v)", ok, err)
	}

	got, err := e.repo.DueGifts(e.ctx, giftNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != due.ID {
		t.Fatalf("DueGifts = %+v, want 只有 due", got)
	}

	if limited, _ := e.repo.DueGifts(e.ctx, giftNow.Add(2*time.Hour), 1); len(limited) != 1 {
		t.Fatalf("limit=1 應只回 1 張, got %d", len(limited))
	}
}

func TestGiftScheduleRetryRespectsStatus(t *testing.T) {
	e := newTestEnv(t)
	g := mustInsertGift(t, e, "k1", giftNow)
	next := giftNow.Add(time.Minute)

	if ok, err := e.repo.ScheduleGiftRetry(e.ctx, g.ID, enum.GiftPending, 3, next, "boom"); err != nil || !ok {
		t.Fatalf("ScheduleGiftRetry = (%v, %v)", ok, err)
	}
	got, _ := e.repo.GiftByID(e.ctx, g.ID)
	if got.Attempts != 3 || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(next) {
		t.Fatalf("排程後 = %+v", got)
	}
	// 呼叫端看到的狀態已過期（別人轉走了）：不可動 attempts。
	if ok, _ := e.repo.ScheduleGiftRetry(e.ctx, g.ID, enum.GiftDebited, 9, next, "boom"); ok {
		t.Fatal("狀態對不上時不該更新")
	}
	if got, _ := e.repo.GiftByID(e.ctx, g.ID); got.Attempts != 3 {
		t.Fatalf("attempts 被錯誤覆寫: %d", got.Attempts)
	}
}

// 錯誤訊息是中文：以 byte 截會切在字中間，寫進 utf8mb4 欄位就是 1366。
func TestGiftLongChineseErrorIsTruncatedSafely(t *testing.T) {
	e := newTestEnv(t)
	g := mustInsertGift(t, e, "k1", giftNow)
	msg := strings.Repeat("錯", 400)
	if ok, err := e.repo.ScheduleGiftRetry(e.ctx, g.ID, enum.GiftPending, 1, giftNow, msg); err != nil || !ok {
		t.Fatalf("長中文訊息應被安全截斷: (%v, %v)", ok, err)
	}
}

func TestGiftCheckConstraints(t *testing.T) {
	e := newTestEnv(t)
	tests := []struct {
		name, sql string
	}{
		{"自己送自己", "INSERT INTO gift_transfers (idempotency_key, sender_id, receiver_id, amount, status) VALUES ('a', 5, 5, 10, 'PENDING')"},
		{"金額為 0", "INSERT INTO gift_transfers (idempotency_key, sender_id, receiver_id, amount, status) VALUES ('b', 1, 2, 0, 'PENDING')"},
		{"未知狀態", "INSERT INTO gift_transfers (idempotency_key, sender_id, receiver_id, amount, status) VALUES ('c', 1, 2, 10, 'DONE')"},
		// utf8mb4_bin：小寫 'pending' 不可混進列舉（地雷 #30）。
		{"小寫狀態", "INSERT INTO gift_transfers (idempotency_key, sender_id, receiver_id, amount, status) VALUES ('d', 1, 2, 10, 'pending')"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := e.db.WithContext(e.ctx).Exec(tt.sql).Error; err == nil {
				t.Fatal("應被 CHECK 約束擋下")
			}
		})
	}
}
