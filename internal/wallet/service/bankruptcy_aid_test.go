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
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/quota"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/repository"
)

type fakeQuota struct {
	claimErr   error
	claimed    int
	claimedAt  time.Time
	released   int
	releasedID int64

	giftErr      error
	giftReserved int
	giftReleased int
}

func (f *fakeQuota) ClaimBankruptcyAidAt(_ context.Context, _ int64, now time.Time) error {
	f.claimed++
	f.claimedAt = now
	return f.claimErr
}

func (f *fakeQuota) ReserveGiftQuota(context.Context, int64, int64, domain.Amount) error {
	f.giftReserved++
	return f.giftErr
}

func (f *fakeQuota) ReleaseGiftQuota(context.Context, int64, int64, domain.Amount) {
	f.giftReleased++
}

func (f *fakeQuota) ReleaseBankruptcyAid(_ context.Context, playerID int64) {
	f.released++
	f.releasedID = playerID
}

// aidService 組出一個時鐘固定的 Service。
// 2026-11-16T17:00:00Z = 台北 2026-11-17 01:00——UTC 與台北的「今天」刻意不同天，
// 日界用錯時區的話冪等鍵的日期會對不上。
func aidService(st *fakeRepository, q *fakeQuota) *Service {
	s := New(st, nil, q, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return time.Date(2026, 11, 16, 17, 0, 0, 0, time.UTC) }
	return s
}

func TestBankruptcyAidEligibility(t *testing.T) {
	notFound := fmt.Errorf("%w: playerID=7", repository.ErrWalletNotFound)
	tests := []struct {
		name        string
		balance     int64
		balanceErr  error
		claimErr    error
		creditErr   error
		idempotent  bool
		wantCode    Code
		wantOK      bool
		wantClaims  int
		wantCredits int
		wantRelease int
	}{
		{name: "餘額 99 → 發放", balance: 99, wantOK: true, wantClaims: 1, wantCredits: 1},
		{name: "餘額 0 → 發放", balance: 0, wantOK: true, wantClaims: 1, wantCredits: 1},
		// 門檻是「< 100」：剛好 100 不符資格（Java: >= BALANCE_THRESHOLD 就拒絕）。
		{name: "餘額剛好 100 → 422，不碰 Redis", balance: 100, wantCode: CodeAidBalanceTooHigh},
		{name: "餘額很高 → 422", balance: 5000, wantCode: CodeAidBalanceTooHigh},
		{name: "錢包不存在 → 404", balanceErr: notFound, wantCode: CodeWalletNotFound},
		{name: "今天領過 → 422，不入帳", balance: 10, claimErr: quota.ErrAlreadyClaimedToday,
			wantCode: CodeAidAlreadyClaimed, wantClaims: 1},
		// ⚠️ fail-closed：Redis 掛了不可放行（quota 套件說明；Java 是例外往外拋成 500）。
		{name: "Redis 掛了 → 500，不入帳", balance: 10, claimErr: errors.New("connection refused"),
			wantCode: CodeInternal, wantClaims: 1},
		{name: "入帳失敗 → 釋放當日鎖並保留原因", balance: 10,
			creditErr: fmt.Errorf("%w: v=1", repository.ErrConcurrentModification),
			wantCode:  CodeConcurrentModification, wantClaims: 1, wantCredits: 1, wantRelease: 1},
		// ⭐ 冪等命中代表 Redis 曾被清空但 DB 已有當日紀錄：視為領過，且**保留**鎖。
		{name: "冪等命中 → 視為已領過，不釋放鎖", balance: 10, idempotent: true,
			wantCode: CodeAidAlreadyClaimed, wantClaims: 1, wantCredits: 1, wantRelease: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeRepository{
				balance: tt.balance, balanceErr: tt.balanceErr, creditErr: tt.creditErr,
				creditResult: repository.CreditResult{
					TransactionID: 9, PlayerID: 7, Amount: 1000,
					BalanceBefore: domain.Amount(tt.balance), BalanceAfter: domain.Amount(tt.balance + 1000),
					Idempotent: tt.idempotent,
				},
			}
			q := &fakeQuota{claimErr: tt.claimErr}
			res, err := aidService(st, q).BankruptcyAid(context.Background(), 7)

			if tt.wantOK {
				if err != nil {
					t.Fatalf("不該失敗: %v", err)
				}
				want := BankruptcyAidResult{PlayerID: 7, Amount: 1000, TransactionID: 9,
					BalanceBefore: tt.balance, BalanceAfter: tt.balance + 1000}
				if res != want {
					t.Errorf("結果 = %+v, want %+v", res, want)
				}
			} else if got := CodeOf(err); got != tt.wantCode {
				t.Fatalf("Code = %v, want %v（err=%v）", got, tt.wantCode, err)
			}
			if q.claimed != tt.wantClaims {
				t.Errorf("搶鎖次數 = %d, want %d", q.claimed, tt.wantClaims)
			}
			if len(st.gotCredits) != tt.wantCredits {
				t.Errorf("入帳次數 = %d, want %d", len(st.gotCredits), tt.wantCredits)
			}
			if q.released != tt.wantRelease {
				t.Errorf("釋放鎖次數 = %d, want %d", q.released, tt.wantRelease)
			}
		})
	}
}

// TestBankruptcyAidCreditIntent 釘住送進 repository 的那筆入帳長什麼樣：金額、子類型、
// 以及冪等鍵的日期必須是**台北**的今天，並且與搶鎖用的是同一個時刻。
func TestBankruptcyAidCreditIntent(t *testing.T) {
	st := &fakeRepository{balance: 0}
	q := &fakeQuota{}
	if _, err := aidService(st, q).BankruptcyAid(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	m := st.gotCredits[0]
	if m.PlayerID != 7 || int64(m.Amount) != 1000 || m.SubType != domain.SubTypeBankruptcyAid {
		t.Errorf("入帳意圖 = %+v", m)
	}
	if want := "bankruptcy-aid:7:2026-11-17"; m.IdempotencyKey != want {
		t.Errorf("冪等鍵 = %q, want %q", m.IdempotencyKey, want)
	}
	if !q.claimedAt.Equal(time.Date(2026, 11, 16, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("搶鎖與組冪等鍵必須用同一個時刻，claimedAt = %v", q.claimedAt)
	}
}

// 釋放鎖要釋放的是同一個玩家，不是別人。
func TestBankruptcyAidReleasesOwnPlayer(t *testing.T) {
	st := &fakeRepository{balance: 0, creditErr: errors.New("boom")}
	q := &fakeQuota{}
	_, _ = aidService(st, q).BankruptcyAid(context.Background(), 7)
	if q.released != 1 || q.releasedID != 7 {
		t.Errorf("released=%d id=%d", q.released, q.releasedID)
	}
}
