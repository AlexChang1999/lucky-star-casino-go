// Package handler 是 wallet 的 HTTP 邊界（Fiber）。
//
// 這一層**很薄，而且必須保持很薄**：帳務語義在 internal/wallet/store，
// 驗證規則在 internal/wallet/domain，錯誤分類在 internal/wallet/service。
// 這裡只做三件事——解 JSON、把 service 的結果碼翻成狀態碼、包成 Java 版的回應信封。
//
// ⭐ 為什麼「翻成狀態碼」值得獨立一層：Go 的 error 是**分類**，HTTP 狀態碼是
// **對外契約**，兩者一對一但不是同一件事。內部錯誤訊息是中文、帶 playerID 與冪等鍵，
// 方便查問題；對外訊息必須逐字等於 Java 版，因為契約測試會 diff 它。
//
// ⚠️ 端點路徑、信封、狀態碼、訊息文字全部照抄 Java 版（`InternalWalletController`、
// `GlobalExceptionHandler`、`ApiResponse`、`InternalSecretFilter`）。不要照直覺改。
package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

// WalletService 是這一層需要的應用能力，定義在消費端（CLAUDE.md §2）。
// *service.Service 不知道這個介面存在。
type WalletService interface {
	Debit(ctx context.Context, m domain.Movement) (service.DebitResult, error)
	Credit(ctx context.Context, m domain.Movement) (service.CreditResult, error)
	BankruptcyAid(ctx context.Context, playerID int64) (service.BankruptcyAidResult, error)
}

// Handler 持有這一層的依賴。沒有第三個——有第三個就代表有邏輯跑錯層了。
type Handler struct {
	svc    WalletService
	logger *slog.Logger
}

// New 組出 Handler。logger 為 nil 時退回 slog.Default()。
func New(svc WalletService, logger *slog.Logger) (*Handler, error) {
	if svc == nil {
		return nil, errors.New("handler.New: svc 不可為 nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, logger: logger}, nil
}
