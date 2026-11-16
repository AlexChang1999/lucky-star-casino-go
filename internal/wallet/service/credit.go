package service

import (
	"context"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// CreditResult 是入帳的結果。
type CreditResult struct {
	TransactionID int64
	PlayerID      int64
	Amount        int64
	BalanceBefore int64
	BalanceAfter  int64
	// ⚠️ *int64 而不是 int64（地雷 #33）：冪等命中時 Java 明確回 null
	// （「不重算凍結；以當初入帳結果為準」，WalletService.java:179、:229）。
	// 用 int64 的話那個 null 會靜靜變成 0——而 0 是一個合法的凍結金額。
	FrozenAfter *int64
	Idempotent  bool
}

// Credit 執行一次已驗證的入帳意圖。失敗時回 *Error（見 classify）。
//
// ⚠️ 這裡**不重試**樂觀鎖衝突（地雷 #36）：重試權在知道冪等鍵怎麼來的那一層，
// 而且悄悄重掉會讓呼叫端從此再也看不到 409。
func (s *Service) Credit(ctx context.Context, m domain.Movement) (CreditResult, error) {
	r, err := s.repo.Credit(ctx, m)
	if err != nil {
		return CreditResult{}, s.classify(err, m)
	}
	res := CreditResult{
		TransactionID: r.TransactionID,
		PlayerID:      r.PlayerID,
		Amount:        int64(r.Amount),
		BalanceBefore: int64(r.BalanceBefore),
		BalanceAfter:  int64(r.BalanceAfter),
		Idempotent:    r.Idempotent,
	}
	if r.FrozenAfter != nil {
		frozen := int64(*r.FrozenAfter)
		res.FrozenAfter = &frozen
	}
	return res, nil
}
