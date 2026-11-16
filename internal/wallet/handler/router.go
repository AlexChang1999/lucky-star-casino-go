package handler

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

// NewApp 組出 Fiber 應用：中介層 → 路由。這個檔只放「路徑對到哪個 handler」，沒有邏輯。
//
// ⚠️ internalSecret 為空字串一律回錯誤。空 secret 會讓 ConstantTimeCompare 對
// 「同樣沒帶 header」的請求回 1，於是 `/internal/**` 對全世界敞開，
// 而服務、日誌、健康檢查看起來全部正常——「設定漏了卻跑得起來」正是本專案最想避免的形狀。
func NewApp(h *Handler, internalSecret string, cfg config.HTTPServer) (*fiber.App, error) {
	if h == nil {
		return nil, errors.New("handler.NewApp: handler 不可為 nil")
	}
	if internalSecret == "" {
		return nil, errors.New("handler.NewApp: internalSecret 不可為空——空值等於 /internal/** 沒有保護")
	}

	app := fiber.New(fiber.Config{
		// ⚠️ 逾時全部顯式設定，理由同舊版（零值是「永不逾時」）。
		// fasthttp 沒有 ReadHeaderTimeout，慢速 header 攻擊改由 ReadTimeout 兜底（docs/ADR-004）。
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		ErrorHandler: errorHandler(h.logger),
	})

	// ⚠️ 順序：recovery 在最外層才接得住後面所有人的 panic；
	// secret 在 logger 之後，這樣被拒絕的請求也有存取日誌。
	app.Use(recovery(h.logger), requestLogger(h.logger), requireInternalSecret(h.logger, internalSecret))

	// ⚠️ 具體路徑必須排在 catch-all 之前（AGENTS.md 地雷 #13）。目前沒有 catch-all。
	app.Get("/healthz", h.Health)
	app.Post("/internal/wallet/debit", h.Debit)
	app.Post("/internal/wallet/credit", h.Credit)
	app.Post("/api/v1/wallet/bankruptcy-aid", h.BankruptcyAid)
	app.Post("/api/v1/wallet/gift", h.Gift)

	return app, nil
}

// errorHandler 處理「沒有 handler 接住」的錯誤：路由不存在（404）、方法不允許（405）等
// *fiber.Error，一律包成信封，訊息沿用 Fiber 的文字；其餘當 500，且不外洩細節。
func errorHandler(logger *slog.Logger) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		var fe *fiber.Error
		if errors.As(err, &fe) {
			return respondError(c, fe.Code, fe.Message)
		}
		logger.Error("未處理的錯誤", "path", c.Path(), "err", err)
		return respondError(c, fiber.StatusInternalServerError, internalErrorMessage)
	}
}
