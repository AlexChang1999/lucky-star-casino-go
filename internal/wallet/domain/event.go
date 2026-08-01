package domain

// Kafka topic 名稱。
//
// ⚠️ `wallet.debit` / `wallet.credit` 是**事件**（某事已發生），
// `wallet.credit.request` 才是**指令**（請做某事）——AGENTS.md 地雷 #2。
// 搞反會讓 wallet 消費到自己發出的事件而無限迴圈。
// wallet **發出**事件、**消費**指令，兩個不同 topic，結構上接不上自己。
const (
	TopicWalletDebit  = "wallet.debit"
	TopicWalletCredit = "wallet.credit"
)

// MovementEvent 是 `wallet.debit` 與 `wallet.credit` 共用的事件 payload。
//
// 逐欄位對齊 Java 版的 record（`kafka/WalletDebitEvent.java` 與
// `kafka/WalletCreditEvent.java`），**欄位順序也一樣**——Jackson 序列化 record 時照
// component 順序輸出，Go 的 encoding/json 照 struct 欄位順序輸出，於是兩邊產生的
// JSON 是逐位元組相同的。這件事有測試釘住（event_test.go），因為 outbox 的 payload
// 欄位存的是**原始字串**，契約測試會直接比對它。
//
// ⚠️ 為什麼 Java 是兩個 record、Go 只有一個 struct：那兩個 record 的 component
// **完全相同**（8 個、同名同序同型，已逐行比對）。Java 寫成兩份是因為它沒有型別別名，
// 不是刻意的語義區分。複製兩份 Go struct 只會得到兩個必定漂移的定義，
// 而漂移的症狀是「某個 topic 的 payload 少一個欄位」——沒有錯誤訊息。
// 兩個 topic 的**語義**區分靠的是常數與呼叫端，不是靠 struct 名字。
//
// ⚠️ ReferenceID 是 *string 而不是 string——這不是潔癖，是等價問題：
// Java 的 `String referenceId` 沒帶時是 null，Jackson 預設會輸出 `"referenceId": null`
// （這個專案沒有設定 NON_NULL，已確認）。Go 的 string 零值是 ""，
// 會輸出 `"referenceId": ""`。兩者在 JSON 裡**不是同一個值**，
// 下游把它投影進 MongoDB 之後就是「有的文件是 null、有的是空字串」，
// 而查詢 `{referenceId: null}` 找不到後者。**沒有任何一層會報錯。**
type MovementEvent struct {
	TransactionID  int64   `json:"transactionId"`
	PlayerID       int64   `json:"playerId"`
	Amount         Amount  `json:"amount"`
	BalanceBefore  Amount  `json:"balanceBefore"`
	BalanceAfter   Amount  `json:"balanceAfter"`
	SubType        SubType `json:"subType"`
	IdempotencyKey string  `json:"idempotencyKey"`
	ReferenceID    *string `json:"referenceId"`
}

// DebitEvent / CreditEvent 是**型別別名**（`=`）而不是定義新型別。
//
// 差別很重要：`type DebitEvent MovementEvent` 會產生一個**不同的型別**，
// 兩者之間要顯式轉換；`=` 則讓它們是同一個型別的兩個名字。
// 這裡要的是後者——名字用來讓呼叫端讀起來像 Java 版，型別只有一個。
// （Java 沒有等價物；最接近的是 `import X as Y`，而 Java 連那個都沒有。）
type (
	DebitEvent  = MovementEvent
	CreditEvent = MovementEvent
)

// OptionalString 把「Go 的空字串」翻譯成「SQL 的 NULL / JSON 的 null」。
//
// 同一個轉換在兩個地方都要用（事件 payload 與 reference_id 欄位），
// 所以放在 domain 而不是各自寫一份：兩份一定會漂移，而漂移的症狀是
// 「事件裡是 null、DB 裡是空字串」這種對得起來又對不起來的資料。
func OptionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
