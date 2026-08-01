package domain

import (
	"errors"
	"strings"
	"testing"
)

// 表格驅動是本專案測試的預設形狀（CLAUDE.md §4）。
// 用 t.Run 讓失敗自己報名字，不必從行號回推是哪個案例。

func TestNewDebit(t *testing.T) {
	tests := []struct {
		name    string
		player  int64
		amount  Amount
		subType SubType
		key     string
		wantErr error
		// wantSubType 只在預期成功時檢查。
		wantSubType SubType
	}{
		{
			name: "正常下注", player: 42, amount: 100, subType: SubTypeBet,
			key: "bet-42-round-1", wantSubType: SubTypeBet,
		},
		{
			// 對齊 Java 版：game-service 送的 JSON 不帶 subType → 服務端預設 BET。
			name: "subType 留空預設為 BET", player: 42, amount: 100, subType: "",
			key: "bet-42-round-2", wantSubType: SubTypeBet,
		},
		{
			name: "商城兌換也是扣款", player: 42, amount: 500, subType: SubTypeShopPurchase,
			key: "shop-42-item-7", wantSubType: SubTypeShopPurchase,
		},
		{
			// ⭐ DB 的 CHECK 約束擋不住這個組合（type 與 sub_type 各查各的白名單），
			// 所以應用層一定要擋。這一條就是那個保護的證據。
			name: "入帳子類型不能用在扣款", player: 42, amount: 100, subType: SubTypeWin,
			key: "bad-42-1", wantErr: ErrSubTypeDirectionMismatch,
		},
		{
			name: "金額為零", player: 42, amount: 0, subType: SubTypeBet,
			key: "bad-42-2", wantErr: ErrInvalidAmount,
		},
		{
			// 負數扣款＝變相入帳。DB 也有 CHECK (amount > 0)，這裡是第一道。
			name: "金額為負", player: 42, amount: -100, subType: SubTypeBet,
			key: "bad-42-3", wantErr: ErrInvalidAmount,
		},
		{
			name: "未知子類型", player: 42, amount: 100, subType: SubType("PLUNDER"),
			key: "bad-42-4", wantErr: ErrUnknownSubType,
		},
		{
			// 冪等鍵是防重複扣款的唯一機制，空字串等於沒有保護。
			name: "冪等鍵為空", player: 42, amount: 100, subType: SubTypeBet,
			key: "", wantErr: ErrIdempotencyKeyRequired,
		},
		{
			name: "playerID 為零", player: 0, amount: 100, subType: SubTypeBet,
			key: "bad-0-1", wantErr: ErrPlayerIDRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewDebit(tt.player, tt.amount, tt.subType, tt.key, "")

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
				// 驗證失敗時必須回零值，不可以回半成品——
				// 呼叫端忽略 error 時，零值會在 DB 層再被擋一次；
				// 半成品則可能真的寫進去。
				if got != (Movement{}) {
					t.Errorf("驗證失敗時應回零值 Movement，得到 %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if got.Type != TxTypeDebit {
				t.Errorf("Type = %q, want %q", got.Type, TxTypeDebit)
			}
			if got.SubType != tt.wantSubType {
				t.Errorf("SubType = %q, want %q", got.SubType, tt.wantSubType)
			}
			if got.Amount != tt.amount {
				t.Errorf("Amount = %d, want %d", got.Amount, tt.amount)
			}
		})
	}
}

func TestNewCredit(t *testing.T) {
	tests := []struct {
		name     string
		amount   Amount
		subType  SubType
		unfreeze Amount
		wantErr  error
	}{
		{name: "派彩", amount: 250, subType: SubTypeWin},
		{name: "每日簽到", amount: 50, subType: SubTypeCheckin},
		{name: "月度累計簽到", amount: 500, subType: SubTypeMonthlyReward},
		{name: "GM 後台發幣", amount: 10000, subType: SubTypeGMReward},
		{name: "破產救助", amount: 100, subType: SubTypeBankruptcyAid},
		{name: "退款／本金返還", amount: 300, subType: SubTypeRefund},
		{
			// 對稱檢查：扣款子類型不能用在入帳。
			name: "BET 不能用在入帳", amount: 100, subType: SubTypeBet,
			wantErr: ErrSubTypeDirectionMismatch,
		},
		{
			name: "SHOP_PURCHASE 不能用在入帳", amount: 100, subType: SubTypeShopPurchase,
			wantErr: ErrSubTypeDirectionMismatch,
		},
		{
			// credit 沒有「合理的預設子類型」，所以空字串是錯誤而不是預設值。
			name: "subType 必填", amount: 100, subType: "",
			wantErr: ErrUnknownSubType,
		},
		{
			// 對齊 Java 的 @PositiveOrZero：0 是**正常值**不是「沒帶」。
			name: "解凍 0 是合法的", amount: 250, subType: SubTypeWin, unfreeze: 0,
		},
		{
			name: "帶解凍", amount: 250, subType: SubTypeWin, unfreeze: 100,
		},
		{
			// ⚠️ 這裡**不**檢查 unfreeze 是否超過目前凍結金額——那要等到 store
			// 那層讀到真正的 frozen_amount 才知道，而且 Java 的做法是夾住不是拒絕。
			name:   "解凍金額超過任何合理值仍然合法（夾住是 store 的事）",
			amount: 250, subType: SubTypeWin, unfreeze: 1 << 40,
		},
		{
			// ⭐ 負的解凍會讓 store 那層的 frozen_amount **變大**，於是可用餘額
			// 憑空變小，症狀是之後下注拿到「餘額不足」而餘額看起來明明夠。
			// schema 的 CHECK (frozen_amount >= 0) 擋不住這個方向。
			name: "解凍不可為負", amount: 250, subType: SubTypeWin, unfreeze: -1,
			wantErr: ErrInvalidUnfreezeAmount,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewCredit(42, tt.amount, tt.subType, "credit-42-"+tt.name, "round-1", tt.unfreeze)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if got.Type != TxTypeCredit {
				t.Errorf("Type = %q, want %q", got.Type, TxTypeCredit)
			}
			if got.ReferenceID != "round-1" {
				t.Errorf("ReferenceID = %q, want %q", got.ReferenceID, "round-1")
			}
			if got.UnfreezeAmount != tt.unfreeze {
				t.Errorf("UnfreezeAmount = %d, want %d", got.UnfreezeAmount, tt.unfreeze)
			}
		})
	}
}

// TestUnfreezeOnlyOnCredit 釘住「只有 CREDIT 可以解凍」這條不變式。
//
// ⚠️ 走公開建構子時觸發不了（NewDebit 硬編 unfreeze=0），所以直接呼叫 newMovement。
// 這正是「測試與被測程式同一個套件」的價值：不變式測得到，而不是只能靠註解宣稱。
// 沒有這條，一個手工組出來的 Movement{Type: DEBIT, UnfreezeAmount: 500} 會在
// store 那層被**靜靜忽略**——不會報錯，只會有一個沒被解凍的凍結金額。
func TestUnfreezeOnlyOnCredit(t *testing.T) {
	_, err := newMovement(42, TxTypeDebit, SubTypeBet, 100, 500, "debit-with-unfreeze", "")
	if !errors.Is(err, ErrUnfreezeNotAllowed) {
		t.Fatalf("err = %v, want errors.Is(err, %v)", err, ErrUnfreezeNotAllowed)
	}

	// 反向：DEBIT 帶 unfreeze=0 是正常的。
	if _, err := newMovement(42, TxTypeDebit, SubTypeBet, 100, 0, "debit-no-unfreeze", ""); err != nil {
		t.Fatalf("DEBIT 帶 unfreeze=0 應該合法，得到: %v", err)
	}
}

func TestIdempotencyKeyLength(t *testing.T) {
	// ⚠️ 邊界用中文字測是刻意的：MySQL 的 VARCHAR(100) 算字元、Go 的 len() 算位元組。
	// 只用 ASCII 測資的話，把 utf8.RuneCountInString 換成 len() 這個 bug 測不出來。
	tests := []struct {
		name    string
		key     string
		wantErr error
	}{
		{name: "剛好 100 個 ASCII 字元", key: strings.Repeat("a", 100)},
		{name: "101 個 ASCII 字元", key: strings.Repeat("a", 101), wantErr: ErrIdempotencyKeyTooLong},
		{
			// 100 個中文字 = 300 bytes。用 len() 驗證會誤拒，用 RuneCount 才對。
			name: "剛好 100 個中文字（300 bytes）", key: strings.Repeat("鍵", 100),
		},
		{name: "101 個中文字", key: strings.Repeat("鍵", 101), wantErr: ErrIdempotencyKeyTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIdempotencyKey(tt.key)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("非預期錯誤: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tt.wantErr)
			}
		})
	}
}

// TestIdempotencyKeyIsCaseSensitive 釘住「應用層不做大小寫正規化」這個決定。
//
// 冪等鍵的相等性由 DB 的 UNIQUE 索引定義（schema 指定 utf8mb4_bin）。
// 如果哪天有人在這裡加上 ToUpper，Go 會認為兩把鍵相同、DB 認為不同，
// 兩邊各自都對，但合起來就是重複入帳。
func TestIdempotencyKeyIsCaseSensitive(t *testing.T) {
	lower, err := NewDebit(42, 100, SubTypeBet, "checkin-42", "")
	if err != nil {
		t.Fatalf("非預期錯誤: %v", err)
	}
	upper, err := NewDebit(42, 100, SubTypeBet, "CHECKIN-42", "")
	if err != nil {
		t.Fatalf("非預期錯誤: %v", err)
	}
	if lower.IdempotencyKey == upper.IdempotencyKey {
		t.Fatal("冪等鍵被正規化了——應用層不可改寫大小寫，相等性由 DB 的 utf8mb4_bin 定義")
	}
}

// TestSubTypeDirectionCoversSchema 釘住「常數清單」與「方向表」的一致性。
//
// AGENTS.md 地雷 #8：新增 sub_type 要四同步。這個測試至少讓
// 「加了常數卻忘了加進方向表」當場失敗，而不是等到執行時回一句
// 「未知的帳務子類型」。
func TestSubTypeDirectionCoversSchema(t *testing.T) {
	// 這份清單必須與 internal/platform/migrate/migrations/00001_wallet_schema.sql
	// 的 chk_wt_sub_type 一字不差。刻意手寫而不是從 map 生成——
	// 從 map 生成的話，map 漏一個這個測試就跟著漏一個。
	schemaSubTypes := []SubType{
		"BET", "SHOP_PURCHASE",
		"WIN", "CHECKIN", "TASK", "GIFT", "GM_REWARD", "BANKRUPTCY_AID",
		"DIAMOND_EXCHANGE", "TOPUP", "CASHBACK", "REFUND", "MONTHLY_REWARD",
	}

	if len(subTypeDirection) != len(schemaSubTypes) {
		t.Errorf("方向表有 %d 個子類型，schema 有 %d 個", len(subTypeDirection), len(schemaSubTypes))
	}
	for _, sub := range schemaSubTypes {
		if _, ok := DirectionOf(sub); !ok {
			t.Errorf("schema 的 %q 不在方向表裡", sub)
		}
	}
}
