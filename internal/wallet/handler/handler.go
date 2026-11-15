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

import "log/slog"

// Handler 持有這一層的依賴。業務端點各自在 debit.go / credit.go 掛上來。
type Handler struct {
	logger *slog.Logger
}

// New 組出 Handler。logger 為 nil 時退回 slog.Default()。
func New(logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{logger: logger}
}
