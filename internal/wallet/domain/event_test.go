package domain

import (
	"encoding/json"
	"testing"
)

// TestDebitEventJSON 釘住 `wallet.debit` 的 payload 逐位元組長什麼樣。
//
// ⚠️ 為什麼比對**字串**而不是反序列化回來比欄位：outbox 的 payload 欄位存的是
// 原始 JSON 字串，poller 原封不動搬進 Kafka，跨語言契約測試會直接 diff 它。
// 比對欄位的測試會放過「欄位名從 subType 變成 sub_type」這種改動，
// 而那正是會讓 Java 版消費端整批落到 default 分支的改動。
//
// 對照來源：`wallet-service/.../kafka/WalletDebitEvent.java`（record，8 個 component）。
func TestDebitEventJSON(t *testing.T) {
	ref := "round-9527"

	tests := []struct {
		name  string
		event DebitEvent
		want  string
	}{
		{
			name: "帶 referenceId",
			event: DebitEvent{
				TransactionID:  1001,
				PlayerID:       42,
				Amount:         100,
				BalanceBefore:  1000,
				BalanceAfter:   900,
				SubType:        SubTypeBet,
				IdempotencyKey: "bet-42-9527",
				ReferenceID:    &ref,
			},
			want: `{"transactionId":1001,"playerId":42,"amount":100,` +
				`"balanceBefore":1000,"balanceAfter":900,"subType":"BET",` +
				`"idempotencyKey":"bet-42-9527","referenceId":"round-9527"}`,
		},
		{
			// ⭐ 這一格才是重點：Java 沒帶 referenceId 時輸出的是 null，不是 ""。
			// 用 string 而不是 *string 的話這格會是 `"referenceId":""`，
			// 而且**兩邊都不會報錯**——只有下游的查詢會少資料。
			name: "未帶 referenceId 時是 null 而不是空字串",
			event: DebitEvent{
				TransactionID:  1002,
				PlayerID:       42,
				Amount:         50,
				BalanceBefore:  900,
				BalanceAfter:   850,
				SubType:        SubTypeShopPurchase,
				IdempotencyKey: "shop-42-1",
				ReferenceID:    OptionalString(""),
			},
			want: `{"transactionId":1002,"playerId":42,"amount":50,` +
				`"balanceBefore":900,"balanceAfter":850,"subType":"SHOP_PURCHASE",` +
				`"idempotencyKey":"shop-42-1","referenceId":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.event)
			if err != nil {
				t.Fatalf("序列化失敗: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("payload 與 Java 版不一致\ngot:  %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestOptionalString(t *testing.T) {
	if got := OptionalString(""); got != nil {
		t.Errorf("空字串應轉成 nil（SQL NULL / JSON null），得到 %q", *got)
	}
	got := OptionalString("x")
	if got == nil || *got != "x" {
		t.Errorf("非空字串應原樣保留，得到 %v", got)
	}
}
