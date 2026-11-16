package handler

import (
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/output"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/apiprotocol"

	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// BankruptcyAid 對應 Java 的 `POST /api/v1/wallet/bankruptcy-aid`。
//
// 玩家身分只來自 gateway 注入的 X-User-Id（只能領自己的），沒有 request body。
func (h *Handler) BankruptcyAid(c fiber.Ctx) error {
	raw := c.Get("X-User-Id")
	if strings.TrimSpace(raw) == "" {
		return respondError(c, fiber.StatusBadRequest, apiprotocol.MissingUserID)
	}
	// Java 的 Long.parseLong 不會先 trim，" 7" 是 NumberFormatException；ParseInt 同樣嚴格。
	playerID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return respondError(c, fiber.StatusBadRequest, apiprotocol.InvalidUserID)
	}

	r, err := h.svc.BankruptcyAid(c.Context(), playerID) // ⚠️ c.Context() 不是 c（ADR-004）
	if err != nil {
		// 錢包不存在的訊息要帶 playerID，所以借用 Movement 的形狀傳進去。
		return h.respondServiceError(c, err, domain.Movement{PlayerID: playerID})
	}

	return respondOK(c, output.BankruptcyAidRes{
		PlayerID:      r.PlayerID,
		Amount:        r.Amount,
		TransactionID: r.TransactionID,
		BalanceBefore: r.BalanceBefore,
		BalanceAfter:  r.BalanceAfter,
	})
}
