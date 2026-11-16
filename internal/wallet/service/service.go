// Package service 是 wallet 的應用層：把 repository 的 sentinel error 分類成
// 「對外有意義的結果碼」，並在**錯誤發生的地方**記錄一次日誌。
//
// 分層（docs/ADR-004）：handler → service → repository。
//   - handler 認得 HTTP（狀態碼、信封、JSON），不認得 SQL 的錯誤。
//   - service 認得帳務的結果（錢包不存在、餘額不足…），不認得 HTTP。
//   - repository 認得 SQL，不認得誰在呼叫它。
//
// ⭐ 為什麼這一層不是多餘的：舊的 httpapi 把「sentinel → 狀態碼」與
// 「該不該記日誌、記幾級」寫在同一個函式裡。換掉 HTTP 框架時那段邏輯得跟著
// 重寫——而它與 HTTP 無關（gRPC 入口也需要同一份分類）。拆開之後，換框架只動 handler。
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/enum"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/repository"
)

// Repository 是這一層需要的帳務能力，定義在消費端（CLAUDE.md §2）。
// *repository.Repository 不知道這個介面存在。
type Repository interface {
	Debit(ctx context.Context, m domain.Movement) (repository.DebitResult, error)
	Credit(ctx context.Context, m domain.Movement) (repository.CreditResult, error)
	// Balance 回傳**總餘額**（不是可用餘額），錢包不存在回 repository.ErrWalletNotFound。
	Balance(ctx context.Context, playerID int64) (int64, error)
}

// Quota 是這一層需要的 Redis 額度能力，定義在消費端（CLAUDE.md §2）。
// *quota.Store 不知道這個介面存在。
type Quota interface {
	ClaimBankruptcyAidAt(ctx context.Context, playerID int64, now time.Time) error
	ReleaseBankruptcyAid(ctx context.Context, playerID int64)
	// ReserveGiftQuota 超限時回 quota.ErrGiftSentLimitExceeded / ErrGiftRecvLimitExceeded（已回補完畢）。
	ReserveGiftQuota(ctx context.Context, senderID, receiverID int64, amount domain.Amount) error
	ReleaseGiftQuota(ctx context.Context, senderID, receiverID int64, amount domain.Amount)
}

// GiftRepository 是贈禮狀態單的存取能力，定義在消費端（CLAUDE.md §2）。
// 與 Repository 分開，是因為只有贈禮用得到它——debit / credit 的測試替身不必跟著長出十個方法。
//
// 回傳 bool 的方法都是條件式轉移：false 代表「別人已經先轉走了」，不是錯誤。
type GiftRepository interface {
	InsertGift(ctx context.Context, key string, senderID, receiverID int64, amount domain.Amount, nextAttemptAt time.Time) (repository.GiftTransfer, error)
	GiftByKey(ctx context.Context, key string) (repository.GiftTransfer, error)
	GiftByID(ctx context.Context, id int64) (repository.GiftTransfer, error)
	DeletePendingGift(ctx context.Context, id int64) (bool, error)
	MarkGiftDebited(ctx context.Context, id, debitTxID, senderBalanceAfter int64, nextAttemptAt time.Time) (bool, error)
	MarkGiftCompleted(ctx context.Context, id, creditTxID, receiverBalanceAfter int64) (bool, error)
	MarkGiftRefunding(ctx context.Context, id int64, nextAttemptAt time.Time, lastErr string) (bool, error)
	MarkGiftRefunded(ctx context.Context, id, senderBalanceAfter int64) (bool, error)
	ScheduleGiftRetry(ctx context.Context, id int64, status enum.GiftStatus, attempts int, nextAttemptAt time.Time, lastErr string) (bool, error)
	DueGifts(ctx context.Context, now time.Time, limit int) ([]repository.GiftTransfer, error)
	// CreditLanded 回答「這把入帳冪等鍵的流水是否已經存在」。轉退款前用它確認入帳沒有悄悄成功。
	CreditLanded(ctx context.Context, idempotencyKey string) (bool, error)
}

// Service 是 wallet 的應用服務。
//
// New 回傳具體型別而不是介面：介面由消費端（handler）宣告，
// 這是與 go-layered-arch skill 「provider 回傳 interface」刻意不同的地方
// （CLAUDE.md §2、docs/ADR-004）。
type Service struct {
	repo   Repository
	gifts  GiftRepository
	quota  Quota
	logger *slog.Logger

	// now 讓測試能把「現在」釘在台北與 UTC 不同天的位置。正式路徑一律 time.Now。
	now func() time.Time
}

// New 組出 Service。logger 為 nil 時退回 slog.Default()。
func New(repo Repository, gifts GiftRepository, quota Quota, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, gifts: gifts, quota: quota, logger: logger, now: time.Now}
}
