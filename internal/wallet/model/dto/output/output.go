// Package output 是 wallet HTTP 回應 `data` 欄位的形狀。只放資料，沒有方法。
//
// ⚠️ 欄位順序刻意與 Java 對應的 Response 一致，讓契約測試逐位元組比對時不會看到差異。
package output

// DebitRes 對應 Java 的 DebitResponse。
type DebitRes struct {
	TransactionID int64 `json:"transactionId"`
	PlayerID      int64 `json:"playerId"`
	Amount        int64 `json:"amount"`
	BalanceBefore int64 `json:"balanceBefore"`
	BalanceAfter  int64 `json:"balanceAfter"`
	Idempotent    bool  `json:"idempotent"`
}

// CreditRes 對應 Java 的 CreditResponse。FrozenAfter 為 nil 時輸出 null（地雷 #33）。
type CreditRes struct {
	TransactionID int64  `json:"transactionId"`
	PlayerID      int64  `json:"playerId"`
	Amount        int64  `json:"amount"`
	BalanceBefore int64  `json:"balanceBefore"`
	BalanceAfter  int64  `json:"balanceAfter"`
	FrozenAfter   *int64 `json:"frozenAfter"`
	Idempotent    bool   `json:"idempotent"`
}

// GiftRes 對應 Java 的 GiftResponse。四個指標欄位在「錢已離開送出方、尚未到帳」
// 時是 null（地雷 #33）：那是真實狀態，不是遺漏——背景會繼續處理，同鍵重送可拿到最新結果。
type GiftRes struct {
	SenderID             int64  `json:"senderId"`
	ReceiverID           int64  `json:"receiverId"`
	Amount               int64  `json:"amount"`
	DebitTransactionID   *int64 `json:"debitTransactionId"`
	CreditTransactionID  *int64 `json:"creditTransactionId"`
	SenderBalanceAfter   *int64 `json:"senderBalanceAfter"`
	ReceiverBalanceAfter *int64 `json:"receiverBalanceAfter"`
	Idempotent           bool   `json:"idempotent"`
}

// BankruptcyAidRes 對應 Java 的 BankruptcyAidResponse。
type BankruptcyAidRes struct {
	PlayerID      int64 `json:"playerId"`
	Amount        int64 `json:"amount"`
	TransactionID int64 `json:"transactionId"`
	BalanceBefore int64 `json:"balanceBefore"`
	BalanceAfter  int64 `json:"balanceAfter"`
}
