package handler

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// maxStringFieldLen 對應 Java DTO 的 `@Size(max = 100)`，也對應 schema 的 VARCHAR(100)。
const maxStringFieldLen = 100

// fieldError 重現 Java `GlobalExceptionHandler.handleValidation` 的訊息格式：
//
//	"Invalid request: " + fe.getField() + " " + fe.getDefaultMessage()
//
// ⚠️ 看起來像 bug 的地方是**對的**：自訂 @Pattern 訊息本身就以欄位名開頭
// （`"subType must be one of BET/SHOP_PURCHASE"`），所以串起來會變成
// `"Invalid request: subType subType must be one of BET/SHOP_PURCHASE"`——
// 欄位名出現兩次。這是 Java 版**真實的輸出**，契約測試會 diff 它。
// 「順手修掉」就是行為漂移（CLAUDE.md §5：等價 > 品味）。
// 做成 (field, message) 兩個欄位，就是為了讓這個重複是**結構造成的**，而不是手抄錯。
type fieldError struct {
	field   string
	message string
}

func (e fieldError) Error() string {
	return "Invalid request: " + e.field + " " + e.message
}

// Bean Validation 的預設訊息。逐字對齊，因為它們會直接出現在回應 body 裡。
const (
	msgNotNull  = "must not be null"
	msgNotBlank = "must not be blank"
	msgPositive = "must be greater than 0"
	msgSize100  = "size must be between 0 and 100"

	msgPositiveOrZero = "must be greater than or equal to 0"
)

// endpoint 帶著端點唯一不同的東西：subType 的 @Pattern 白名單訊息。
//
// ⚠️ 兩個端點的白名單合計 13 個且不重疊，剛好蓋滿 schema 的 chk_wt_sub_type
// （docs/notes/Java版-wallet-帳務口徑.md §2）。新增子類型要「四同步」（地雷 #8），
// 這裡是第五個地方——因為訊息文字是對外契約。
type endpoint struct {
	subTypeMessage string
}

var (
	debitEndpoint = endpoint{
		subTypeMessage: "subType must be one of BET/SHOP_PURCHASE",
	}
	creditEndpoint = endpoint{
		subTypeMessage: "subType must be one of WIN/CHECKIN/TASK/GIFT/GM_REWARD/" +
			"BANKRUPTCY_AID/DIAMOND_EXCHANGE/TOPUP/CASHBACK/REFUND/MONTHLY_REWARD",
	}
)

// validationError 把 domain 的 sentinel 翻譯成 Java 的欄位訊息。
//
// ⚠️ 每個 sentinel 對應**恰好一個**欄位，所以這張表能成立。domain 哪天多一個
// sentinel 而這裡忘了加，結果是 500 而不是 400——會被看到，不會靜靜地錯。
func (e endpoint) validationError(err error) error {
	switch {
	case errors.Is(err, domain.ErrPlayerIDRequired):
		// ⚠️ **刻意與 Java 分歧**：Java 的 playerId 只有 @NotNull 沒有 @Positive，
		// 所以 `playerId: 0` 會一路走到「查不到錢包」→ 404。Go 這邊 domain 擋在前面 → 400。
		// 非正整數的玩家 ID 不可能有錢包，「請求有問題」比「資源不存在」準確；
		// 真實呼叫端（game-service）永遠送真的 ID。記在 CHANGELOG 與藍圖 §5。
		return fieldError{field: "playerId", message: "must be positive"}
	case errors.Is(err, domain.ErrInvalidAmount):
		return fieldError{field: "amount", message: msgPositive}
	case errors.Is(err, domain.ErrUnknownSubType), errors.Is(err, domain.ErrSubTypeDirectionMismatch):
		// 兩個 sentinel 併成同一個回應，因為 Java 也只有一條 @Pattern。
		return fieldError{field: "subType", message: e.subTypeMessage}
	case errors.Is(err, domain.ErrIdempotencyKeyRequired):
		return fieldError{field: "idempotencyKey", message: msgNotBlank}
	case errors.Is(err, domain.ErrIdempotencyKeyTooLong):
		return fieldError{field: "idempotencyKey", message: msgSize100}
	case errors.Is(err, domain.ErrInvalidUnfreezeAmount):
		return fieldError{field: "unfreezeAmount", message: msgPositiveOrZero}
	default:
		// domain.ErrUnknownTxType、ErrUnfreezeNotAllowed 等從這裡走不到（NewDebit / NewCredit 硬編了方向與 unfreeze）。
		// 真的出現代表程式有 bug 而不是請求有問題 → 原樣往上丟，落到 500。
		return err
	}
}

// checkCommon 跑端點共有、且 domain 表達不了的那幾條 DTO 規則。
func checkCommon(playerID, amount *int64, idempotencyKey, referenceID string) error {
	// @NotNull：null 與 0 是**兩個不同的錯誤**，這是用指標的全部理由（地雷 #33）。
	if playerID == nil {
		return fieldError{field: "playerId", message: msgNotNull}
	}
	if amount == nil {
		return fieldError{field: "amount", message: msgNotNull}
	}

	// ⚠️ @NotBlank 是 trim 之後不可為空，而 domain 只擋 ""。兩者**不是同一條規則**：
	// 少了這一行，`"idempotencyKey": "   "` 在 Java 是 400，在 Go 會變成一把
	// 由三個空白組成的合法冪等鍵——而且它真的會被寫進帳務流水。
	if strings.TrimSpace(idempotencyKey) == "" {
		return fieldError{field: "idempotencyKey", message: msgNotBlank}
	}
	if err := checkSize("idempotencyKey", idempotencyKey); err != nil {
		return err
	}
	// ⚠️ referenceId 沒有任何一層擋長度：少了這一行，101 字元的 referenceId 會在
	// INSERT 時撞 MySQL 1406（Data too long）→ 500，而 Java 是 400。
	return checkSize("referenceId", referenceID)
}

// checkSize 對應 @Size(max = 100)。
//
// ⚠️ 算的是**字元不是位元組**，對齊 MySQL VARCHAR(100) 的語意。用 len() 會把
// 一個合法的 100 字元中文鍵當成 300 而誤拒——只在非 ASCII 輸入時出現。
// （吹毛求疵的差異：Java 的 @Size 數 UTF-16 code unit，BMP 以外的字元在 Java 算 2、
// 這裡算 1，不值得為它改成 UTF-16 計數。）
func checkSize(field, value string) error {
	if utf8.RuneCountInString(value) > maxStringFieldLen {
		return fieldError{field: field, message: msgSize100}
	}
	return nil
}
