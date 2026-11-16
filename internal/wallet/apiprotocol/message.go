package apiprotocol

import "fmt"

// 對外訊息。逐字對齊 Java 的 GlobalExceptionHandler / WalletController /
// BankruptcyAidService / GiftService；契約測試會 diff 它們，不要「順手潤飾」。
const (
	InternalError          = "Internal server error"
	Unauthorized           = "Unauthorized"
	InsufficientBalance    = "Insufficient balance"
	ConcurrentModify       = "Concurrent modification detected, please retry"
	MissingUserID          = "Missing X-User-Id header"
	InvalidUserID          = "Invalid X-User-Id header"
	AidBalanceTooHigh      = "Balance not below threshold (100), not eligible for bankruptcy aid"
	AidAlreadyClaimed      = "Bankruptcy aid already claimed today"
	GiftToSelf             = "Cannot gift to yourself"
	GiftRefunded           = "Gift could not be delivered and the amount was refunded"
	GiftKeyAlreadyUsed     = "Idempotency key already used by another gift"
	InvalidRequestPrefix   = "Invalid request: "
	PlayerIDMustBePositive = "must be positive"

	// InvalidRequestBody 是**本專案自己的**訊息，Java 沒有對應物（見 handler 的 Debit）。
	InvalidRequestBody = "Invalid request body"
)

// Bean Validation 的預設訊息。逐字對齊，因為它們會直接出現在回應 body 裡。
const (
	MustNotBeNull        = "must not be null"
	MustNotBeBlank       = "must not be blank"
	MustBePositive       = "must be greater than 0"
	MustBePositiveOrZero = "must be greater than or equal to 0"
)

// 兩個端點的 subType @Pattern 白名單訊息。
//
// ⚠️ 兩個白名單合計 13 個且不重疊，剛好蓋滿 schema 的 chk_wt_sub_type
// （docs/notes/Java版-wallet-帳務口徑.md §2）。新增子類型要「四同步」（地雷 #8），
// 這裡是第五個地方——因為訊息文字是對外契約。
const (
	DebitSubType  = "subType must be one of BET/SHOP_PURCHASE"
	CreditSubType = "subType must be one of WIN/CHECKIN/TASK/GIFT/GM_REWARD/" +
		"BANKRUPTCY_AID/DIAMOND_EXCHANGE/TOPUP/CASHBACK/REFUND/MONTHLY_REWARD"
)

// SizeBetween 對應 `@Size(max = N)` 的預設訊息。
func SizeBetween(max int) string {
	return fmt.Sprintf("size must be between 0 and %d", max)
}

// WalletNotFound 對應 `"Wallet not found for player: " + id`。
func WalletNotFound(playerID int64) string {
	return fmt.Sprintf("Wallet not found for player: %d", playerID)
}

// GiftSenderNotFound / GiftReceiverNotFound 是贈禮專屬的 404 訊息。
func GiftSenderNotFound(senderID int64) string {
	return fmt.Sprintf("Wallet not found for sender: %d", senderID)
}

func GiftReceiverNotFound(receiverID int64) string {
	return fmt.Sprintf("Wallet not found for receiver: %d", receiverID)
}

// GiftSentLimitExceeded / GiftRecvLimitExceeded 的額度數字由呼叫端傳入（取自 quota 的常數），
// 避免「改了上限、訊息還寫舊數字」。
func GiftSentLimitExceeded(limit int64) string {
	return fmt.Sprintf("Daily gift-sent limit exceeded (limit %d)", limit)
}

func GiftRecvLimitExceeded(limit int64) string {
	return fmt.Sprintf("Receiver daily gift-received limit exceeded (limit %d)", limit)
}
