package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

// fakeService 是 WalletService 的測試替身。只有宣告過的方法——這就是在消費端定義小介面換到的東西。
//
// ⚠️ 它也**記下收到的 Movement**。這一層最容易寫錯的不是回應格式，而是「請求欄位有沒有
// 正確翻成帳務意圖」（例如 subType 沒帶時有沒有變成 BET）。只斷言狀態碼測不到那類 bug。
type fakeService struct {
	debitResult service.DebitResult
	debitErr    error

	calls    int
	received domain.Movement
}

func (f *fakeService) Debit(_ context.Context, m domain.Movement) (service.DebitResult, error) {
	f.calls++
	f.received = m
	return f.debitResult, f.debitErr
}

var goodSecret = testSecret

func post(t *testing.T, svc WalletService, path, body string) (int, string) {
	t.Helper()
	return send(t, newTestApp(t, svc), http.MethodPost, path, body, &goodSecret)
}

func TestDebitSuccess(t *testing.T) {
	svc := &fakeService{debitResult: service.DebitResult{
		TransactionID: 7, PlayerID: 42, Amount: 100, BalanceBefore: 1000, BalanceAfter: 900,
	}}

	status, body := post(t, svc, "/internal/wallet/debit",
		`{"playerId":42,"amount":100,"idempotencyKey":"bet-42-1","referenceId":"round-9"}`)

	if status != http.StatusOK {
		t.Fatalf("狀態碼 = %d, want 200（body=%s）", status, body)
	}
	// ⭐ 逐位元組比對，守的是**信封的形狀**：有人給 envelope 加 omitempty，
	// `"message":null` 就會消失（地雷 #33），那是契約測試才會發現的漂移。
	const want = `{"success":true,"data":{"transactionId":7,"playerId":42,"amount":100,` +
		`"balanceBefore":1000,"balanceAfter":900,"idempotent":false},"message":null}`
	if body != want {
		t.Errorf("回應 body 不符\n got: %s\nwant: %s", body, want)
	}
	if svc.received.SubType != domain.SubTypeBet {
		t.Errorf("subType 未帶時應預設 BET，得到 %q", svc.received.SubType)
	}
	if svc.received.Type != domain.TxTypeDebit {
		t.Errorf("Type = %q, want DEBIT", svc.received.Type)
	}
	if svc.received.ReferenceID != "round-9" {
		t.Errorf("ReferenceID = %q, want round-9", svc.received.ReferenceID)
	}
}

// TestDebitErrorMapping 是 service 結果碼 → HTTP 的核心表格。
// ⚠️ 每一格都來自逐行讀過的 Java GlobalExceptionHandler；特別是餘額不足是 **422 不是 400**。
func TestDebitErrorMapping(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{"錢包不存在 → 404", &service.Error{Code: service.CodeWalletNotFound, Err: errors.New("x")},
			http.StatusNotFound, "Wallet not found for player: 42"},
		{"餘額不足 → 422", &service.Error{Code: service.CodeInsufficientBalance, Err: errors.New("x")},
			http.StatusUnprocessableEntity, "Insufficient balance"},
		{"樂觀鎖衝突 → 409", &service.Error{Code: service.CodeConcurrentModification, Err: errors.New("x")},
			http.StatusConflict, "Concurrent modification detected, please retry"},
		{"內部錯誤 → 500", &service.Error{Code: service.CodeInternal, Err: errors.New("x")},
			http.StatusInternalServerError, "Internal server error"},
		// 不是 *service.Error 的錯誤（不該發生）一律當 500，不可變成某個 4xx。
		{"非 service.Error → 500", errors.New("boom"),
			http.StatusInternalServerError, "Internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(t, &fakeService{debitErr: tt.err}, "/internal/wallet/debit",
				`{"playerId":42,"amount":100,"idempotencyKey":"bet-42-1"}`)
			if status != tt.wantStatus {
				t.Errorf("狀態碼 = %d, want %d（body=%s）", status, tt.wantStatus, body)
			}
			var got struct {
				Success bool            `json:"success"`
				Data    json.RawMessage `json:"data"`
				Message *string         `json:"message"`
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("回應不是合法 JSON（%s）: %v", body, err)
			}
			if got.Success || string(got.Data) != "null" {
				t.Errorf("錯誤回應 success 必須 false、data 必須 null，得到 %s", body)
			}
			if got.Message == nil || *got.Message != tt.wantMessage {
				t.Errorf("message = %v, want %q", got.Message, tt.wantMessage)
			}
		})
	}
}

// TestInternalErrorDoesNotLeakDetails 守「500 不可以把內部訊息吐出去」：
// 把 err.Error() 直接回給呼叫端是很自然的「改進」，會把連線字串、內部主機名送到外面。
func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	svc := &fakeService{debitErr: &service.Error{Code: service.CodeInternal,
		Err: errors.New("connection refused to 10.0.0.7:3306")}}
	_, body := post(t, svc, "/internal/wallet/debit", `{"playerId":42,"amount":100,"idempotencyKey":"k"}`)
	if strings.Contains(body, "10.0.0.7") {
		t.Errorf("500 回應外洩了內部細節: %s", body)
	}
}

// TestDebitValidation 逐條對齊 Java DTO 的 Bean Validation 註解，訊息逐字比對。
// ⚠️ 每一格只錯**一個**欄位（Java 多錯時挑哪個不保證）。
func TestDebitValidation(t *testing.T) {
	tests := []struct {
		name, body, wantMessage string
	}{
		{"playerId 未帶", `{"amount":100,"idempotencyKey":"k"}`, "Invalid request: playerId must not be null"},
		{"playerId 是 null", `{"playerId":null,"amount":100,"idempotencyKey":"k"}`, "Invalid request: playerId must not be null"},
		// 刻意與 Java 分歧：Java 是 404。見 endpoint.validationError。
		{"playerId 非正整數", `{"playerId":0,"amount":100,"idempotencyKey":"k"}`, "Invalid request: playerId must be positive"},
		{"amount 未帶", `{"playerId":42,"idempotencyKey":"k"}`, "Invalid request: amount must not be null"},
		// ⭐ 「選填欄位必須用指標」的全部理由：未帶與 0 在 Java 是兩個不同的錯誤訊息。
		{"amount 是 0", `{"playerId":42,"amount":0,"idempotencyKey":"k"}`, "Invalid request: amount must be greater than 0"},
		{"amount 是負數", `{"playerId":42,"amount":-1,"idempotencyKey":"k"}`, "Invalid request: amount must be greater than 0"},
		{"idempotencyKey 未帶", `{"playerId":42,"amount":100}`, "Invalid request: idempotencyKey must not be blank"},
		// 少了 HTTP 層這一條，由空白組成的冪等鍵會真的被寫進帳務流水。
		{"idempotencyKey 全是空白", `{"playerId":42,"amount":100,"idempotencyKey":"   "}`, "Invalid request: idempotencyKey must not be blank"},
		{"idempotencyKey 超過 100 字元", `{"playerId":42,"amount":100,"idempotencyKey":"` + strings.Repeat("k", 101) + `"}`,
			"Invalid request: idempotencyKey size must be between 0 and 100"},
		// 少了這一條，101 字元的 referenceId 會撞 MySQL 1406 → 500，而 Java 是 400。
		{"referenceId 超過 100 字元", `{"playerId":42,"amount":100,"idempotencyKey":"k","referenceId":"` + strings.Repeat("r", 101) + `"}`,
			"Invalid request: referenceId size must be between 0 and 100"},
		{"不在白名單的 subType", `{"playerId":42,"amount":100,"subType":"NOPE","idempotencyKey":"k"}`,
			"Invalid request: subType subType must be one of BET/SHOP_PURCHASE"},
		// ⭐ 方向配對只存在於應用層，DB 的兩個 CHECK 擋不住 DEBIT + WIN 這種組合。
		{"debit 帶了 CREDIT 類的 subType", `{"playerId":42,"amount":100,"subType":"WIN","idempotencyKey":"k"}`,
			"Invalid request: subType subType must be one of BET/SHOP_PURCHASE"},
		// 「沒帶」預設 BET，「帶了空字串」是 @Pattern 不符。用 string 而非 *string 這格會變 200。
		{"空字串 subType 不等於沒帶", `{"playerId":42,"amount":100,"subType":"","idempotencyKey":"k"}`,
			"Invalid request: subType subType must be one of BET/SHOP_PURCHASE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{}
			status, body := post(t, svc, "/internal/wallet/debit", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("狀態碼 = %d, want 400（body=%s）", status, body)
			}
			// ⭐ 驗證失敗**不可以**碰到 service：少了它，「先扣款再驗證」的重構會靜靜通過。
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

// TestMalformedBodyIsRejected 釘住壞 JSON → 400（刻意與 Java 的 500 分歧，藍圖 §5）。
func TestMalformedBodyIsRejected(t *testing.T) {
	tests := []struct{ name, body string }{
		{"不是 JSON", `{not json`},
		{"空 body", ``},
		{"型別錯誤", `{"playerId":"forty-two","amount":100,"idempotencyKey":"k"}`},
		{"amount 超過 int64", `{"playerId":42,"amount":99999999999999999999,"idempotencyKey":"k"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{}
			status, body := post(t, svc, "/internal/wallet/debit", tt.body)
			if status != http.StatusBadRequest {
				t.Errorf("狀態碼 = %d, want 400（body=%s）", status, body)
			}
			if svc.calls != 0 {
				t.Errorf("解不開的 body 不該碰到 service，卻呼叫了 %d 次", svc.calls)
			}
		})
	}
}

// TestUnknownFieldsAreIgnored 釘住向前相容。⚠️ 不要加 DisallowUnknownFields：
// 那會讓「呼叫端先部署了帶新欄位的版本」變成整批 400，而那正是滾動部署的常態。
func TestUnknownFieldsAreIgnored(t *testing.T) {
	status, body := post(t, &fakeService{}, "/internal/wallet/debit",
		`{"playerId":42,"amount":100,"idempotencyKey":"k","futureField":"whatever"}`)
	if status != http.StatusOK {
		t.Errorf("狀態碼 = %d, want 200（body=%s）", status, body)
	}
}
