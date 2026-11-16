package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

type fakeStore struct {
	debitResult walletstore.DebitResult
	debitErr    error

	creditResult walletstore.CreditResult
}

func (f *fakeStore) Debit(context.Context, domain.Movement) (walletstore.DebitResult, error) {
	return f.debitResult, f.debitErr
}

func (f *fakeStore) Credit(context.Context, domain.Movement) (walletstore.CreditResult, error) {
	return f.creditResult, nil
}

func newTestService(st Store) *Service {
	return New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestDebitClassifiesStoreErrors 釘住 sentinel → Code 的分類。
// 用 fmt.Errorf("%w") 包一層，因為 store 實際回的就是被包過的錯誤（errors.Is 要穿得過去）。
func TestDebitClassifiesStoreErrors(t *testing.T) {
	tests := []struct {
		name     string
		storeErr error
		want     Code
	}{
		{"錢包不存在", fmt.Errorf("%w: playerID=42", walletstore.ErrWalletNotFound), CodeWalletNotFound},
		{"餘額不足", fmt.Errorf("%w: need 100", walletstore.ErrInsufficientBalance), CodeInsufficientBalance},
		{"樂觀鎖衝突", fmt.Errorf("%w: v=3", walletstore.ErrConcurrentModification), CodeConcurrentModification},
		// 贏家不見是 Java 的 IllegalStateException → 500，不可被歸類成任何 4xx。
		{"冪等贏家不見", fmt.Errorf("%w: key=k", walletstore.ErrIdempotencyWinnerMissing), CodeInternal},
		{"未預期錯誤", errors.New("connection refused"), CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestService(&fakeStore{debitErr: tt.storeErr})
			_, err := s.Debit(context.Background(), domain.Movement{PlayerID: 42})
			if got := CodeOf(err); got != tt.want {
				t.Errorf("Code = %v, want %v", got, tt.want)
			}
			// 原始錯誤必須還能被 errors.Is 找到（log 與除錯靠它）。
			if !errors.Is(err, tt.storeErr) {
				t.Errorf("service.Error 沒有保留原始錯誤: %v", err)
			}
		})
	}
}

// TestDebitResultComesFromStore 守「結果取自 store 而不是請求」：
// 冪等命中時 PlayerID/Amount 是原交易的值，與請求不同是刻意保留的診斷訊號。
func TestDebitResultComesFromStore(t *testing.T) {
	s := newTestService(&fakeStore{debitResult: walletstore.DebitResult{
		TransactionID: 7, PlayerID: 99, Amount: 50, BalanceBefore: 1000, BalanceAfter: 950, Idempotent: true,
	}})
	got, err := s.Debit(context.Background(), domain.Movement{PlayerID: 42, Amount: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := DebitResult{TransactionID: 7, PlayerID: 99, Amount: 50, BalanceBefore: 1000, BalanceAfter: 950, Idempotent: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestCreditFrozenAfterKeepsNil 守地雷 #33：store 回 nil（冪等命中）時，service 不可把它
// 變成指向 0 的指標——0 是合法的凍結金額，null 才是「沒有這個資訊」。
func TestCreditFrozenAfterKeepsNil(t *testing.T) {
	frozen := domain.Amount(250)
	tests := []struct {
		name string
		in   *domain.Amount
		want *int64
	}{
		{"nil 維持 nil", nil, nil},
		{"有值就帶值", &frozen, func() *int64 { v := int64(250); return &v }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestService(&fakeStore{creditResult: walletstore.CreditResult{FrozenAfter: tt.in}})
			got, err := s.Credit(context.Background(), domain.Movement{})
			if err != nil {
				t.Fatal(err)
			}
			if (got.FrozenAfter == nil) != (tt.want == nil) || (tt.want != nil && *got.FrozenAfter != *tt.want) {
				t.Errorf("FrozenAfter = %v, want %v", got.FrozenAfter, tt.want)
			}
		})
	}
}
