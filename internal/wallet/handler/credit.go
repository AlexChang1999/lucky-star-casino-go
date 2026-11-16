package handler

import (
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/output"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/dto/input"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/apiprotocol"

	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// creditToMovement 把入帳請求翻成一次已驗證的帳務意圖。
func creditToMovement(r input.CreditReq) (domain.Movement, error) {
	if err := checkCommon(r.PlayerID, r.Amount, r.IdempotencyKey, r.ReferenceID); err != nil {
		return domain.Movement{}, err
	}

	// credit 的 subType 是 @NotBlank + @Pattern，沒有預設值——入帳來源太多元
	// （中獎、簽到、任務、GM 補發…），沒有一個「合理的預設」。
	if r.SubType == nil || strings.TrimSpace(*r.SubType) == "" {
		return domain.Movement{}, fieldError{field: "subType", message: apiprotocol.MustNotBeBlank}
	}

	// ⚠️ 未帶時是 0，對齊 Java 的 `getUnfreezeAmount() == null ? 0L : ...`。
	// 傳 0 是**正常值不是錯誤**。
	var unfreeze int64
	if r.UnfreezeAmount != nil {
		unfreeze = *r.UnfreezeAmount
	}

	m, err := domain.NewCredit(
		*r.PlayerID,
		domain.Amount(*r.Amount),
		domain.SubType(*r.SubType),
		r.IdempotencyKey,
		r.ReferenceID,
		domain.Amount(unfreeze),
	)
	if err != nil {
		return domain.Movement{}, creditEndpoint.validationError(err)
	}
	return m, nil
}

// Credit 對應 Java 的 `POST /internal/wallet/credit`。
func (h *Handler) Credit(c fiber.Ctx) error {
	var req input.CreditReq
	if err := c.Bind().JSON(&req); err != nil {
		return respondError(c, fiber.StatusBadRequest, apiprotocol.InvalidRequestBody) // 理由見 Debit
	}

	m, err := creditToMovement(req)
	if err != nil {
		return h.respondValidationError(c, err)
	}

	r, err := h.svc.Credit(c.Context(), m) // ⚠️ c.Context() 不是 c（ADR-004）
	if err != nil {
		return h.respondServiceError(c, err, m)
	}

	return respondOK(c, output.CreditRes{
		TransactionID: r.TransactionID,
		PlayerID:      r.PlayerID,
		Amount:        r.Amount,
		BalanceBefore: r.BalanceBefore,
		BalanceAfter:  r.BalanceAfter,
		FrozenAfter:   r.FrozenAfter,
		Idempotent:    r.Idempotent,
	})
}
