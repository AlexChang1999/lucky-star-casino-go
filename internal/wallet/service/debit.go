package service

import (
	"context"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// DebitResult 是扣款的結果（業務物件，不帶 JSON tag——對外形狀是 handler 的事）。
type DebitResult struct {
	TransactionID int64
	PlayerID      int64
	Amount        int64
	BalanceBefore int64
	BalanceAfter  int64
	Idempotent    bool
}

// Debit 執行一次已驗證的扣款意圖。失敗時回 *Error（見 classify）。
//
// ⚠️ 結果裡的 PlayerID / Amount 取自 **store 的回傳**而不是請求：冪等命中時 Java 回的是
// **原交易**的值（toIdempotentResponse:135-142），冪等鍵跨玩家碰撞時兩者會不同——
// 那是刻意保留的診斷訊號，不是 bug。
func (s *Service) Debit(ctx context.Context, m domain.Movement) (DebitResult, error) {
	r, err := s.store.Debit(ctx, m)
	if err != nil {
		return DebitResult{}, s.classify(err, m)
	}
	return DebitResult{
		TransactionID: r.TransactionID,
		PlayerID:      r.PlayerID,
		Amount:        int64(r.Amount),
		BalanceBefore: int64(r.BalanceBefore),
		BalanceAfter:  int64(r.BalanceAfter),
		Idempotent:    r.Idempotent,
	}, nil
}
