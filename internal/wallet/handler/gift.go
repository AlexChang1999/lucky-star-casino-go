package handler

import (
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/output"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/input"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/apiprotocol"

	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/quota"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

// maxGiftKeyLen 對應 Java GiftRequest 的 `@Size(max = 80)`。
// 80 + 衍生後綴 `:gift:credit`（12）剛好塞進 wallet_transactions.idempotency_key 的 VARCHAR(100)。
const maxGiftKeyLen = 80

// validate 跑 Java GiftRequest 的 Bean Validation（@NotNull / @Positive / @NotBlank / @Size）。
// 順序與 DTO 欄位宣告順序一致。
func validateGift(r input.GiftReq) error {
	if r.ReceiverID == nil {
		return fieldError{field: "receiverId", message: apiprotocol.MustNotBeNull}
	}
	if r.Amount == nil {
		return fieldError{field: "amount", message: apiprotocol.MustNotBeNull}
	}
	if *r.Amount <= 0 {
		return fieldError{field: "amount", message: apiprotocol.MustBePositive}
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return fieldError{field: "idempotencyKey", message: apiprotocol.MustNotBeBlank}
	}
	// 數字元不是位元組，理由同 checkSize。
	if len([]rune(r.IdempotencyKey)) > maxGiftKeyLen {
		return fieldError{field: "idempotencyKey", message: apiprotocol.SizeBetween(maxGiftKeyLen)}
	}
	return nil
}

// Gift 對應 Java 的 `POST /api/v1/wallet/gift`。送出方只來自 gateway 注入的 X-User-Id。
func (h *Handler) Gift(c fiber.Ctx) error {
	raw := c.Get("X-User-Id")
	if strings.TrimSpace(raw) == "" {
		return respondError(c, fiber.StatusBadRequest, apiprotocol.MissingUserID)
	}
	senderID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return respondError(c, fiber.StatusBadRequest, apiprotocol.InvalidUserID)
	}

	var req input.GiftReq
	if err := c.Bind().JSON(&req); err != nil {
		return respondError(c, fiber.StatusBadRequest, apiprotocol.InvalidRequestBody)
	}
	if err := validateGift(req); err != nil {
		return h.respondValidationError(c, err)
	}

	r, err := h.svc.Gift(c.Context(), senderID, *req.ReceiverID, *req.Amount, req.IdempotencyKey) // ⚠️ c.Context()（ADR-004）
	if err != nil {
		return h.respondGiftError(c, err, senderID, *req.ReceiverID)
	}

	return respondOK(c, output.GiftRes{
		SenderID:             r.SenderID,
		ReceiverID:           r.ReceiverID,
		Amount:               r.Amount,
		DebitTransactionID:   r.DebitTransactionID,
		CreditTransactionID:  r.CreditTransactionID,
		SenderBalanceAfter:   r.SenderBalanceAfter,
		ReceiverBalanceAfter: r.ReceiverBalanceAfter,
		Idempotent:           r.Idempotent,
	})
}

// respondGiftError 處理贈禮專屬的結果碼；其餘（餘額不足、樂觀鎖…）交回共用的翻譯。
//
// ⚠️ 兩個 Go 版新增的結果碼（Java 沒有對應物，因為 Java 是單一交易、不會有「已退款」）：
// 已退款與鍵衝突都回 409。
func (h *Handler) respondGiftError(c fiber.Ctx, err error, senderID, receiverID int64) error {
	switch service.CodeOf(err) {
	case service.CodeInvalidGift:
		return respondError(c, fiber.StatusBadRequest, apiprotocol.GiftToSelf)
	case service.CodeGiftSentLimitExceeded:
		return respondError(c, fiber.StatusUnprocessableEntity, apiprotocol.GiftSentLimitExceeded(int64(quota.DailyGiftSentLimit)))
	case service.CodeGiftRecvLimitExceeded:
		return respondError(c, fiber.StatusUnprocessableEntity, apiprotocol.GiftRecvLimitExceeded(int64(quota.DailyGiftRecvLimit)))
	case service.CodeGiftSenderNotFound:
		return respondError(c, fiber.StatusNotFound, apiprotocol.GiftSenderNotFound(senderID))
	case service.CodeGiftReceiverNotFound:
		return respondError(c, fiber.StatusNotFound, apiprotocol.GiftReceiverNotFound(receiverID))
	case service.CodeGiftRefunded:
		return respondError(c, fiber.StatusConflict, apiprotocol.GiftRefunded)
	case service.CodeGiftKeyConflict:
		return respondError(c, fiber.StatusConflict, apiprotocol.GiftKeyAlreadyUsed)
	default:
		return h.respondServiceError(c, err, domain.Movement{PlayerID: senderID})
	}
}
