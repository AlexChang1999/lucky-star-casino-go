package handler

import (
	"fmt"
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

// 對外訊息。前五句逐字對齊 Java（GiftService / GiftTransferService）。
// 額度數字取自 quota 的常數，避免「改了上限、訊息還寫舊數字」。
var (
	giftToSelfMessage      = "Cannot gift to yourself"
	giftSentLimitMessage   = fmt.Sprintf("Daily gift-sent limit exceeded (limit %d)", quota.DailyGiftSentLimit)
	giftRecvLimitMessage   = fmt.Sprintf("Receiver daily gift-received limit exceeded (limit %d)", quota.DailyGiftRecvLimit)
	giftSizeMessage        = fmt.Sprintf("size must be between 0 and %d", maxGiftKeyLen)
	giftRefundedMessage    = "Gift could not be delivered and the amount was refunded"
	giftKeyConflictMessage = "Idempotency key already used by another gift"
)

type giftRequest struct {
	ReceiverID     *int64 `json:"receiverId"`
	Amount         *int64 `json:"amount"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// ⚠️ 欄位順序與 Java 的 GiftResponse 一致。四個指標欄位在「錢已離開送出方、尚未到帳」
// 時是 null（地雷 #33）：那是真實狀態，不是遺漏——背景會繼續處理，同鍵重送可拿到最新結果。
type giftResponse struct {
	SenderID             int64  `json:"senderId"`
	ReceiverID           int64  `json:"receiverId"`
	Amount               int64  `json:"amount"`
	DebitTransactionID   *int64 `json:"debitTransactionId"`
	CreditTransactionID  *int64 `json:"creditTransactionId"`
	SenderBalanceAfter   *int64 `json:"senderBalanceAfter"`
	ReceiverBalanceAfter *int64 `json:"receiverBalanceAfter"`
	Idempotent           bool   `json:"idempotent"`
}

// validate 跑 Java GiftRequest 的 Bean Validation（@NotNull / @Positive / @NotBlank / @Size）。
// 順序與 DTO 欄位宣告順序一致。
func (r giftRequest) validate() error {
	if r.ReceiverID == nil {
		return fieldError{field: "receiverId", message: msgNotNull}
	}
	if r.Amount == nil {
		return fieldError{field: "amount", message: msgNotNull}
	}
	if *r.Amount <= 0 {
		return fieldError{field: "amount", message: msgPositive}
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return fieldError{field: "idempotencyKey", message: msgNotBlank}
	}
	// 數字元不是位元組，理由同 checkSize。
	if len([]rune(r.IdempotencyKey)) > maxGiftKeyLen {
		return fieldError{field: "idempotencyKey", message: giftSizeMessage}
	}
	return nil
}

// Gift 對應 Java 的 `POST /api/v1/wallet/gift`。送出方只來自 gateway 注入的 X-User-Id。
func (h *Handler) Gift(c fiber.Ctx) error {
	raw := c.Get("X-User-Id")
	if strings.TrimSpace(raw) == "" {
		return respondError(c, fiber.StatusBadRequest, missingUserIDMessage)
	}
	senderID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return respondError(c, fiber.StatusBadRequest, invalidUserIDMessage)
	}

	var req giftRequest
	if err := c.Bind().JSON(&req); err != nil {
		return respondError(c, fiber.StatusBadRequest, malformedBodyMessage)
	}
	if err := req.validate(); err != nil {
		return h.respondValidationError(c, err)
	}

	r, err := h.svc.Gift(c.Context(), senderID, *req.ReceiverID, *req.Amount, req.IdempotencyKey) // ⚠️ c.Context()（ADR-004）
	if err != nil {
		return h.respondGiftError(c, err, senderID, *req.ReceiverID)
	}

	return respondOK(c, giftResponse{
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
		return respondError(c, fiber.StatusBadRequest, giftToSelfMessage)
	case service.CodeGiftSentLimitExceeded:
		return respondError(c, fiber.StatusUnprocessableEntity, giftSentLimitMessage)
	case service.CodeGiftRecvLimitExceeded:
		return respondError(c, fiber.StatusUnprocessableEntity, giftRecvLimitMessage)
	case service.CodeGiftSenderNotFound:
		return respondError(c, fiber.StatusNotFound, fmt.Sprintf("Wallet not found for sender: %d", senderID))
	case service.CodeGiftReceiverNotFound:
		return respondError(c, fiber.StatusNotFound, fmt.Sprintf("Wallet not found for receiver: %d", receiverID))
	case service.CodeGiftRefunded:
		return respondError(c, fiber.StatusConflict, giftRefundedMessage)
	case service.CodeGiftKeyConflict:
		return respondError(c, fiber.StatusConflict, giftKeyConflictMessage)
	default:
		return h.respondServiceError(c, err, domain.Movement{PlayerID: senderID})
	}
}
