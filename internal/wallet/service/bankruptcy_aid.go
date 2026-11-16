package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/quota"
)

const (
	// BankruptcyBalanceThreshold：**總餘額**低於此值才符合資格（Java `BALANCE_THRESHOLD`）。
	BankruptcyBalanceThreshold int64 = 100
	// BankruptcyAidAmount 是每次發放的星幣（Java `AID_AMOUNT`）。
	BankruptcyAidAmount int64 = 1_000
)

// 兩個 sentinel 只給 Error.Err 與日誌用；對外訊息由 handler 依 Code 決定。
var (
	errAidBalanceTooHigh = errors.New("餘額未低於破產救助門檻")
	errAidAlreadyClaimed = errors.New("今天已領過破產救助")
)

// BankruptcyAidResult 是一次破產救助的結果。
type BankruptcyAidResult struct {
	PlayerID      int64
	Amount        int64
	TransactionID int64
	BalanceBefore int64
	BalanceAfter  int64
}

// BankruptcyAid 對應 Java 的 `BankruptcyAidService.claim`，四步順序不可調換：
//
//  1. 資格：總餘額 < 門檻（錢包不存在 → 404）
//  2. Redis 當日鎖（搶不到 → 已領過）
//  3. 入帳，冪等鍵 `bankruptcy-aid:{id}:{date}`；失敗要釋放鎖讓玩家可重試
//  4. 冪等命中 → 視為已領過，**保留**鎖、不重複加錢
//
// ⚠️ Redis 失敗一律 fail-closed（回 CodeInternal，不入帳）。放行在帳務上其實安全
// （第 3 步的 UNIQUE 索引是第二道防線），但 Java 在那裡回 500，改掉是對外行為漂移。
//
// ⚠️ 不重試樂觀鎖衝突（地雷 #36）：409 往外傳，由呼叫端決定是否重來。
func (s *Service) BankruptcyAid(ctx context.Context, playerID int64) (BankruptcyAidResult, error) {
	// 先組好入帳意圖再去搶鎖：意圖不合法時不該留下一把鎖。
	// 只讀一次時鐘，鎖的日期與冪等鍵的日期才保證是同一天。
	now := s.now()
	m, err := domain.NewCredit(playerID, domain.Amount(BankruptcyAidAmount), domain.SubTypeBankruptcyAid,
		fmt.Sprintf("bankruptcy-aid:%d:%s", playerID, quota.DayStamp(now)), "", 0)
	if err != nil {
		s.logger.Error("破產救助的入帳意圖不合法（程式錯誤）", "playerID", playerID, "err", err)
		return BankruptcyAidResult{}, &Error{Code: CodeInternal, Err: err}
	}

	balance, err := s.repo.Balance(ctx, playerID)
	if err != nil {
		return BankruptcyAidResult{}, s.classify(err, m)
	}
	if balance >= BankruptcyBalanceThreshold {
		return BankruptcyAidResult{}, &Error{Code: CodeAidBalanceTooHigh, Err: errAidBalanceTooHigh}
	}

	if err := s.quota.ClaimBankruptcyAidAt(ctx, playerID, now); err != nil {
		if errors.Is(err, quota.ErrAlreadyClaimedToday) {
			return BankruptcyAidResult{}, &Error{Code: CodeAidAlreadyClaimed, Err: errAidAlreadyClaimed}
		}
		s.logger.Error("搶破產救助當日鎖失敗（fail-closed，不入帳）", "playerID", playerID, "err", err)
		return BankruptcyAidResult{}, &Error{Code: CodeInternal, Err: err}
	}

	r, err := s.Credit(ctx, m)
	if err != nil {
		s.quota.ReleaseBankruptcyAid(ctx, playerID)
		return BankruptcyAidResult{}, err
	}
	if r.Idempotent {
		return BankruptcyAidResult{}, &Error{Code: CodeAidAlreadyClaimed, Err: errAidAlreadyClaimed}
	}

	return BankruptcyAidResult{
		PlayerID:      playerID,
		Amount:        BankruptcyAidAmount,
		TransactionID: r.TransactionID,
		BalanceBefore: r.BalanceBefore,
		BalanceAfter:  r.BalanceAfter,
	}, nil
}
