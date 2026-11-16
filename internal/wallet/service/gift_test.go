package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/enum"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/quota"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/repository"
)

// giftWorld 同時扮演 Repository（帳務）與 GiftRepository（狀態單）的記憶體替身。
//
// 帳務那一半刻意**照真的 repository 的語義**：冪等鍵命中回原交易、餘額不足與錢包不存在回
// 對應 sentinel。這樣測試驗的是「service 在這些語義下把狀態單推對了沒有」，
// 而不是「fake 回什麼我就信什麼」。
type giftWorld struct {
	balances map[int64]int64
	ledger   map[string]ledgerRow
	nextTxID int64

	// 故障注入
	creditErr       func(m domain.Movement) error // 非 nil 回傳 → 該次入帳失敗且**沒有**記帳
	markDebitedErr  error                         // 一次性：MarkGiftDebited 失敗（扣款已記帳）
	debitCalls      int
	creditCalls     map[string]int
	debitsRecorded  int
	creditsRecorded int
	refundsRecorded int
	gifts           map[int64]*repository.GiftTransfer
	byKey           map[string]int64
	nextGiftID      int64
	deleted         int
}

type ledgerRow struct {
	txID     int64
	playerID int64
	amount   int64
	after    int64
}

func newGiftWorld(balances map[int64]int64) *giftWorld {
	return &giftWorld{
		balances:    balances,
		ledger:      map[string]ledgerRow{},
		creditCalls: map[string]int{},
		gifts:       map[int64]*repository.GiftTransfer{},
		byKey:       map[string]int64{},
	}
}

// ── Repository ──────────────────────────────────────────────────────────────

func (w *giftWorld) Balance(_ context.Context, id int64) (int64, error) {
	b, ok := w.balances[id]
	if !ok {
		return 0, fmt.Errorf("%w: playerID=%d", repository.ErrWalletNotFound, id)
	}
	return b, nil
}

func (w *giftWorld) Debit(_ context.Context, m domain.Movement) (repository.DebitResult, error) {
	w.debitCalls++
	if row, ok := w.ledger[m.IdempotencyKey]; ok {
		return repository.DebitResult{TransactionID: row.txID, PlayerID: row.playerID,
			Amount: domain.Amount(row.amount), BalanceAfter: domain.Amount(row.after), Idempotent: true}, nil
	}
	bal, ok := w.balances[m.PlayerID]
	if !ok {
		return repository.DebitResult{}, fmt.Errorf("%w: playerID=%d", repository.ErrWalletNotFound, m.PlayerID)
	}
	if bal < int64(m.Amount) {
		return repository.DebitResult{}, repository.ErrInsufficientBalance
	}
	w.balances[m.PlayerID] = bal - int64(m.Amount)
	w.nextTxID++
	w.ledger[m.IdempotencyKey] = ledgerRow{w.nextTxID, m.PlayerID, int64(m.Amount), bal - int64(m.Amount)}
	w.debitsRecorded++
	return repository.DebitResult{TransactionID: w.nextTxID, PlayerID: m.PlayerID, Amount: m.Amount,
		BalanceBefore: domain.Amount(bal), BalanceAfter: domain.Amount(bal - int64(m.Amount))}, nil
}

func (w *giftWorld) Credit(_ context.Context, m domain.Movement) (repository.CreditResult, error) {
	w.creditCalls[m.IdempotencyKey]++
	if row, ok := w.ledger[m.IdempotencyKey]; ok {
		return repository.CreditResult{TransactionID: row.txID, PlayerID: row.playerID,
			Amount: domain.Amount(row.amount), BalanceAfter: domain.Amount(row.after), Idempotent: true}, nil
	}
	if w.creditErr != nil {
		if err := w.creditErr(m); err != nil {
			return repository.CreditResult{}, err
		}
	}
	bal, ok := w.balances[m.PlayerID]
	if !ok {
		return repository.CreditResult{}, fmt.Errorf("%w: playerID=%d", repository.ErrWalletNotFound, m.PlayerID)
	}
	w.balances[m.PlayerID] = bal + int64(m.Amount)
	w.nextTxID++
	w.ledger[m.IdempotencyKey] = ledgerRow{w.nextTxID, m.PlayerID, int64(m.Amount), bal + int64(m.Amount)}
	if m.SubType == domain.SubTypeRefund {
		w.refundsRecorded++
	} else {
		w.creditsRecorded++
	}
	return repository.CreditResult{TransactionID: w.nextTxID, PlayerID: m.PlayerID, Amount: m.Amount,
		BalanceBefore: domain.Amount(bal), BalanceAfter: domain.Amount(bal + int64(m.Amount))}, nil
}

// ── GiftRepository ─────────────────────────────────────────────────────────

func (w *giftWorld) InsertGift(_ context.Context, key string, sender, receiver int64, amt domain.Amount, next time.Time) (repository.GiftTransfer, error) {
	if _, dup := w.byKey[key]; dup {
		return repository.GiftTransfer{}, repository.ErrGiftDuplicateKey
	}
	w.nextGiftID++
	g := &repository.GiftTransfer{ID: w.nextGiftID, IdempotencyKey: key, SenderID: sender, ReceiverID: receiver,
		Amount: int64(amt), Status: enum.GiftPending, NextAttemptAt: &next}
	w.gifts[g.ID] = g
	w.byKey[key] = g.ID
	return *g, nil
}

func (w *giftWorld) GiftByKey(_ context.Context, key string) (repository.GiftTransfer, error) {
	id, ok := w.byKey[key]
	if !ok {
		return repository.GiftTransfer{}, repository.ErrGiftNotFound
	}
	return *w.gifts[id], nil
}

func (w *giftWorld) GiftByID(_ context.Context, id int64) (repository.GiftTransfer, error) {
	g, ok := w.gifts[id]
	if !ok {
		return repository.GiftTransfer{}, repository.ErrGiftNotFound
	}
	return *g, nil
}

func (w *giftWorld) DeletePendingGift(_ context.Context, id int64) (bool, error) {
	g, ok := w.gifts[id]
	if !ok || g.Status != enum.GiftPending {
		return false, nil
	}
	delete(w.byKey, g.IdempotencyKey)
	delete(w.gifts, id)
	w.deleted++
	return true, nil
}

// transition 是條件式轉移：目前狀態不是 from 就回 false（別人先轉走了）。
func (w *giftWorld) transition(id int64, from enum.GiftStatus, apply func(g *repository.GiftTransfer)) bool {
	g, ok := w.gifts[id]
	if !ok || g.Status != from {
		return false
	}
	apply(g)
	return true
}

func (w *giftWorld) MarkGiftDebited(_ context.Context, id, txID, after int64, next time.Time) (bool, error) {
	if w.markDebitedErr != nil {
		err := w.markDebitedErr
		w.markDebitedErr = nil
		return false, err
	}
	return w.transition(id, enum.GiftPending, func(g *repository.GiftTransfer) {
		g.Status, g.Attempts, g.NextAttemptAt = enum.GiftDebited, 0, &next
		g.DebitTransactionID, g.SenderBalanceAfter = &txID, &after
	}), nil
}

func (w *giftWorld) MarkGiftCompleted(_ context.Context, id, txID, after int64) (bool, error) {
	return w.transition(id, enum.GiftDebited, func(g *repository.GiftTransfer) {
		g.Status, g.NextAttemptAt = enum.GiftCompleted, nil
		g.CreditTransactionID, g.ReceiverBalanceAfter = &txID, &after
	}), nil
}

func (w *giftWorld) MarkGiftRefunding(_ context.Context, id int64, next time.Time, _ string) (bool, error) {
	return w.transition(id, enum.GiftDebited, func(g *repository.GiftTransfer) {
		g.Status, g.Attempts, g.NextAttemptAt = enum.GiftRefunding, 0, &next
	}), nil
}

func (w *giftWorld) MarkGiftRefunded(_ context.Context, id, after int64) (bool, error) {
	return w.transition(id, enum.GiftRefunding, func(g *repository.GiftTransfer) {
		g.Status, g.NextAttemptAt, g.SenderBalanceAfter = enum.GiftRefunded, nil, &after
	}), nil
}

func (w *giftWorld) ScheduleGiftRetry(_ context.Context, id int64, status enum.GiftStatus, attempts int, next time.Time, _ string) (bool, error) {
	return w.transition(id, status, func(g *repository.GiftTransfer) {
		g.Attempts, g.NextAttemptAt = attempts, &next
	}), nil
}

func (w *giftWorld) DueGifts(_ context.Context, now time.Time, limit int) ([]repository.GiftTransfer, error) {
	var out []repository.GiftTransfer
	for _, g := range w.gifts {
		switch g.Status {
		case enum.GiftPending, enum.GiftDebited, enum.GiftRefunding:
			if g.NextAttemptAt != nil && !g.NextAttemptAt.After(now) && len(out) < limit {
				out = append(out, *g)
			}
		}
	}
	return out, nil
}

func (w *giftWorld) CreditLanded(_ context.Context, key string) (bool, error) {
	_, ok := w.ledger[key]
	return ok, nil
}

// ── 測試輔助 ──────────────────────────────────────────────────────────

type giftHarness struct {
	*giftWorld
	svc   *Service
	quota *fakeQuota
	clock time.Time
}

func newGiftHarness(balances map[int64]int64) *giftHarness {
	w := newGiftWorld(balances)
	q := &fakeQuota{}
	h := &giftHarness{giftWorld: w, quota: q, clock: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	h.svc = New(w, w, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.svc.now = func() time.Time { return h.clock }
	return h
}

// drain 讓背景重試跑到沒有到期的單為止：每輪先把時鐘撥過 giftRetryCap，
// 所以退避時間不會讓測試卡住，也不需要真的 sleep。
func (h *giftHarness) drain(t *testing.T) {
	t.Helper()
	for range 50 {
		h.clock = h.clock.Add(giftRetryCap + time.Second)
		n, err := h.svc.RetryDueGifts(context.Background())
		if err != nil {
			t.Fatalf("RetryDueGifts: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("背景重試 50 輪還沒收斂")
}

func totalMoney(m map[int64]int64) (sum int64) {
	for _, v := range m {
		sum += v
	}
	return
}

// ── 測試 ──────────────────────────────────────────────────────────────

func TestGiftHappyPath(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 50})
	r, err := h.svc.Gift(context.Background(), 1, 2, 300, "k1")
	if err != nil {
		t.Fatalf("Gift: %v", err)
	}
	if r.Status != enum.GiftCompleted || r.Idempotent {
		t.Fatalf("status=%s idempotent=%v, want COMPLETED/false", r.Status, r.Idempotent)
	}
	if h.balances[1] != 700 || h.balances[2] != 350 {
		t.Fatalf("balances = %v, want 1:700 2:350", h.balances)
	}
	if r.DebitTransactionID == nil || r.CreditTransactionID == nil || r.SenderBalanceAfter == nil || r.ReceiverBalanceAfter == nil {
		t.Fatalf("COMPLETED 的結果四個欄位都該有值: %+v", r)
	}
	if *r.SenderBalanceAfter != 700 || *r.ReceiverBalanceAfter != 350 {
		t.Fatalf("balanceAfter = %d/%d, want 700/350", *r.SenderBalanceAfter, *r.ReceiverBalanceAfter)
	}
	if h.quota.giftReserved != 1 || h.quota.giftReleased != 0 {
		t.Fatalf("額度 reserved=%d released=%d, want 1/0", h.quota.giftReserved, h.quota.giftReleased)
	}
}

// 什麼都沒動就結束的出口，額度必須回補；否則玩家每被拒一次就少一次額度。
func TestGiftRejectedBeforeMoneyMovesReleasesQuota(t *testing.T) {
	tests := []struct {
		name     string
		balances map[int64]int64
		amount   int64
		wantCode Code
		debited  bool // 是否走到了 repository.Debit（預檢沒擋住、由扣款本身拒絕）
	}{
		{"預檢：餘額不足", map[int64]int64{1: 100, 2: 0}, 300, CodeInsufficientBalance, false},
		{"預檢：送出方錢包不存在", map[int64]int64{2: 0}, 10, CodeGiftSenderNotFound, false},
		{"預檢：接收方錢包不存在", map[int64]int64{1: 100}, 10, CodeGiftReceiverNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGiftHarness(tt.balances)
			_, err := h.svc.Gift(context.Background(), 1, 2, tt.amount, "k")
			if got := CodeOf(err); got != tt.wantCode {
				t.Fatalf("code = %v, want %v (err=%v)", got, tt.wantCode, err)
			}
			if h.quota.giftReleased != 1 {
				t.Errorf("額度回補 %d 次, want 1", h.quota.giftReleased)
			}
			if len(h.gifts) != 0 || h.debitCalls != 0 {
				t.Errorf("不該留下狀態單或碰帳務: gifts=%d debitCalls=%d", len(h.gifts), h.debitCalls)
			}
		})
	}
}

// 預檢通過、扣款當下才被拒（預檢到扣款之間餘額被花掉）：刪單、回補，鍵可以再用。
func TestGiftDebitRejectedAfterPrecheckDeletesPendingRow(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 500, 2: 0})
	// 預檢看到 500，但真正扣款時餘額已經被別的請求花掉。
	h.svc.repo = &spendBeforeDebit{giftWorld: h.giftWorld, spend: 450}

	_, err := h.svc.Gift(context.Background(), 1, 2, 300, "k")
	if got := CodeOf(err); got != CodeInsufficientBalance {
		t.Fatalf("code = %v, want InsufficientBalance (err=%v)", got, err)
	}
	if h.deleted != 1 || len(h.gifts) != 0 {
		t.Errorf("狀態單該被刪掉: deleted=%d remaining=%d", h.deleted, len(h.gifts))
	}
	if h.quota.giftReleased != 1 {
		t.Errorf("額度回補 %d 次, want 1", h.quota.giftReleased)
	}
}

type spendBeforeDebit struct {
	*giftWorld
	spend int64
}

func (s *spendBeforeDebit) Debit(ctx context.Context, m domain.Movement) (repository.DebitResult, error) {
	s.balances[m.PlayerID] -= s.spend
	return s.giftWorld.Debit(ctx, m)
}

func TestGiftQuotaExceeded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"送出超限", quota.ErrGiftSentLimitExceeded, CodeGiftSentLimitExceeded},
		{"收受超限", quota.ErrGiftRecvLimitExceeded, CodeGiftRecvLimitExceeded},
		{"Redis 掛了 → fail-closed 500", errors.New("connection refused"), CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
			h.quota.giftErr = tt.err
			_, err := h.svc.Gift(context.Background(), 1, 2, 10, "k")
			if got := CodeOf(err); got != tt.want {
				t.Fatalf("code = %v, want %v", got, tt.want)
			}
			if h.debitCalls != 0 || len(h.gifts) != 0 {
				t.Errorf("額度沒過就不該碰帳務或立單")
			}
			// 超限時 quota 層自己已回補完畢，service 不可再回補一次。
			if h.quota.giftReleased != 0 {
				t.Errorf("service 不該重複回補: released=%d", h.quota.giftReleased)
			}
		})
	}
}

func TestGiftToSelf(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000})
	_, err := h.svc.Gift(context.Background(), 1, 1, 10, "k")
	if got := CodeOf(err); got != CodeInvalidGift {
		t.Fatalf("code = %v, want InvalidGift", got)
	}
	if h.quota.giftReserved != 0 {
		t.Error("送給自己要在動額度之前就擋掉")
	}
}

// 同鍵重送：不可再吃一次額度、不可再扣一次款。
func TestGiftIdempotentReplay(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	first, err := h.svc.Gift(context.Background(), 1, 2, 300, "k")
	if err != nil {
		t.Fatal(err)
	}
	// 同一個送出方用同一把鍵改送別人 / 改金額：照 Java 回原本那一筆。
	again, err := h.svc.Gift(context.Background(), 1, 2, 999, "k")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Idempotent || again.Amount != first.Amount || again.Status != enum.GiftCompleted {
		t.Fatalf("replay = %+v, want 原本那筆 (amount=%d) 且 Idempotent", again, first.Amount)
	}
	if h.quota.giftReserved != 1 {
		t.Errorf("重送吃了額度: reserved=%d, want 1", h.quota.giftReserved)
	}
	if h.debitsRecorded != 1 || h.creditsRecorded != 1 || h.balances[1] != 700 {
		t.Errorf("重送重複記帳: debits=%d credits=%d balances=%v", h.debitsRecorded, h.creditsRecorded, h.balances)
	}
}

func TestGiftKeyOwnedByAnotherSenderIsConflict(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 1000, 3: 0})
	if _, err := h.svc.Gift(context.Background(), 1, 3, 100, "k"); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.Gift(context.Background(), 2, 3, 100, "k")
	if got := CodeOf(err); got != CodeGiftKeyConflict {
		t.Fatalf("code = %v, want GiftKeyConflict", got)
	}
	if h.balances[2] != 1000 {
		t.Error("別人的鍵不可動到呼叫者的錢")
	}
}

// 立單時同鍵已被別的請求搶先立了單：這次什麼都沒做，額度要回補，回贏家的現況。
func TestGiftInsertRaceReturnsWinnerAndReleasesQuota(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	h.svc.gifts = &raceOnInsert{giftWorld: h.giftWorld}

	r, err := h.svc.Gift(context.Background(), 1, 2, 100, "k")
	if err != nil {
		t.Fatalf("Gift: %v", err)
	}
	if !r.Idempotent {
		t.Error("輸掉立單競賽的請求應回冪等結果")
	}
	if h.quota.giftReleased != 1 {
		t.Errorf("輸家要回補自己預扣的額度: released=%d", h.quota.giftReleased)
	}
	if h.debitCalls != 0 {
		t.Error("輸家不可動錢")
	}
}

// raceOnInsert 在第一次 InsertGift 之前，先替「另一個請求」立好同鍵單，再回 1062。
type raceOnInsert struct{ *giftWorld }

func (r *raceOnInsert) InsertGift(ctx context.Context, key string, s, rc int64, a domain.Amount, next time.Time) (repository.GiftTransfer, error) {
	if _, err := r.giftWorld.InsertGift(ctx, key, s, rc, a, next); err != nil {
		return repository.GiftTransfer{}, err
	}
	return r.giftWorld.InsertGift(ctx, key, s, rc, a, next) // 第二次撞鍵
}

// ⭐ 核心不變量：錢已扣、狀態單沒更新（行程死在兩步之間）→ 重試只會冪等命中，帳上仍只有一筆扣款。
func TestGiftCrashBetweenDebitAndMarkNeverDoubleDebits(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	h.markDebitedErr = errors.New("connection reset")

	_, err := h.svc.Gift(context.Background(), 1, 2, 300, "k")
	if CodeOf(err) != CodeInternal {
		t.Fatalf("code = %v, want Internal", CodeOf(err))
	}
	if h.quota.giftReleased != 0 {
		t.Error("錢已扣，額度不可回補")
	}
	if g, _ := h.GiftByKey(context.Background(), "k"); g.Status != enum.GiftPending {
		t.Fatalf("狀態單應停在 PENDING, got %s", g.Status)
	}

	h.drain(t)

	g, _ := h.GiftByKey(context.Background(), "k")
	if g.Status != enum.GiftCompleted {
		t.Fatalf("重試後狀態 = %s, want COMPLETED", g.Status)
	}
	if h.debitsRecorded != 1 || h.creditsRecorded != 1 {
		t.Errorf("記帳筆數 debits=%d credits=%d, want 1/1", h.debitsRecorded, h.creditsRecorded)
	}
	if h.balances[1] != 700 || h.balances[2] != 300 {
		t.Errorf("balances = %v, want 1:700 2:300", h.balances)
	}
}

// 入帳暫時失敗：請求回「處理中」，背景把它做完，最終兩邊各一筆。
func TestGiftCreditTransientFailureCompletesInBackground(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	fails := 2
	h.creditErr = func(domain.Movement) error {
		if fails > 0 {
			fails--
			return errors.New("deadlock")
		}
		return nil
	}

	r, err := h.svc.Gift(context.Background(), 1, 2, 300, "k")
	// 錢已離開送出方、入帳還在路上：回「處理中」而不是錯誤，背景接手。
	if err != nil || r.Status != enum.GiftDebited {
		t.Fatalf("got (%+v, %v), want Status=DEBITED 且 err=nil", r, err)
	}
	if h.balances[1] != 700 {
		t.Fatalf("錢已離開送出方, balance = %d", h.balances[1])
	}

	h.drain(t)

	g, _ := h.GiftByKey(context.Background(), "k")
	if g.Status != enum.GiftCompleted || h.balances[2] != 300 || h.creditsRecorded != 1 {
		t.Fatalf("status=%s receiver=%d credits=%d", g.Status, h.balances[2], h.creditsRecorded)
	}
}

// ⭐ 入帳一直失敗 → 用盡次數 → 退款；總錢數守恆、退款只有一筆。
func TestGiftCreditExhaustedRefundsSender(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	before := totalMoney(h.balances)
	h.creditErr = func(m domain.Movement) error {
		if m.SubType == domain.SubTypeGift {
			return repository.ErrConcurrentModification // 確定沒入帳的失敗，用盡次數才可退款
		}
		return nil // 退款那筆正常
	}

	// 請求當下那一次是第 1 次，其餘靠背景。
	_, _ = h.svc.Gift(context.Background(), 1, 2, 300, "k")
	h.drain(t)

	g, _ := h.GiftByKey(context.Background(), "k")
	if g.Status != enum.GiftRefunded {
		t.Fatalf("status = %s, want REFUNDED", g.Status)
	}
	if h.balances[1] != 1000 || h.balances[2] != 0 {
		t.Errorf("balances = %v, want 送出方全額退回", h.balances)
	}
	if totalMoney(h.balances) != before {
		t.Errorf("總錢數不守恆: %d → %d", before, totalMoney(h.balances))
	}
	if h.refundsRecorded != 1 || h.creditsRecorded != 0 {
		t.Errorf("refunds=%d credits=%d, want 1/0", h.refundsRecorded, h.creditsRecorded)
	}
	if got := h.creditCalls["k:gift:credit"]; got != GiftMaxCreditAttempts {
		t.Errorf("入帳嘗試 %d 次, want %d", got, GiftMaxCreditAttempts)
	}

	// 已退款的贈禮重送：回「已退回」，不是成功。
	_, err := h.svc.Gift(context.Background(), 1, 2, 300, "k")
	if CodeOf(err) != CodeGiftRefunded {
		t.Errorf("重送已退款贈禮: code = %v, want GiftRefunded", CodeOf(err))
	}
}

// 接收方錢包在預檢之後才消失：重試只是讓錢多卡幾秒，直接退款。
func TestGiftReceiverWalletGoneRefundsWithoutRetrying(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	h.svc.repo = &dropReceiverAfterDebit{giftWorld: h.giftWorld, receiver: 2}

	_, err := h.svc.Gift(context.Background(), 1, 2, 300, "k")
	if CodeOf(err) != CodeGiftRefunded {
		t.Fatalf("code = %v, want GiftRefunded (err=%v)", CodeOf(err), err)
	}
	if h.balances[1] != 1000 {
		t.Errorf("送出方 = %d, want 1000", h.balances[1])
	}
	if got := h.creditCalls["k:gift:credit"]; got != 1 {
		t.Errorf("入帳嘗試 %d 次, want 1（不會自己好的錯誤不重試）", got)
	}
}

type dropReceiverAfterDebit struct {
	*giftWorld
	receiver int64
}

func (d *dropReceiverAfterDebit) Debit(ctx context.Context, m domain.Movement) (repository.DebitResult, error) {
	r, err := d.giftWorld.Debit(ctx, m)
	delete(d.balances, d.receiver)
	return r, err
}

func TestGiftBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{0, time.Second}, {1, time.Second}, {2, 2 * time.Second}, {4, 8 * time.Second},
		{20, giftRetryCap}, {1000, giftRetryCap}, // 位移不可溢位成負數
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.attempts), func(t *testing.T) {
			if got := giftBackoff(tt.attempts); got != tt.want {
				t.Errorf("giftBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
			}
		})
	}
}

// 三把冪等鍵由呼叫端的鍵衍生且互不相同；最長鍵衍生後仍塞得進 VARCHAR(100)。
func TestGiftMovementKeys(t *testing.T) {
	key := string(make([]byte, 0))
	for range 80 {
		key += "x"
	}
	d, c, r, err := giftMovements(repository.GiftTransfer{IdempotencyKey: key, SenderID: 1, ReceiverID: 2, Amount: 10})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{d.IdempotencyKey: true, c.IdempotencyKey: true, r.IdempotencyKey: true}
	if len(keys) != 3 {
		t.Fatalf("三把鍵必須互不相同: %v", keys)
	}
	for k := range keys {
		if len(k) > 100 {
			t.Errorf("鍵長 %d 超過 wallet_transactions.idempotency_key 的 100", len(k))
		}
	}
	if d.SubType != domain.SubTypeGift || d.Type != domain.TxTypeDebit {
		t.Errorf("送出方分錄應為 DEBIT/GIFT: %s/%s", d.Type, d.SubType)
	}
}

// ⭐ 結果不明的入帳失敗（CodeInternal）即使次數用盡也不可退款：入帳可能已提交。
// 模擬「第 5 次入帳其實提交了、但客戶端只看到逾時」——最後必須完成，而不是退款。
func TestGiftAmbiguousCreditFailureNeverRefundsEvenWhenExhausted(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	h.svc.repo = &timeoutAfterCommit{giftWorld: h.giftWorld, onAttempt: GiftMaxCreditAttempts}

	_, _ = h.svc.Gift(context.Background(), 1, 2, 300, "k")
	h.drain(t)

	g, _ := h.GiftByKey(context.Background(), "k")
	if g.Status != enum.GiftCompleted {
		t.Fatalf("status = %s, want COMPLETED", g.Status)
	}
	if h.refundsRecorded != 0 || h.creditsRecorded != 1 || h.balances[1] != 700 || h.balances[2] != 300 {
		t.Fatalf("refunds=%d credits=%d balances=%v", h.refundsRecorded, h.creditsRecorded, h.balances)
	}
}

// timeoutAfterCommit：前 onAttempt-1 次入帳回「結果不明」且沒記帳；第 onAttempt 次記帳成功但回逾時。
type timeoutAfterCommit struct {
	*giftWorld
	onAttempt int
	seen      int
}

func (t *timeoutAfterCommit) Credit(ctx context.Context, m domain.Movement) (repository.CreditResult, error) {
	if m.SubType != domain.SubTypeGift {
		return t.giftWorld.Credit(ctx, m)
	}
	t.seen++
	switch {
	case t.seen < t.onAttempt:
		return repository.CreditResult{}, errors.New("i/o timeout")
	case t.seen == t.onAttempt:
		if _, err := t.giftWorld.Credit(ctx, m); err != nil {
			return repository.CreditResult{}, err
		}
		return repository.CreditResult{}, errors.New("commit timeout") // 已提交，客戶端不知道
	}
	return t.giftWorld.Credit(ctx, m)
}

// 防線一（單獨成立）：結果不明的失敗，入帳**從未**提交，次數再多也不退款，狀態單維持 DEBITED 繼續重試。
// 拿掉「CodeInternal 不算次數用盡」，這個測試會紅（CreditLanded 在這裡永遠回 false，擋不住）。
func TestGiftAmbiguousFailureKeepsRetryingNeverRefunds(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	h.creditErr = func(domain.Movement) error { return errors.New("i/o timeout") }

	_, _ = h.svc.Gift(context.Background(), 1, 2, 300, "k")
	for range GiftMaxCreditAttempts * 3 {
		h.clock = h.clock.Add(giftRetryCap + time.Second)
		if _, err := h.svc.RetryDueGifts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	g, _ := h.GiftByKey(context.Background(), "k")
	if g.Status != enum.GiftDebited || h.refundsRecorded != 0 {
		t.Fatalf("status=%s refunds=%d, want DEBITED/0（錢卡在途中好過兩邊都拿到）", g.Status, h.refundsRecorded)
	}
}

// 防線二（單獨成立）：確定性失敗、次數用盡，但入帳流水其實已存在 → 不退款，改排重試。
// 假帳務讓入帳**一律**回樂觀鎖衝突（即使流水已在），所以只有 CreditLanded 擋得住退款；
// 拿掉那個檢查，這個測試會紅。
func TestGiftDoesNotRefundWhenCreditAlreadyLanded(t *testing.T) {
	h := newGiftHarness(map[int64]int64{1: 1000, 2: 0})
	h.svc.repo = &alwaysConflictOnGiftCredit{giftWorld: h.giftWorld}
	// 別的副本已把入帳做完（流水存在），但這個副本一路只看到衝突。
	h.ledger["k:gift:credit"] = ledgerRow{txID: 99, playerID: 2, amount: 300, after: 300}

	_, _ = h.svc.Gift(context.Background(), 1, 2, 300, "k")
	for range GiftMaxCreditAttempts * 2 {
		h.clock = h.clock.Add(giftRetryCap + time.Second)
		if _, err := h.svc.RetryDueGifts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if h.refundsRecorded != 0 {
		t.Fatalf("入帳流水已存在卻退款了 %d 次", h.refundsRecorded)
	}
	if g, _ := h.GiftByKey(context.Background(), "k"); g.Status == enum.GiftRefunded || g.Status == enum.GiftRefunding {
		t.Fatalf("status = %s，不該走到退款", g.Status)
	}
}

type alwaysConflictOnGiftCredit struct{ *giftWorld }

func (a *alwaysConflictOnGiftCredit) Credit(ctx context.Context, m domain.Movement) (repository.CreditResult, error) {
	if m.SubType == domain.SubTypeGift {
		return repository.CreditResult{}, repository.ErrConcurrentModification
	}
	return a.giftWorld.Credit(ctx, m)
}
