package handler

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// 對外訊息，逐字對齊 Java 的 WalletController 與 BankruptcyAidService。
const (
	missingUserIDMessage     = "Missing X-User-Id header"
	invalidUserIDMessage     = "Invalid X-User-Id header"
	aidBalanceTooHighMessage = "Balance not below threshold (100), not eligible for bankruptcy aid"
	aidAlreadyClaimedMessage = "Bankruptcy aid already claimed today"
)

// ⚠️ 欄位順序刻意與 Java 的 BankruptcyAidResponse 一致。
type bankruptcyAidResponse struct {
	PlayerID      int64 `json:"playerId"`
	Amount        int64 `json:"amount"`
	TransactionID int64 `json:"transactionId"`
	BalanceBefore int64 `json:"balanceBefore"`
	BalanceAfter  int64 `json:"balanceAfter"`
}

// BankruptcyAid 對應 Java 的 `POST /api/v1/wallet/bankruptcy-aid`。
//
// 玩家身分只來自 gateway 注入的 X-User-Id（只能領自己的），沒有 request body。
func (h *Handler) BankruptcyAid(c fiber.Ctx) error {
	raw := c.Get("X-User-Id")
	if strings.TrimSpace(raw) == "" {
		return respondError(c, fiber.StatusBadRequest, missingUserIDMessage)
	}
	// Java 的 Long.parseLong 不會先 trim，" 7" 是 NumberFormatException；ParseInt 同樣嚴格。
	playerID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return respondError(c, fiber.StatusBadRequest, invalidUserIDMessage)
	}

	r, err := h.svc.BankruptcyAid(c.Context(), playerID) // ⚠️ c.Context() 不是 c（ADR-004）
	if err != nil {
		// 錢包不存在的訊息要帶 playerID，所以借用 Movement 的形狀傳進去。
		return h.respondServiceError(c, err, domain.Movement{PlayerID: playerID})
	}

	return respondOK(c, bankruptcyAidResponse{
		PlayerID:      r.PlayerID,
		Amount:        r.Amount,
		TransactionID: r.TransactionID,
		BalanceBefore: r.BalanceBefore,
		BalanceAfter:  r.BalanceAfter,
	})
}
