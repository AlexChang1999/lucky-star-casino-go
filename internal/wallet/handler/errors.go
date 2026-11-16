package handler

import (
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/apiprotocol"

	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

// respondValidationError 把驗證錯誤翻成 400。
func (h *Handler) respondValidationError(c fiber.Ctx, err error) error {
	var fe fieldError
	if errors.As(err, &fe) {
		return respondError(c, fiber.StatusBadRequest, fe.Error())
	}
	// 走到這裡代表 domain 多了一個 sentinel、而 validationError 沒跟上。
	// 回 500 是刻意的失敗方向：它會被看到、會被記錄，而不是靜靜地變成看起來合理的 400。
	h.logger.Error("驗證錯誤沒有對應的欄位訊息（domain 是不是多了一個 sentinel？）", "err", err)
	return respondError(c, fiber.StatusInternalServerError, apiprotocol.InternalError)
}

// respondServiceError 把 service 的結果碼翻成 Java 版的狀態碼與訊息。
// 日誌已經由 service 在錯誤起點記過了，這裡**不重複記**。
//
// ⚠️ **餘額不足是 422 不是 400**：Java 的 `@ResponseStatus(UNPROCESSABLE_ENTITY)`
// 寫在 GlobalExceptionHandler:23-27。JSON 語法對、欄位合法，是**業務狀態**不允許——
// 那正是 422 與 400 的分界。
func (h *Handler) respondServiceError(c fiber.Ctx, err error, m domain.Movement) error {
	switch service.CodeOf(err) {
	case service.CodeWalletNotFound:
		// 訊息帶請求的 playerID，逐字對齊 Java：`"Wallet not found for player: " + id`。
		return respondError(c, fiber.StatusNotFound,
			apiprotocol.WalletNotFound(m.PlayerID))
	case service.CodeInsufficientBalance:
		return respondError(c, fiber.StatusUnprocessableEntity, apiprotocol.InsufficientBalance)
	case service.CodeAidBalanceTooHigh:
		return respondError(c, fiber.StatusUnprocessableEntity, apiprotocol.AidBalanceTooHigh)
	case service.CodeAidAlreadyClaimed:
		return respondError(c, fiber.StatusUnprocessableEntity, apiprotocol.AidAlreadyClaimed)
	case service.CodeConcurrentModification:
		// 訊息是 Java 寫死的那一句（GlobalExceptionHandler:131），不是 ex.getMessage()。
		return respondError(c, fiber.StatusConflict, apiprotocol.ConcurrentModify)
	default:
		return respondError(c, fiber.StatusInternalServerError, apiprotocol.InternalError)
	}
}
