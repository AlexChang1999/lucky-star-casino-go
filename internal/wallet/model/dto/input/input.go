// Package input 是 wallet HTTP 請求的 body 形狀。只放資料：沒有方法、沒有轉換、
// 沒有驗證（驗證與轉成 domain.Movement 在 handler，見 go-layered-arch §4）。
package input

// ⭐ 選填與可為 null 的欄位一律用**指標**（AGENTS.md 地雷 #33）：Java 的 `Long` 分得出
// null 與 0，Go 的 int64 分不出——`{"amount":null}` 與 `{"amount":0}` 在 Java 是
// 兩個不同的 400 訊息。subType 同理：沒帶 → 預設 BET；明確帶 `""` → @Pattern 不符 → 400。

// DebitReq 對應 `POST /internal/wallet/debit` 的 body。
type DebitReq struct {
	PlayerID       *int64  `json:"playerId"`
	Amount         *int64  `json:"amount"`
	SubType        *string `json:"subType"`
	IdempotencyKey string  `json:"idempotencyKey"`
	ReferenceID    string  `json:"referenceId"`
}

// CreditReq 對應 `POST /internal/wallet/credit` 的 body。
type CreditReq struct {
	PlayerID       *int64  `json:"playerId"`
	Amount         *int64  `json:"amount"`
	SubType        *string `json:"subType"`
	IdempotencyKey string  `json:"idempotencyKey"`
	ReferenceID    string  `json:"referenceId"`
	UnfreezeAmount *int64  `json:"unfreezeAmount"`
}

// GiftReq 對應 `POST /api/v1/wallet/gift` 的 body。送出方不在 body 裡：它只來自
// gateway 注入的 X-User-Id。
type GiftReq struct {
	ReceiverID     *int64 `json:"receiverId"`
	Amount         *int64 `json:"amount"`
	IdempotencyKey string `json:"idempotencyKey"`
}
