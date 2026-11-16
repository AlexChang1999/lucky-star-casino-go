package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

// 與下一個測試是一對，釘住地雷 #33 在 HTTP 層的表現：frozenAfter 有值與「明確是 null」必須分得出來。
func TestCreditSuccessCarriesFrozenAfter(t *testing.T) {
	frozen := int64(250)
	svc := &fakeService{creditResult: service.CreditResult{
		TransactionID: 11, PlayerID: 42, Amount: 500, BalanceBefore: 1000, BalanceAfter: 1500, FrozenAfter: &frozen,
	}}

	status, body := post(t, svc, "/internal/wallet/credit",
		`{"playerId":42,"amount":500,"subType":"WIN","idempotencyKey":"win-42-1","unfreezeAmount":50}`)

	if status != http.StatusOK {
		t.Fatalf("狀態碼 = %d, want 200（body=%s）", status, body)
	}
	const want = `{"success":true,"data":{"transactionId":11,"playerId":42,"amount":500,` +
		`"balanceBefore":1000,"balanceAfter":1500,"frozenAfter":250,"idempotent":false},"message":null}`
	if body != want {
		t.Errorf("回應 body 不符\n got: %s\nwant: %s", body, want)
	}
	if svc.received.UnfreezeAmount != 50 {
		t.Errorf("UnfreezeAmount = %d, want 50", svc.received.UnfreezeAmount)
	}
}

func TestCreditIdempotentHitReturnsNullFrozenAfter(t *testing.T) {
	svc := &fakeService{creditResult: service.CreditResult{
		TransactionID: 11, PlayerID: 42, Amount: 500, BalanceBefore: 1000, BalanceAfter: 1500,
		FrozenAfter: nil, // 對齊 Java 的 `.frozenAfter(null)`
		Idempotent:  true,
	}}

	status, body := post(t, svc, "/internal/wallet/credit",
		`{"playerId":42,"amount":500,"subType":"WIN","idempotencyKey":"win-42-1"}`)

	if status != http.StatusOK {
		t.Fatalf("狀態碼 = %d, want 200（body=%s）", status, body)
	}
	// ⭐ 一定要看**原始位元組**：`null` 與 `0` 解進 int64 都是 0，用結構斷言會綠得毫無意義。
	if !strings.Contains(body, `"frozenAfter":null`) {
		t.Errorf("冪等命中時 frozenAfter 必須是 null，得到: %s", body)
	}
	if svc.received.UnfreezeAmount != 0 {
		t.Errorf("unfreezeAmount 未帶時應為 0，得到 %d", svc.received.UnfreezeAmount)
	}
}

func TestCreditValidation(t *testing.T) {
	const creditPattern = "WIN/CHECKIN/TASK/GIFT/GM_REWARD/BANKRUPTCY_AID/DIAMOND_EXCHANGE/TOPUP/CASHBACK/REFUND/MONTHLY_REWARD"
	tests := []struct{ name, body, wantMessage string }{
		{"未帶 subType（credit 沒有預設值）", `{"playerId":42,"amount":100,"idempotencyKey":"k"}`,
			"Invalid request: subType must not be blank"},
		{"帶了 DEBIT 類的 subType", `{"playerId":42,"amount":100,"subType":"BET","idempotencyKey":"k"}`,
			"Invalid request: subType subType must be one of " + creditPattern},
		{"unfreezeAmount 是負數", `{"playerId":42,"amount":100,"subType":"WIN","idempotencyKey":"k","unfreezeAmount":-1}`,
			"Invalid request: unfreezeAmount must be greater than or equal to 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{}
			status, body := post(t, svc, "/internal/wallet/credit", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("狀態碼 = %d, want 400（body=%s）", status, body)
			}
			if svc.calls != 0 {
				t.Errorf("驗證失敗時不該呼叫 service，卻呼叫了 %d 次", svc.calls)
			}
			var got struct {
				Message *string `json:"message"`
			}
			_ = json.Unmarshal([]byte(body), &got)
			if got.Message == nil || *got.Message != tt.wantMessage {
				t.Errorf("message = %v\nwant %q", got.Message, tt.wantMessage)
			}
		})
	}
}
