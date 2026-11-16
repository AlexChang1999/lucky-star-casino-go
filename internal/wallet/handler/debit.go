package handler

import (
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/output"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/input"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/apiprotocol"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// debitToMovement 把扣款請求翻成一次已驗證的帳務意圖。
//
// 這裡只做 domain **看不到**的檢查（null vs 0、trim 後是否為空、長度），其餘交給
// domain.NewDebit。判準是：「這是請求格式的規則，還是帳務的不變式？」——前者屬於這一層。
func debitToMovement(r input.DebitReq) (domain.Movement, error) {
	if err := checkCommon(r.PlayerID, r.Amount, r.IdempotencyKey, r.ReferenceID); err != nil {
		return domain.Movement{}, err
	}

	// ⚠️ nil（沒帶或 null）才走預設 BET，對齊 Java：
	// `request.getSubType() != null ? request.getSubType() : "BET"`。
	var subType domain.SubType
	if r.SubType != nil {
		if *r.SubType == "" {
			return domain.Movement{}, fieldError{field: "subType", message: debitEndpoint.subTypeMessage}
		}
		subType = domain.SubType(*r.SubType)
	}

	m, err := domain.NewDebit(*r.PlayerID, domain.Amount(*r.Amount), subType, r.IdempotencyKey, r.ReferenceID)
	if err != nil {
		return domain.Movement{}, debitEndpoint.validationError(err)
	}
	return m, nil
}

// Debit 對應 Java 的 `POST /internal/wallet/debit`。
func (h *Handler) Debit(c fiber.Ctx) error {
	var req input.DebitReq
	if err := c.Bind().JSON(&req); err != nil {
		// ⚠️ **刻意的分歧**：Java 的 GlobalExceptionHandler 沒有處理
		// HttpMessageNotReadableException，而它註冊了 @ExceptionHandler(Exception.class)，
		// 於是壞 JSON 會落到 handleGeneral → **500**。「呼叫端送了壞 JSON，伺服器說自己壞了」
		// 是缺陷不是契約，這裡回 400。記在藍圖 §5，待契約測試對 Java 版複驗。
		return respondError(c, fiber.StatusBadRequest, apiprotocol.InvalidRequestBody)
	}

	m, err := debitToMovement(req)
	if err != nil {
		return h.respondValidationError(c, err)
	}

	// ⚠️ 傳 c.Context() 而不是 c：fiber.Ctx 實作 context.Context 但永遠不會取消（ADR-004）。
	r, err := h.svc.Debit(c.Context(), m)
	if err != nil {
		return h.respondServiceError(c, err, m)
	}

	return respondOK(c, output.DebitRes{
		TransactionID: r.TransactionID,
		PlayerID:      r.PlayerID,
		Amount:        r.Amount,
		BalanceBefore: r.BalanceBefore,
		BalanceAfter:  r.BalanceAfter,
		Idempotent:    r.Idempotent,
	})
}
