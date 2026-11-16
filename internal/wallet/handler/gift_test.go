package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/model/enum"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

const giftPath = "/api/v1/wallet/gift"

// Gift 讓既有的 fakeService 繼續滿足 WalletService；非贈禮測試呼叫到它代表路由接錯了。
func (f *fakeService) Gift(context.Context, int64, int64, int64, string) (service.GiftResult, error) {
	return service.GiftResult{}, errors.New("fakeService.Gift 不該被呼叫")
}

// giftSvc 內嵌 fakeService（滿足 WalletService 其餘方法），只覆寫 Gift。
type giftSvc struct {
	fakeService
	result service.GiftResult
	err    error

	calls                 int
	sender, receiver, amt int64
	key                   string
}

func (g *giftSvc) Gift(_ context.Context, sender, receiver, amount int64, key string) (service.GiftResult, error) {
	g.calls++
	g.sender, g.receiver, g.amt, g.key = sender, receiver, amount, key
	return g.result, g.err
}

func postGift(t *testing.T, svc WalletService, userID *string, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, giftPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalSecretHeader, goodSecret)
	if userID != nil {
		req.Header.Set("X-User-Id", *userID)
	}
	resp, err := newTestApp(t, svc).Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func i64(v int64) *int64 { return &v }

func TestGiftSuccessShape(t *testing.T) {
	svc := &giftSvc{result: service.GiftResult{
		SenderID: 1, ReceiverID: 2, Amount: 300,
		DebitTransactionID: i64(10), CreditTransactionID: i64(11),
		SenderBalanceAfter: i64(700), ReceiverBalanceAfter: i64(350),
		Status: enum.GiftCompleted,
	}}
	status, body := postGift(t, svc, strp("1"), `{"receiverId":2,"amount":300,"idempotencyKey":"k1"}`)
	if status != http.StatusOK {
		t.Fatalf("狀態碼 = %d（body=%s）", status, body)
	}
	// 欄位順序對齊 Java GiftResponse。
	const want = `{"success":true,"data":{"senderId":1,"receiverId":2,"amount":300,` +
		`"debitTransactionId":10,"creditTransactionId":11,"senderBalanceAfter":700,` +
		`"receiverBalanceAfter":350,"idempotent":false},"message":null}`
	if body != want {
		t.Errorf("body 不符\n got: %s\nwant: %s", body, want)
	}
	if svc.sender != 1 || svc.receiver != 2 || svc.amt != 300 || svc.key != "k1" {
		t.Errorf("傳給 service 的參數不對: %+v", svc)
	}
}

// 錢已離開送出方、尚未到帳：尚未發生的欄位是 null，不是 0。
func TestGiftInFlightHasNullFields(t *testing.T) {
	svc := &giftSvc{result: service.GiftResult{
		SenderID: 1, ReceiverID: 2, Amount: 300,
		DebitTransactionID: i64(10), SenderBalanceAfter: i64(700),
		Status: enum.GiftDebited,
	}}
	_, body := postGift(t, svc, strp("1"), `{"receiverId":2,"amount":300,"idempotencyKey":"k1"}`)
	if !strings.Contains(body, `"creditTransactionId":null`) || !strings.Contains(body, `"receiverBalanceAfter":null`) {
		t.Errorf("未到帳的欄位應為 null: %s", body)
	}
}

func TestGiftValidation(t *testing.T) {
	long := strings.Repeat("x", 81)
	tests := []struct {
		name    string
		user    *string
		body    string
		status  int
		wantMsg string
	}{
		{"沒帶 X-User-Id", nil, `{}`, 400, "Missing X-User-Id header"},
		{"X-User-Id 不是數字", strp("abc"), `{}`, 400, "Invalid X-User-Id header"},
		{"壞 JSON", strp("1"), `{`, 400, "Invalid request body"},
		{"receiverId 沒帶", strp("1"), `{"amount":1,"idempotencyKey":"k"}`, 400, "Invalid request: receiverId must not be null"},
		{"amount 沒帶", strp("1"), `{"receiverId":2,"idempotencyKey":"k"}`, 400, "Invalid request: amount must not be null"},
		{"amount 為 0", strp("1"), `{"receiverId":2,"amount":0,"idempotencyKey":"k"}`, 400, "Invalid request: amount must be greater than 0"},
		{"amount 為負", strp("1"), `{"receiverId":2,"amount":-5,"idempotencyKey":"k"}`, 400, "Invalid request: amount must be greater than 0"},
		{"鍵全空白", strp("1"), `{"receiverId":2,"amount":1,"idempotencyKey":"   "}`, 400, "Invalid request: idempotencyKey must not be blank"},
		{"鍵 81 字", strp("1"), `{"receiverId":2,"amount":1,"idempotencyKey":"` + long + `"}`, 400, "Invalid request: idempotencyKey size must be between 0 and 80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &giftSvc{}
			status, body := postGift(t, svc, tt.user, tt.body)
			want := `{"success":false,"data":null,"message":"` + tt.wantMsg + `"}`
			if status != tt.status || body != want {
				t.Errorf("得到 %d %s\nwant %d %s", status, body, tt.status, want)
			}
			if svc.calls != 0 {
				t.Error("驗證失敗不可呼叫 service")
			}
		})
	}
}

func TestGiftKeyOf80RunesIsAccepted(t *testing.T) {
	svc := &giftSvc{}
	key := strings.Repeat("好", 80) // 240 位元組、80 字元：用 len() 會誤拒
	status, body := postGift(t, svc, strp("1"), `{"receiverId":2,"amount":1,"idempotencyKey":"`+key+`"}`)
	if status != http.StatusOK || svc.calls != 1 {
		t.Fatalf("80 個中文字元的鍵應通過: %d %s calls=%d", status, body, svc.calls)
	}
}

func TestGiftErrorMapping(t *testing.T) {
	e := func(c service.Code) error { return &service.Error{Code: c, Err: errors.New("x")} }
	tests := []struct {
		name    string
		err     error
		status  int
		wantMsg string
	}{
		{"送給自己", e(service.CodeInvalidGift), 400, "Cannot gift to yourself"},
		{"送出超限", e(service.CodeGiftSentLimitExceeded), 422, "Daily gift-sent limit exceeded (limit 5000)"},
		{"收受超限", e(service.CodeGiftRecvLimitExceeded), 422, "Receiver daily gift-received limit exceeded (limit 10000)"},
		{"餘額不足", e(service.CodeInsufficientBalance), 422, "Insufficient balance"},
		{"送出方錢包不存在", e(service.CodeGiftSenderNotFound), 404, "Wallet not found for sender: 1"},
		{"接收方錢包不存在", e(service.CodeGiftReceiverNotFound), 404, "Wallet not found for receiver: 2"},
		{"已退款", e(service.CodeGiftRefunded), 409, "Gift could not be delivered and the amount was refunded"},
		{"鍵屬於別人", e(service.CodeGiftKeyConflict), 409, "Idempotency key already used by another gift"},
		{"內部錯誤不外洩", &service.Error{Code: service.CodeInternal, Err: errors.New("dial tcp 10.0.0.5:6380")}, 500, "Internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := postGift(t, &giftSvc{err: tt.err}, strp("1"), `{"receiverId":2,"amount":1,"idempotencyKey":"k"}`)
			want := `{"success":false,"data":null,"message":"` + tt.wantMsg + `"}`
			if status != tt.status || body != want {
				t.Errorf("得到 %d %s\nwant %d %s", status, body, tt.status, want)
			}
		})
	}
}
