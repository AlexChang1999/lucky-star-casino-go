package httpapi

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// maxStringFieldLen 對應 Java DTO 的 `@Size(max = 100)`，也對應 schema 的 VARCHAR(100)。
const maxStringFieldLen = 100

// ── 請求 DTO ────────────────────────────────────────────────────────────────
//
// ⭐ 選填與可為 null 的欄位一律用**指標**（AGENTS.md 地雷 #33）。
// 這不是風格潔癖，是因為 Java 的 `Long` 分得出 null 與 0，而 Go 的 int64 分不出：
//
//	{"amount": null}  → Java 400 "amount must not be null"
//	{"amount": 0}     → Java 400 "amount must be greater than 0"
//
// 用 int64 的話兩者都會變成 0，於是**兩個不同的錯誤變成同一個**。
// 同理 `subType` 必須是 *string：debit 的 subType 沒帶時預設 BET，
// 但明確帶了空字串 `""` 在 Java 是 @Pattern 不符 → 400。
// 用 string 的話「沒帶」與「帶了空字串」會被混為一談，於是一筆本該被拒絕的
// 請求會被當成 BET 記成一筆下注——**沒有錯誤訊息**。

type debitRequest struct {
	PlayerID       *int64  `json:"playerId"`
	Amount         *int64  `json:"amount"`
	SubType        *string `json:"subType"`
	IdempotencyKey string  `json:"idempotencyKey"`
	ReferenceID    string  `json:"referenceId"`
}

type creditRequest struct {
	PlayerID       *int64  `json:"playerId"`
	Amount         *int64  `json:"amount"`
	SubType        *string `json:"subType"`
	IdempotencyKey string  `json:"idempotencyKey"`
	ReferenceID    string  `json:"referenceId"`
	UnfreezeAmount *int64  `json:"unfreezeAmount"`
}

// ── 驗證錯誤 ────────────────────────────────────────────────────────────────

// fieldError 重現 Java `GlobalExceptionHandler.handleValidation` 的訊息格式：
//
//	"Invalid request: " + fe.getField() + " " + fe.getDefaultMessage()
//
// ⚠️ 看起來像 bug 的地方是**對的**：自訂 @Pattern 訊息本身就以欄位名開頭
// （`"subType must be one of BET/SHOP_PURCHASE"`），所以串起來會變成
// `"Invalid request: subType subType must be one of BET/SHOP_PURCHASE"`
// ——欄位名出現兩次。這是 Java 版**真實的輸出**，契約測試會 diff 它。
// 「順手修掉」就是行為漂移（CLAUDE.md §5：等價 > 品味）。
//
// 把它做成 (field, message) 兩個欄位而不是一整句字串，就是為了讓這個重複
// 是**結構造成的**，而不是某個人手抄了一句奇怪的訊息。
type fieldError struct {
	field   string
	message string
}

func (e fieldError) Error() string {
	return "Invalid request: " + e.field + " " + e.message
}

// Bean Validation 的預設訊息。逐字對齊，因為它們會直接出現在回應 body 裡。
const (
	msgNotNull        = "must not be null"
	msgNotBlank       = "must not be blank"
	msgPositive       = "must be greater than 0"
	msgPositiveOrZero = "must be greater than or equal to 0"
	msgSize100        = "size must be between 0 and 100"
)

// endpoint 帶著兩個端點唯一不同的東西：subType 的 @Pattern 白名單訊息。
//
// ⚠️ 兩份清單合計 13 個且不重疊，剛好蓋滿 schema 的 chk_wt_sub_type
// （docs/notes/Java版-wallet-帳務口徑.md §2）。新增子類型要「四同步」
// （地雷 #8），這裡是第五個地方——因為訊息文字是對外契約。
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
// ⚠️ 每個 sentinel 對應**恰好一個**欄位，所以這張表能成立。
// domain 那邊哪天多一個 sentinel 而這裡忘了加，結果是 500 而不是 400——
// 會被看到，不會靜靜地錯。這是刻意選的失敗方向。
func (e endpoint) validationError(err error) error {
	switch {
	case errors.Is(err, domain.ErrPlayerIDRequired):
		// ⚠️ **刻意與 Java 分歧**：Java 的 playerId 只有 @NotNull 沒有 @Positive，
		// 所以 `playerId: 0` 會一路走到「查不到錢包」→ 404。
		// Go 這邊 domain 擋在前面 → 400。
		// 選 400 的理由：非正整數的玩家 ID 不可能有錢包，「請求有問題」比
		// 「資源不存在」更準確，而且真實呼叫端（game-service）永遠送真的 ID，
		// 這條路徑在正式流量上到不了。記在 CHANGELOG 與藍圖 §5，不是無聲漂移。
		return fieldError{field: "playerId", message: "must be positive"}
	case errors.Is(err, domain.ErrInvalidAmount):
		return fieldError{field: "amount", message: msgPositive}
	case errors.Is(err, domain.ErrUnknownSubType), errors.Is(err, domain.ErrSubTypeDirectionMismatch):
		// 兩個 sentinel 併成同一個回應，因為 Java 那邊也只有一條 @Pattern：
		// 「未知的子類型」與「子類型方向不符（例如 credit 帶 BET）」在對外看來
		// 都是「不在白名單裡」。
		return fieldError{field: "subType", message: e.subTypeMessage}
	case errors.Is(err, domain.ErrIdempotencyKeyRequired):
		return fieldError{field: "idempotencyKey", message: msgNotBlank}
	case errors.Is(err, domain.ErrIdempotencyKeyTooLong):
		return fieldError{field: "idempotencyKey", message: msgSize100}
	case errors.Is(err, domain.ErrInvalidUnfreezeAmount):
		return fieldError{field: "unfreezeAmount", message: msgPositiveOrZero}
	default:
		// domain.ErrUnknownTxType 與 ErrUnfreezeNotAllowed 從這裡走不到
		// （NewDebit / NewCredit 各自硬編了方向與 unfreeze）。真的出現代表
		// 程式有 bug 而不是請求有問題 → 原樣往上丟，落到 500。
		return err
	}
}

// ── DTO → Movement ─────────────────────────────────────────────────────────

// toMovement 把扣款請求翻成一次已驗證的帳務意圖。
//
// 這裡只做 domain **看不到**的檢查（null vs 0、trim 後是否為空、長度），
// 其餘一律交給 domain.NewDebit。分工的判準是：
// 「這是請求格式的規則，還是帳務的不變式？」——前者屬於這一層，後者屬於 domain。
func (r debitRequest) toMovement() (domain.Movement, error) {
	if err := checkCommon(r.PlayerID, r.Amount, r.IdempotencyKey, r.ReferenceID); err != nil {
		return domain.Movement{}, err
	}

	// ⚠️ nil（欄位沒帶或是 null）才走預設 BET，對齊 Java：
	// `request.getSubType() != null ? request.getSubType() : "BET"`。
	// 明確帶 `""` 的話 @Pattern 不符 → 400，所以不能一起當成 nil 處理。
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

// toMovement 把入帳請求翻成一次已驗證的帳務意圖。
func (r creditRequest) toMovement() (domain.Movement, error) {
	if err := checkCommon(r.PlayerID, r.Amount, r.IdempotencyKey, r.ReferenceID); err != nil {
		return domain.Movement{}, err
	}

	// credit 的 subType 是 @NotBlank + @Pattern，沒有預設值——入帳來源太多元
	// （中獎、簽到、任務、GM 補發…），沒有一個「合理的預設」。
	if r.SubType == nil || strings.TrimSpace(*r.SubType) == "" {
		return domain.Movement{}, fieldError{field: "subType", message: msgNotBlank}
	}

	// ⚠️ 未帶時是 0，對齊 Java 的
	// `request.getUnfreezeAmount() == null ? 0L : request.getUnfreezeAmount()`。
	// 傳 0 是**正常值不是錯誤**：目前 debit 尚未實作凍結流程，多數呼叫端不帶。
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

// checkCommon 跑兩個端點共有、且 domain 表達不了的那幾條 DTO 規則。
func checkCommon(playerID, amount *int64, idempotencyKey, referenceID string) error {
	// @NotNull：null 與 0 是**兩個不同的錯誤**，這是用指標的全部理由。
	if playerID == nil {
		return fieldError{field: "playerId", message: msgNotNull}
	}
	if amount == nil {
		return fieldError{field: "amount", message: msgNotNull}
	}

	// ⚠️ @NotBlank 是 trim 之後不可為空，而 domain 只擋 ""。兩者**不是同一條規則**：
	// domain 的是帳務不變式（鍵不可為空），這裡的是請求格式規則。
	// 少了這一行，`"idempotencyKey": "   "` 在 Java 是 400，在 Go 會變成一把
	// 由三個空白組成的合法冪等鍵——而且它真的會被寫進帳務流水。
	if strings.TrimSpace(idempotencyKey) == "" {
		return fieldError{field: "idempotencyKey", message: msgNotBlank}
	}
	// 冪等鍵的長度 domain 也擋（那是不變式），這裡擋是為了讓兩個端點的
	// referenceId 檢查有個對稱的位置。重複檢查的成本是一次 RuneCount。
	if err := checkSize("idempotencyKey", idempotencyKey); err != nil {
		return err
	}
	// ⚠️ referenceId 沒有任何一層擋長度：domain 不管它（它不參與帳務判定），
	// DB 是 VARCHAR(100)。少了這一行，一個 101 字元的 referenceId 會在
	// INSERT 時撞 MySQL 1406（Data too long）→ 500，而 Java 是 400。
	return checkSize("referenceId", referenceID)
}

// checkSize 對應 @Size(max = 100)。
//
// ⚠️ 算的是**字元不是位元組**，對齊 MySQL VARCHAR(100) 的語意。用 len() 會把
// 一個合法的 100 字元中文鍵當成 300 而誤拒——這種 bug 只在非 ASCII 輸入時出現，
// 用英文測資永遠測不到。
// （吹毛求疵的差異：Java 的 @Size 數的是 UTF-16 code unit，所以 BMP 以外的
// 字元在 Java 算 2、在這裡算 1。要撞到得送 51 個表情符號當 referenceId，
// 不值得為它把整層改成 UTF-16 計數。）
func checkSize(field, value string) error {
	if utf8.RuneCountInString(value) > maxStringFieldLen {
		return fieldError{field: field, message: msgSize100}
	}
	return nil
}
