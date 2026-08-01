// Package domain 放 wallet 的帳務型別與規則——**不碰資料庫、不碰網路**。
//
// 這裡的每一條規則都來自團隊 Java 版的實地查證（紀錄在
// docs/notes/Java版-wallet-帳務口徑.md），不是「看起來合理」的推測。
// 改動任何一條之前先回去讀原始碼：帳務規則寫錯不會有錯誤訊息，
// 只會有一個對不起來的餘額（CLAUDE.md §5）。
package domain

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Amount 是星幣金額，單位為**整數最小單位**（星幣沒有小數）。
//
// 為什麼要定義一個型別而不是直接用 int64：wallet 的函式簽章裡到處都是
// int64（playerID、amount、balance、version、txID），編譯器沒辦法幫你擋
// 「參數順序寫反」。定義具名型別之後，把 playerID 傳進 amount 的位置
// 會**編譯不過**。這是 Go 的 defined type 相對 Java 的優勢——Java 要做到
// 同樣的事得包一個 value object，成本高到大家都不做。
//
// ⚠️ 嚴禁改成 float64。IEEE 754 表示不了 0.1，而帳務對不起來的時候，
// 每一筆單看都「差不多對」。
type Amount int64

// 帳務主類型。對應 wallet_transactions.type 的 CHECK 約束。
type TxType string

const (
	TxTypeDebit  TxType = "DEBIT"  // 扣款
	TxTypeCredit TxType = "CREDIT" // 入帳
	// TxTypeBonus 在 DB 的 CHECK 約束裡存在，但 Java 版的 WalletService
	// 沒有任何路徑會寫入它。**保留但不使用**——移除等於偷偷改了 schema 契約。
	TxTypeBonus TxType = "BONUS"
)

// 帳務子類型。對應 wallet_transactions.sub_type 的 CHECK 約束（13 個值）。
//
// ⚠️ 新增一個子類型要**四同步**（AGENTS.md 地雷 #8）：
// 這裡的常數、DB 的 CHECK 約束、前端顯示、後台報表。
// 只改一處不會報錯，只會有一類交易在報表裡消失。
type SubType string

const (
	// DEBIT 類（扣款）
	SubTypeBet          SubType = "BET"           // 下注。game-service 不帶 subType 時的預設值
	SubTypeShopPurchase SubType = "SHOP_PURCHASE" // 商城兌換扣星幣

	// CREDIT 類（入帳）
	SubTypeWin             SubType = "WIN"
	SubTypeCheckin         SubType = "CHECKIN"
	SubTypeTask            SubType = "TASK"
	SubTypeGift            SubType = "GIFT"
	SubTypeGMReward        SubType = "GM_REWARD"
	SubTypeBankruptcyAid   SubType = "BANKRUPTCY_AID"
	SubTypeDiamondExchange SubType = "DIAMOND_EXCHANGE"
	SubTypeTopup           SubType = "TOPUP"
	SubTypeCashback        SubType = "CASHBACK"
	SubTypeRefund          SubType = "REFUND" // 退款／本金返還（捕魚 buy-in 退款、場次結算返還）
	// SubTypeMonthlyReward 刻意不是 WIN：算成 WIN 會污染 rank 的今日贏幣榜。
	// 這種「為了下游正確而拆出來的子類型」正是不能自行合併的原因。
	SubTypeMonthlyReward SubType = "MONTHLY_REWARD"
)

// subTypeDirection 是子類型 → 所屬主類型的對照。
//
// ⚠️ 這張表**必須**在應用層強制執行，DB 幫不上忙：
// wallet_transactions 的 CHECK 約束是 type 與 sub_type 各查各的白名單，
// 沒有任何約束擋得住 `type='CREDIT', sub_type='BET'` 這種組合。
// Java 版靠的是 DTO 上的兩條 @Pattern regex（CreditRequest 允許 11 種、
// DebitRequest 允許 2 種），Go 版把同一份知識收斂成這一張表。
var subTypeDirection = map[SubType]TxType{
	SubTypeBet:          TxTypeDebit,
	SubTypeShopPurchase: TxTypeDebit,

	SubTypeWin:             TxTypeCredit,
	SubTypeCheckin:         TxTypeCredit,
	SubTypeTask:            TxTypeCredit,
	SubTypeGift:            TxTypeCredit,
	SubTypeGMReward:        TxTypeCredit,
	SubTypeBankruptcyAid:   TxTypeCredit,
	SubTypeDiamondExchange: TxTypeCredit,
	SubTypeTopup:           TxTypeCredit,
	SubTypeCashback:        TxTypeCredit,
	SubTypeRefund:          TxTypeCredit,
	SubTypeMonthlyReward:   TxTypeCredit,
}

// maxIdempotencyKeyLen 對應 schema 的 VARCHAR(100)。
//
// ⚠️ MySQL 的 VARCHAR(100) 算的是**字元**不是位元組，所以驗證要用
// utf8.RuneCountInString 而不是 len()。用 len() 會把一個合法的
// 100 字元中文鍵當成 300 而誤拒——這種 bug 只在非 ASCII 輸入時出現，
// 用英文測資永遠測不到。
const maxIdempotencyKeyLen = 100

// 帳務驗證的 sentinel error。
//
// 為什麼是 sentinel 而不是各自定義型別：呼叫端只需要**分類**
// （是不是該回 400、要不要重試），不需要從 error 裡撈出額外欄位。
// 有了額外欄位的需求再升級成自訂型別，現在升級只是多寫程式碼。
var (
	ErrInvalidAmount            = errors.New("金額必須為正整數")
	ErrUnknownTxType            = errors.New("未知的帳務主類型")
	ErrUnknownSubType           = errors.New("未知的帳務子類型")
	ErrSubTypeDirectionMismatch = errors.New("子類型與主類型方向不符")
	ErrIdempotencyKeyRequired   = errors.New("冪等鍵不可為空")
	ErrIdempotencyKeyTooLong    = errors.New("冪等鍵超過長度上限")
	ErrPlayerIDRequired         = errors.New("playerID 必須為正整數")
	ErrInvalidUnfreezeAmount    = errors.New("解凍金額不可為負")
	ErrUnfreezeNotAllowed       = errors.New("只有 CREDIT 可以解凍")
)

// DirectionOf 回傳子類型所屬的主類型。第二個回傳值為 false 代表子類型未知。
func DirectionOf(sub SubType) (TxType, bool) {
	t, ok := subTypeDirection[sub]
	return t, ok
}

// Movement 是一次「已驗證」的帳務異動意圖，尚未落庫。
//
// 它刻意**不帶** balanceBefore / balanceAfter：那兩個值只有 DB 算得出來
// （條件 UPDATE 之後才知道），在這裡放欄位等於邀請別人在應用層算餘額，
// 而應用層算出來的餘額在併發下一定是錯的。
type Movement struct {
	PlayerID       int64
	Type           TxType
	SubType        SubType
	Amount         Amount
	UnfreezeAmount Amount // 選填、**僅 CREDIT 可非零**：要釋放的凍結金額
	IdempotencyKey string
	ReferenceID    string // 選填：roundID / eventID，供事後對帳
}

// NewDebit 建立一次已驗證的扣款意圖。
//
// subType 傳空字串時預設為 BET——對齊 Java 版：game-service 送的 JSON
// 不帶 subType，服務端預設記 BET。⚠️ 不要「順手」改成必填，
// 那會讓 game-service 現有的呼叫全部變成 400。
func NewDebit(playerID int64, amount Amount, subType SubType, idempotencyKey, referenceID string) (Movement, error) {
	if subType == "" {
		subType = SubTypeBet
	}
	// unfreeze 恆為 0：Java 的 DebitRequest 根本沒有這個欄位，凍結是 credit 端的事。
	return newMovement(playerID, TxTypeDebit, subType, amount, 0, idempotencyKey, referenceID)
}

// NewCredit 建立一次已驗證的入帳意圖。subType 為必填——入帳來源太多元
// （中獎、簽到、任務、GM 補發…），沒有一個「合理的預設」。
//
// unfreeze 是選填的解凍金額（對齊 Java `CreditRequest.unfreezeAmount`，
// `@PositiveOrZero`），用於「下注時先凍結、結算時解凍」的流程。
// ⚠️ 傳 0 是正常值不是錯誤——Java 的註解明說目前多半傳 0 或不傳。
//
// ⚠️ 這裡**不擋** unfreeze > 目前凍結金額。Java 的做法是在服務層
// `max(0, frozen - unfreeze)` 夾住並 log.warn（WalletService.java:196-200），
// 不是拒絕請求。要擋也擋不了——真正的凍結金額只有 DB 那一刻才知道，
// 在這裡比對等於拿一個過期的值做決定。
func NewCredit(playerID int64, amount Amount, subType SubType, idempotencyKey, referenceID string, unfreeze Amount) (Movement, error) {
	return newMovement(playerID, TxTypeCredit, subType, amount, unfreeze, idempotencyKey, referenceID)
}

func newMovement(playerID int64, txType TxType, subType SubType, amount, unfreeze Amount, idempotencyKey, referenceID string) (Movement, error) {
	if playerID <= 0 {
		return Movement{}, fmt.Errorf("%w: 得到 %d", ErrPlayerIDRequired, playerID)
	}
	// DB 有 CHECK (amount > 0)，但擋在這裡的價值是**錯誤訊息說得清楚**：
	// 讓 DB 擋，呼叫端拿到的是一句 MySQL 3819，指不到是哪個欄位錯。
	if amount <= 0 {
		return Movement{}, fmt.Errorf("%w: 得到 %d", ErrInvalidAmount, amount)
	}
	if txType != TxTypeDebit && txType != TxTypeCredit {
		return Movement{}, fmt.Errorf("%w: %q", ErrUnknownTxType, txType)
	}
	want, ok := DirectionOf(subType)
	if !ok {
		return Movement{}, fmt.Errorf("%w: %q", ErrUnknownSubType, subType)
	}
	if want != txType {
		return Movement{}, fmt.Errorf("%w: %q 屬於 %s，不能用在 %s", ErrSubTypeDirectionMismatch, subType, want, txType)
	}
	// 對齊 Java 的 @PositiveOrZero：0 合法、負數不合法。
	// ⚠️ 負的 unfreeze 會讓 store 那層的 frozen_amount **變大**，
	// 於是可用餘額（balance - frozen_amount）憑空變小，症狀是後續下注拿到
	// 「餘額不足」而餘額看起來明明夠。schema 的 CHECK 只擋負數，擋不住這個方向。
	if unfreeze < 0 {
		return Movement{}, fmt.Errorf("%w: 得到 %d", ErrInvalidUnfreezeAmount, unfreeze)
	}
	// 走公開建構子時不可能觸發（NewDebit 硬編 0），但 Movement 是可以手動組出來的
	// struct。把不變式寫在這裡，它才有一個看得見、測得到的位置。
	if unfreeze != 0 && txType != TxTypeCredit {
		return Movement{}, fmt.Errorf("%w: %s 帶了 unfreeze=%d", ErrUnfreezeNotAllowed, txType, unfreeze)
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return Movement{}, err
	}
	return Movement{
		PlayerID:       playerID,
		Type:           txType,
		SubType:        subType,
		Amount:         amount,
		UnfreezeAmount: unfreeze,
		IdempotencyKey: idempotencyKey,
		ReferenceID:    referenceID,
	}, nil
}

// validateIdempotencyKey 檢查冪等鍵。
//
// ⚠️ 這裡**不做**大小寫正規化。看起來「順手 ToUpper 一下比較保險」，
// 但冪等鍵的相等性由 DB 的 UNIQUE 索引定義（schema 已指定 utf8mb4_bin，
// 區分大小寫），應用層再加一層正規化就會出現「Go 認為相同、DB 認為不同」
// 的錯位——那是最難查的一種 bug，因為兩邊各自都是對的。
func validateIdempotencyKey(key string) error {
	if key == "" {
		return ErrIdempotencyKeyRequired
	}
	// 算字元不是位元組，對齊 MySQL VARCHAR(100) 的語意。
	if n := utf8.RuneCountInString(key); n > maxIdempotencyKeyLen {
		return fmt.Errorf("%w: %d 字元，上限 %d", ErrIdempotencyKeyTooLong, n, maxIdempotencyKeyLen)
	}
	return nil
}
