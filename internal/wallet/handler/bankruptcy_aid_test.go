package handler

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
)

const aidPath = "/api/v1/wallet/bankruptcy-aid"

// postAid 送 POST，userID=nil 代表不帶 X-User-Id，secret=nil 代表不帶 X-Internal-Secret。
func postAid(t *testing.T, svc WalletService, userID, secret *string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, aidPath, nil)
	if userID != nil {
		req.Header.Set("X-User-Id", *userID)
	}
	if secret != nil {
		req.Header.Set(internalSecretHeader, *secret)
	}
	resp, err := newTestApp(t, svc).Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func strp(s string) *string { return &s }

func TestBankruptcyAidSuccess(t *testing.T) {
	svc := &fakeService{aidResult: service.BankruptcyAidResult{
		PlayerID: 7, Amount: 1000, TransactionID: 9, BalanceBefore: 50, BalanceAfter: 1050,
	}}
	status, body := postAid(t, svc, strp("7"), &goodSecret)
	if status != http.StatusOK {
		t.Fatalf("狀態碼 = %d（body=%s）", status, body)
	}
	// 欄位順序對齊 Java BankruptcyAidResponse：playerId, amount, transactionId, balanceBefore, balanceAfter。
	const want = `{"success":true,"data":{"playerId":7,"amount":1000,"transactionId":9,` +
		`"balanceBefore":50,"balanceAfter":1050},"message":null}`
	if body != want {
		t.Errorf("body 不符\n got: %s\nwant: %s", body, want)
	}
	if svc.aidPlayer != 7 {
		t.Errorf("傳給 service 的 playerID = %d, want 7", svc.aidPlayer)
	}
}

// 玩家身分只來自 X-User-Id；body 與 query 不可影響它（只能領自己的）。
func TestBankruptcyAidHeaderValidation(t *testing.T) {
	tests := []struct {
		name    string
		userID  *string
		wantMsg string
	}{
		{"沒帶", nil, "Missing X-User-Id header"},
		{"空字串", strp(""), "Missing X-User-Id header"},
		{"全空白", strp("   "), "Missing X-User-Id header"},
		{"不是數字", strp("abc"), "Invalid X-User-Id header"},
		{"小數", strp("1.5"), "Invalid X-User-Id header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{}
			status, body := postAid(t, svc, tt.userID, &goodSecret)
			want := `{"success":false,"data":null,"message":"` + tt.wantMsg + `"}`
			if status != http.StatusBadRequest || body != want {
				t.Errorf("得到 %d %s, want 400 %s", status, body, want)
			}
			if svc.aidCalls != 0 {
				t.Error("驗證失敗時不可呼叫 service")
			}
		})
	}
}

func TestBankruptcyAidErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		// 兩句 422 的文字不同，逐字對齊 Java BankruptcyAidService。
		{"餘額太高", &service.Error{Code: service.CodeAidBalanceTooHigh, Err: errors.New("x")},
			422, "Balance not below threshold (100), not eligible for bankruptcy aid"},
		{"今天領過", &service.Error{Code: service.CodeAidAlreadyClaimed, Err: errors.New("x")},
			422, "Bankruptcy aid already claimed today"},
		{"錢包不存在", &service.Error{Code: service.CodeWalletNotFound, Err: errors.New("x")},
			404, "Wallet not found for player: 7"},
		{"樂觀鎖衝突", &service.Error{Code: service.CodeConcurrentModification, Err: errors.New("x")},
			409, "Concurrent modification detected, please retry"},
		{"內部錯誤不外洩", &service.Error{Code: service.CodeInternal, Err: errors.New("dial tcp 10.0.0.5:6380")},
			500, "Internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := postAid(t, &fakeService{aidErr: tt.err}, strp("7"), &goodSecret)
			want := `{"success":false,"data":null,"message":"` + tt.wantMsg + `"}`
			if status != tt.wantStatus || body != want {
				t.Errorf("得到 %d %s\nwant %d %s", status, body, tt.wantStatus, want)
			}
		})
	}
}

// 選項 1 的前提：玩家路由沿用 X-Internal-Secret，信任邊界由 gateway 負責。
// ⚠️ 這比 Java 嚴（Java 的 /api/v1/** 不驗 secret）——gateway 未就緒前，沒有它
// 任何人都能用 X-User-Id 冒充玩家。
func TestPlayerRoutesRequireInternalSecret(t *testing.T) {
	svc := &fakeService{}
	for _, secret := range []*string{nil, strp("wrong")} {
		status, body := postAid(t, svc, strp("7"), secret)
		const want = `{"success":false,"data":null,"message":"Unauthorized"}`
		if status != http.StatusUnauthorized || body != want {
			t.Errorf("得到 %d %s", status, body)
		}
	}
	if svc.aidCalls != 0 {
		t.Error("未通過 secret 驗證不可呼叫 service")
	}
}
