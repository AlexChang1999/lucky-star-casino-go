package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// 對外訊息。逐字對齊 Java 的 GlobalExceptionHandler，契約測試會 diff 它們。
const (
	internalErrorMessage    = "Internal server error"
	insufficientMessage     = "Insufficient balance"
	concurrentModifyMessage = "Concurrent modification detected, please retry"

	// ⚠️ malformedBodyMessage 是**本專案自己的**訊息，Java 沒有對應物——
	// 理由見 debit 裡的註解（這是一條刻意的分歧）。
	malformedBodyMessage = "Invalid request body"
)

// ── 回應 DTO ────────────────────────────────────────────────────────────────
//
// ⚠️ 欄位順序刻意與 Java 的 DebitResponse / CreditResponse 一致。
// JSON 物件理論上無序，但契約測試若逐位元組比對就會看到差異，
// 而「讓兩邊長得一模一樣」的成本只是把欄位排對。

type debitResponse struct {
	TransactionID int64 `json:"transactionId"`
	PlayerID      int64 `json:"playerId"`
	Amount        int64 `json:"amount"`
	BalanceBefore int64 `json:"balanceBefore"`
	BalanceAfter  int64 `json:"balanceAfter"`
	Idempotent    bool  `json:"idempotent"`
}

type creditResponse struct {
	TransactionID int64 `json:"transactionId"`
	PlayerID      int64 `json:"playerId"`
	Amount        int64 `json:"amount"`
	BalanceBefore int64 `json:"balanceBefore"`
	BalanceAfter  int64 `json:"balanceAfter"`
	// ⚠️ *int64 而不是 int64（AGENTS.md 地雷 #33）：冪等命中時 Java 明確回
	// `null`（「不重算凍結；以當初入帳結果為準」，WalletService.java:179、:229）。
	// 用 int64 的話那個 null 會靜靜變成 0——而 0 是一個**合法的凍結金額**，
	// 呼叫端分不出「沒有這個資訊」與「凍結金額是 0」。
	FrozenAfter *int64 `json:"frozenAfter"`
	Idempotent  bool   `json:"idempotent"`
}

// ── 端點 ────────────────────────────────────────────────────────────────────

// debit 對應 Java 的 `POST /internal/wallet/debit`。
func (a *API) debit(c *gin.Context) {
	var req debitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// ⚠️ **刻意的分歧**：Java 的 GlobalExceptionHandler 沒有處理
		// HttpMessageNotReadableException，而它註冊了 @ExceptionHandler(Exception.class)，
		// 於是 ExceptionHandlerExceptionResolver 會搶在 Spring 內建的
		// DefaultHandlerExceptionResolver（那個才會回 400）之前接住它 → **500**。
		// 「呼叫端送了壞 JSON，伺服器說自己壞了」是缺陷不是契約，這裡回 400。
		// 記在藍圖 §5，並待契約測試對真的跑起來的 Java 版複驗。
		a.respondError(c, http.StatusBadRequest, malformedBodyMessage)
		return
	}

	m, err := req.toMovement()
	if err != nil {
		a.respondValidationError(c, err)
		return
	}

	result, err := a.store.Debit(c.Request.Context(), m)
	if err != nil {
		a.respondStoreError(c, err, m)
		return
	}

	a.respondOK(c, debitResponse{
		TransactionID: result.TransactionID,
		// ⚠️ 這裡用的是 **result** 的 PlayerID / Amount 而不是請求的。
		// 冪等命中時 Java 回的是**原交易**的值（toIdempotentResponse:135-142），
		// 冪等鍵跨玩家碰撞時兩者會不同——那是刻意保留的診斷訊號，不是 bug。
		PlayerID:      result.PlayerID,
		Amount:        int64(result.Amount),
		BalanceBefore: int64(result.BalanceBefore),
		BalanceAfter:  int64(result.BalanceAfter),
		Idempotent:    result.Idempotent,
	})
}

// credit 對應 Java 的 `POST /internal/wallet/credit`。
func (a *API) credit(c *gin.Context) {
	var req creditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		a.respondError(c, http.StatusBadRequest, malformedBodyMessage)
		return
	}

	m, err := req.toMovement()
	if err != nil {
		a.respondValidationError(c, err)
		return
	}

	result, err := a.store.Credit(c.Request.Context(), m)
	if err != nil {
		a.respondStoreError(c, err, m)
		return
	}

	resp := creditResponse{
		TransactionID: result.TransactionID,
		PlayerID:      result.PlayerID,
		Amount:        int64(result.Amount),
		BalanceBefore: int64(result.BalanceBefore),
		BalanceAfter:  int64(result.BalanceAfter),
		Idempotent:    result.Idempotent,
	}
	if result.FrozenAfter != nil {
		frozen := int64(*result.FrozenAfter)
		resp.FrozenAfter = &frozen
	}
	a.respondOK(c, resp)
}

// ── 錯誤 → 狀態碼 ───────────────────────────────────────────────────────────

// respondValidationError 把驗證錯誤翻成 400。
func (a *API) respondValidationError(c *gin.Context, err error) {
	var fe fieldError
	if errors.As(err, &fe) {
		a.respondError(c, http.StatusBadRequest, fe.Error())
		return
	}
	// 走到這裡代表 domain 多了一個 sentinel、而 endpoint.validationError 沒跟上。
	// 回 500 是刻意的失敗方向：它會被看到、會被記錄，而不是靜靜地變成某個
	// 看起來合理的 400。
	a.logger.Error("驗證錯誤沒有對應的欄位訊息（domain 是不是多了一個 sentinel？）", "err", err)
	a.respondError(c, http.StatusInternalServerError, internalErrorMessage)
}

// respondStoreError 把帳務層的 sentinel 翻成 Java 版的狀態碼與訊息。
//
// ⭐ 這張表就是整個切片的核心，來源是逐行讀過的 `GlobalExceptionHandler`：
//
//	ErrWalletNotFound          → 404  WalletNotFoundException
//	ErrInsufficientBalance     → 422  InsufficientBalanceException（**不是 400**）
//	ErrConcurrentModification  → 409  ObjectOptimisticLockingFailureException
//	其餘                        → 500  handleGeneral
//
// ⚠️ **餘額不足是 422 不是 400**。Java 的 `@ResponseStatus(HttpStatus.UNPROCESSABLE_ENTITY)`
// 寫在 GlobalExceptionHandler:23-27。語義上也說得通：JSON 語法對、欄位都合法，
// 是**業務狀態**不允許——那正是 422 與 400 的分界。
func (a *API) respondStoreError(c *gin.Context, err error, m domain.Movement) {
	switch {
	case errors.Is(err, walletstore.ErrWalletNotFound):
		// 訊息帶請求的 playerID，逐字對齊 Java：
		// `"Wallet not found for player: " + request.getPlayerId()`。
		a.respondError(c, http.StatusNotFound,
			fmt.Sprintf("Wallet not found for player: %d", m.PlayerID))

	case errors.Is(err, walletstore.ErrInsufficientBalance):
		a.respondError(c, http.StatusUnprocessableEntity, insufficientMessage)

	case errors.Is(err, walletstore.ErrConcurrentModification):
		// ⚠️ 用 Warn 而不是 Error：credit 是讀改寫 + 樂觀鎖，同玩家高併發下
		// 成功率是 1/N（地雷 #36），409 在那個情境是**常態**。記成 Error 會讓
		// 告警一直響，然後大家開始忽略告警——那比沒有告警更糟。
		a.logger.Warn("樂觀鎖衝突，呼叫端應帶原本那把冪等鍵重試",
			"playerID", m.PlayerID,
			"idempotencyKey", m.IdempotencyKey,
		)
		// 訊息是 Java 寫死的那一句（GlobalExceptionHandler:131），
		// 不是 ex.getMessage()。
		a.respondError(c, http.StatusConflict, concurrentModifyMessage)

	default:
		// 其餘一律 500 + 不外洩細節（對齊 handleGeneral），但**內部要記全**：
		// 冪等鍵是事後追一筆帳唯一有用的線索。
		//
		// 這一格接住的包括 ErrIdempotencyWinnerMissing（Java 是 IllegalStateException
		// → 同樣落到 handleGeneral → 500）、ErrTransactionBalanceMissing
		// （這一條 Java 會回 200 帶 null，是本專案刻意的分歧，見 CHANGELOG）、
		// 以及所有 DB 層的意外。
		a.logger.Error("帳務操作失敗",
			"type", m.Type,
			"subType", m.SubType,
			"playerID", m.PlayerID,
			"amount", int64(m.Amount),
			"idempotencyKey", m.IdempotencyKey,
			"err", err,
		)
		a.respondError(c, http.StatusInternalServerError, internalErrorMessage)
	}
}
