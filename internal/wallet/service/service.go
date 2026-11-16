// Package service 是 wallet 的應用層：把 store 的 sentinel error 分類成
// 「對外有意義的結果碼」，並在**錯誤發生的地方**記錄一次日誌。
//
// 分層（docs/ADR-004）：handler → service → store。
//   - handler 認得 HTTP（狀態碼、信封、JSON），不認得 SQL 的錯誤。
//   - service 認得帳務的結果（錢包不存在、餘額不足…），不認得 HTTP。
//   - store 認得 SQL，不認得誰在呼叫它。
//
// ⭐ 為什麼這一層不是多餘的：舊的 httpapi 把「sentinel → 狀態碼」與
// 「該不該記日誌、記幾級」寫在同一個函式裡。換掉 HTTP 框架時那段邏輯得跟著
// 重寫——而它與 HTTP 無關（gRPC 入口也需要同一份分類）。拆開之後，換框架只動 handler。
package service

import (
	"context"
	"log/slog"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// Store 是這一層需要的帳務能力，定義在消費端（CLAUDE.md §2）。
// *walletstore.Repository 不知道這個介面存在。
type Store interface {
	Debit(ctx context.Context, m domain.Movement) (walletstore.DebitResult, error)
	Credit(ctx context.Context, m domain.Movement) (walletstore.CreditResult, error)
}

// Service 是 wallet 的應用服務。
//
// New 回傳具體型別而不是介面：介面由消費端（handler）宣告，
// 這是與 go-layered-arch skill 「provider 回傳 interface」刻意不同的地方
// （CLAUDE.md §2、docs/ADR-004）。
type Service struct {
	store  Store
	logger *slog.Logger
}

// New 組出 Service。logger 為 nil 時退回 slog.Default()。
func New(store Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, logger: logger}
}
