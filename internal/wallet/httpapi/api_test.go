package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

const testSecret = "test-internal-secret"

// fakeStore 是 Store 介面的測試替身。
//
// ⭐ 它只有兩個方法，因為 Store 只宣告了兩個方法——那正是「在消費端定義
// 小介面」換到的東西（CLAUDE.md §2）。若介面由 store 套件定義成
// 「Repository 的全部方法」，這個假物件每次 store 長大就得跟著長。
//
// ⚠️ 它也**記下收到的 Movement**。這一層最容易寫錯的不是回應格式，而是
// 「請求欄位有沒有正確地翻成帳務意圖」——例如 subType 沒帶時有沒有變成 BET。
// 只斷言 HTTP 狀態碼的話，那類 bug 完全測不到。
type fakeStore struct {
	debitResult  walletstore.DebitResult
	debitErr     error
	creditResult walletstore.CreditResult
	creditErr    error

	calls    int
	received domain.Movement
}

func (f *fakeStore) Debit(_ context.Context, m domain.Movement) (walletstore.DebitResult, error) {
	f.calls++
	f.received = m
	return f.debitResult, f.debitErr
}

func (f *fakeStore) Credit(_ context.Context, m domain.Movement) (walletstore.CreditResult, error) {
	f.calls++
	f.received = m
	return f.creditResult, f.creditErr
}

// newTestHandler 組出一個丟棄日誌的 handler。
//
// 日誌走 io.Discard 是刻意的：這一層在 4xx / 5xx 路徑上會記不少東西，
// 讓它們噴進測試輸出會蓋掉真正的失敗訊息。
func newTestHandler(t *testing.T, store Store) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(store, logger, testSecret)
	if err != nil {
		t.Fatalf("組裝 handler 失敗: %v", err)
	}
	return h
}

// do 送一個帶合法 secret 的請求。
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalSecretHeader, testSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeEnvelope 把回應解成信封 + 原始 data，讓斷言能分兩段做。
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (success bool, message *string, data json.RawMessage) {
	t.Helper()
	var got struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Message *string         `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("回應不是合法 JSON（%s）: %v", rec.Body.String(), err)
	}
	return got.Success, got.Message, got.Data
}

// ── 成功路徑 ────────────────────────────────────────────────────────────────

func TestDebitSuccess(t *testing.T) {
	store := &fakeStore{debitResult: walletstore.DebitResult{
		TransactionID: 7,
		PlayerID:      42,
		Amount:        100,
		BalanceBefore: 1000,
		BalanceAfter:  900,
		Idempotent:    false,
	}}
	h := newTestHandler(t, store)

	rec := do(t, h, http.MethodPost, "/internal/wallet/debit",
		`{"playerId":42,"amount":100,"idempotencyKey":"bet-42-1","referenceId":"round-9"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("狀態碼 = %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}

	// ⭐ 逐位元組比對。這裡守的是**信封的形狀**：只要有人給 envelope 的欄位
	// 加上 omitempty，`"message":null` 就會整個消失（地雷 #33），而那是
	// 契約測試才會發現的漂移——除非這裡先擋住。
	const want = `{"success":true,"data":{"transactionId":7,"playerId":42,"amount":100,` +
		`"balanceBefore":1000,"balanceAfter":900,"idempotent":false},"message":null}`
	if got := rec.Body.String(); got != want {
		t.Errorf("回應 body 不符\n got: %s\nwant: %s", got, want)
	}

	// 請求欄位有沒有正確翻成帳務意圖。
	if store.received.SubType != domain.SubTypeBet {
		t.Errorf("subType 未帶時應預設 BET，得到 %q", store.received.SubType)
	}
	if store.received.Type != domain.TxTypeDebit {
		t.Errorf("Type = %q, want DEBIT", store.received.Type)
	}
	if store.received.ReferenceID != "round-9" {
		t.Errorf("ReferenceID = %q, want round-9", store.received.ReferenceID)
	}
}

func TestDebitAcceptsShopPurchaseSubType(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(t, store)

	rec := do(t, h, http.MethodPost, "/internal/wallet/debit",
		`{"playerId":42,"amount":100,"subType":"SHOP_PURCHASE","idempotencyKey":"shop-42-1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("狀態碼 = %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}
	if store.received.SubType != domain.SubTypeShopPurchase {
		t.Errorf("SubType = %q, want SHOP_PURCHASE", store.received.SubType)
	}
}

// TestCreditSuccessCarriesFrozenAfter 與下面那個是一對，釘住地雷 #33 在
// HTTP 層的表現：frozenAfter 有值與「明確是 null」必須分得出來。
func TestCreditSuccessCarriesFrozenAfter(t *testing.T) {
	frozen := domain.Amount(250)
	store := &fakeStore{creditResult: walletstore.CreditResult{
		TransactionID: 11,
		PlayerID:      42,
		Amount:        500,
		BalanceBefore: 1000,
		BalanceAfter:  1500,
		FrozenAfter:   &frozen,
		Idempotent:    false,
	}}
	h := newTestHandler(t, store)

	rec := do(t, h, http.MethodPost, "/internal/wallet/credit",
		`{"playerId":42,"amount":500,"subType":"WIN","idempotencyKey":"win-42-1","unfreezeAmount":50}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("狀態碼 = %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}
	const want = `{"success":true,"data":{"transactionId":11,"playerId":42,"amount":500,` +
		`"balanceBefore":1000,"balanceAfter":1500,"frozenAfter":250,"idempotent":false},"message":null}`
	if got := rec.Body.String(); got != want {
		t.Errorf("回應 body 不符\n got: %s\nwant: %s", got, want)
	}
	if store.received.UnfreezeAmount != 50 {
		t.Errorf("UnfreezeAmount = %d, want 50", store.received.UnfreezeAmount)
	}
}

func TestCreditIdempotentHitReturnsNullFrozenAfter(t *testing.T) {
	store := &fakeStore{creditResult: walletstore.CreditResult{
		TransactionID: 11,
		PlayerID:      42,
		Amount:        500,
		BalanceBefore: 1000,
		BalanceAfter:  1500,
		// ⚠️ nil，對齊 Java 的 `.frozenAfter(null)`（WalletService:179、:229）：
		// 「冪等命中不重算凍結；以當初入帳結果為準」。
		FrozenAfter: nil,
		Idempotent:  true,
	}}
	h := newTestHandler(t, store)

	rec := do(t, h, http.MethodPost, "/internal/wallet/credit",
		`{"playerId":42,"amount":500,"subType":"WIN","idempotencyKey":"win-42-1"}`)

	// ⭐ 這裡一定要看**原始位元組**而不是解出來的結構：`"frozenAfter":null` 與
	// `"frozenAfter":0` 解進 *int64 是 nil 與 &0，但解進 int64 兩者都是 0。
	// 而 0 是一個合法的凍結金額——用錯型別的話這個測試會綠得毫無意義。
	if !strings.Contains(rec.Body.String(), `"frozenAfter":null`) {
		t.Errorf("冪等命中時 frozenAfter 必須是 null，得到: %s", rec.Body.String())
	}
	if store.received.UnfreezeAmount != 0 {
		t.Errorf("unfreezeAmount 未帶時應為 0，得到 %d", store.received.UnfreezeAmount)
	}
}

// ── 錯誤 → 狀態碼 ───────────────────────────────────────────────────────────

// TestStoreErrorMapping 是這個切片的核心表格。
//
// ⚠️ 每一格的 status 都來自逐行讀過的 Java `GlobalExceptionHandler`，
// **不是憑直覺**。特別是餘額不足是 **422 不是 400**（:23-27）。
func TestStoreErrorMapping(t *testing.T) {
	tests := []struct {
		name        string
		storeErr    error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "錢包不存在 → 404",
			storeErr:    fmt.Errorf("%w: playerID=42", walletstore.ErrWalletNotFound),
			wantStatus:  http.StatusNotFound,
			wantMessage: "Wallet not found for player: 42",
		},
		{
			name:        "餘額不足 → 422（不是 400）",
			storeErr:    fmt.Errorf("%w: playerID=42 需要 100", walletstore.ErrInsufficientBalance),
			wantStatus:  http.StatusUnprocessableEntity,
			wantMessage: "Insufficient balance",
		},
		{
			name:        "樂觀鎖衝突 → 409",
			storeErr:    fmt.Errorf("%w: playerID=42 version=3", walletstore.ErrConcurrentModification),
			wantStatus:  http.StatusConflict,
			wantMessage: "Concurrent modification detected, please retry",
		},
		{
			name: "冪等鍵衝突卻找不到贏家 → 500（對齊 Java 的 IllegalStateException）",
			storeErr: fmt.Errorf("%w: key=%q",
				walletstore.ErrIdempotencyWinnerMissing, "bet-42-1"),
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "Internal server error",
		},
		{
			name:        "未預期的錯誤 → 500，且不外洩細節",
			storeErr:    errors.New("connection refused to 10.0.0.7:3306"),
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "Internal server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(t, &fakeStore{debitErr: tt.storeErr})
			rec := do(t, h, http.MethodPost, "/internal/wallet/debit",
				`{"playerId":42,"amount":100,"idempotencyKey":"bet-42-1"}`)

			if rec.Code != tt.wantStatus {
				t.Errorf("狀態碼 = %d, want %d（body=%s）", rec.Code, tt.wantStatus, rec.Body.String())
			}
			success, message, data := decodeEnvelope(t, rec)
			if success {
				t.Error("錯誤回應的 success 必須是 false")
			}
			if string(data) != "null" {
				t.Errorf("錯誤回應的 data 必須是 null，得到 %s", data)
			}
			if message == nil || *message != tt.wantMessage {
				t.Errorf("message = %v, want %q", message, tt.wantMessage)
			}
		})
	}
}

// TestInternalErrorDoesNotLeakDetails 單獨釘住「500 不可以把內部訊息吐出去」。
//
// 它與上面表格重疊，但守的是不同的東西：表格守的是狀態碼對應，
// 這裡守的是**不外洩**——把 err.Error() 直接回給呼叫端是很自然的「改進」，
// 而那會把連線字串、內部主機名、SQL 片段全部送到外面去。
func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	const secretish = "connection refused to 10.0.0.7:3306"
	h := newTestHandler(t, &fakeStore{debitErr: errors.New(secretish)})

	rec := do(t, h, http.MethodPost, "/internal/wallet/debit",
		`{"playerId":42,"amount":100,"idempotencyKey":"bet-42-1"}`)

	if strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("500 回應外洩了內部細節: %s", rec.Body.String())
	}
}

// ── 請求驗證 ────────────────────────────────────────────────────────────────

// TestValidation 逐條對齊 Java DTO 的 Bean Validation 註解。
//
// ⚠️ 訊息文字是**對外契約**的一部分（Java 的 handleValidation 會把它組成
// `"Invalid request: " + field + " " + defaultMessage`），所以逐字比對。
// 看起來重複的 `subType subType` 是 Java 的真實輸出，不是這裡抄錯——
// 自訂的 @Pattern 訊息本身就以欄位名開頭。
//
// ⚠️ 每一格都只錯**一個**欄位。Java 那邊是 `findFirst()` 挑一個 field error，
// 而多個錯誤時挑到哪一個是不保證的——用多錯欄位的測資去釘訊息，
// 會得到一個在別的機器上會紅的測試。
func TestValidation(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		body        string
		wantMessage string
	}{
		{
			name:        "playerId 未帶",
			path:        "/internal/wallet/debit",
			body:        `{"amount":100,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: playerId must not be null",
		},
		{
			name:        "playerId 是 null",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":null,"amount":100,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: playerId must not be null",
		},
		{
			// ⚠️ 刻意與 Java 分歧：Java 的 playerId 只有 @NotNull 沒有 @Positive，
			// 所以 0 會一路走到 404。見 endpoint.validationError 的註解。
			name:        "playerId 非正整數（刻意的分歧：Java 是 404）",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":0,"amount":100,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: playerId must be positive",
		},
		{
			name:        "amount 未帶",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: amount must not be null",
		},
		{
			// ⭐ 這一格就是「選填欄位必須用指標」的全部理由：amount 未帶與
			// amount 是 0 在 Java 是**兩個不同的錯誤訊息**。
			name:        "amount 是 0",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":0,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: amount must be greater than 0",
		},
		{
			name:        "amount 是負數",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":-1,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: amount must be greater than 0",
		},
		{
			name:        "idempotencyKey 未帶",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100}`,
			wantMessage: "Invalid request: idempotencyKey must not be blank",
		},
		{
			// @NotBlank 是 trim 之後不可為空，而 domain 只擋 ""。
			// 少了 HTTP 層這一條，一把由空白組成的冪等鍵會真的被寫進帳務流水。
			name:        "idempotencyKey 全是空白",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100,"idempotencyKey":"   "}`,
			wantMessage: "Invalid request: idempotencyKey must not be blank",
		},
		{
			name:        "idempotencyKey 超過 100 字元",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100,"idempotencyKey":"` + strings.Repeat("k", 101) + `"}`,
			wantMessage: "Invalid request: idempotencyKey size must be between 0 and 100",
		},
		{
			// 沒有這一條的話，101 字元的 referenceId 會撞 MySQL 1406 → 500，
			// 而 Java 是 400。
			name:        "referenceId 超過 100 字元",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100,"idempotencyKey":"k","referenceId":"` + strings.Repeat("r", 101) + `"}`,
			wantMessage: "Invalid request: referenceId size must be between 0 and 100",
		},
		{
			name:        "debit 帶了不在白名單的 subType",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100,"subType":"NOPE","idempotencyKey":"k"}`,
			wantMessage: "Invalid request: subType subType must be one of BET/SHOP_PURCHASE",
		},
		{
			// ⭐ 方向配對只存在於應用層，DB 的兩個 CHECK 各查各的白名單，
			// 擋不住 type=DEBIT + sub_type=WIN 這種組合。
			name:        "debit 帶了 CREDIT 類的 subType",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100,"subType":"WIN","idempotencyKey":"k"}`,
			wantMessage: "Invalid request: subType subType must be one of BET/SHOP_PURCHASE",
		},
		{
			// ⚠️ 「沒帶」預設 BET，「帶了空字串」是 @Pattern 不符 → 400。
			// 用 string 而不是 *string 的話，這一格會變成 200。
			name:        "debit 帶了空字串 subType（不等於沒帶）",
			path:        "/internal/wallet/debit",
			body:        `{"playerId":42,"amount":100,"subType":"","idempotencyKey":"k"}`,
			wantMessage: "Invalid request: subType subType must be one of BET/SHOP_PURCHASE",
		},
		{
			name:        "credit 未帶 subType（credit 沒有預設值）",
			path:        "/internal/wallet/credit",
			body:        `{"playerId":42,"amount":100,"idempotencyKey":"k"}`,
			wantMessage: "Invalid request: subType must not be blank",
		},
		{
			name: "credit 帶了 DEBIT 類的 subType",
			path: "/internal/wallet/credit",
			body: `{"playerId":42,"amount":100,"subType":"BET","idempotencyKey":"k"}`,
			wantMessage: "Invalid request: subType subType must be one of " +
				"WIN/CHECKIN/TASK/GIFT/GM_REWARD/BANKRUPTCY_AID/DIAMOND_EXCHANGE/TOPUP/CASHBACK/REFUND/MONTHLY_REWARD",
		},
		{
			name:        "credit 的 unfreezeAmount 是負數",
			path:        "/internal/wallet/credit",
			body:        `{"playerId":42,"amount":100,"subType":"WIN","idempotencyKey":"k","unfreezeAmount":-1}`,
			wantMessage: "Invalid request: unfreezeAmount must be greater than or equal to 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			h := newTestHandler(t, store)
			rec := do(t, h, http.MethodPost, tt.path, tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("狀態碼 = %d, want 400（body=%s）", rec.Code, rec.Body.String())
			}
			// ⭐ 最重要的一條：驗證失敗**不可以**碰到帳務層。
			// 少了它，一個「先扣款再驗證」的重構會靜靜通過所有其他斷言。
			if store.calls != 0 {
				t.Errorf("驗證失敗時不該呼叫 store，卻呼叫了 %d 次", store.calls)
			}
			_, message, _ := decodeEnvelope(t, rec)
			if message == nil || *message != tt.wantMessage {
				t.Errorf("message = %v\nwant %q", message, tt.wantMessage)
			}
		})
	}
}

// TestMalformedBodyIsRejected 釘住壞 JSON 的處理。
//
// ⚠️ 這是**刻意的分歧**：Java 的 GlobalExceptionHandler 沒有處理
// HttpMessageNotReadableException，而它註冊了 @ExceptionHandler(Exception.class)，
// 於是壞 JSON 會落到 handleGeneral → 500。「呼叫端送了壞 JSON，
// 伺服器說自己壞了」是缺陷不是契約，所以這裡回 400（記在藍圖 §5）。
func TestMalformedBodyIsRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "不是 JSON", body: `{not json`},
		{name: "空 body", body: ``},
		{name: "型別錯誤", body: `{"playerId":"forty-two","amount":100,"idempotencyKey":"k"}`},
		{name: "amount 超過 int64", body: `{"playerId":42,"amount":99999999999999999999,"idempotencyKey":"k"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			h := newTestHandler(t, store)
			rec := do(t, h, http.MethodPost, "/internal/wallet/debit", tt.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("狀態碼 = %d, want 400（body=%s）", rec.Code, rec.Body.String())
			}
			if store.calls != 0 {
				t.Errorf("解不開的 body 不該碰到帳務層，卻呼叫了 %d 次", store.calls)
			}
		})
	}
}

// TestUnknownFieldsAreIgnored 釘住向前相容。
//
// Spring Boot 預設關掉 FAIL_ON_UNKNOWN_PROPERTIES，Go 的 encoding/json 也預設
// 忽略未知欄位。⚠️ **不要**加 DisallowUnknownFields：那會讓「呼叫端先部署了
// 帶新欄位的版本」變成整批 400，而那正是滾動部署的常態。
func TestUnknownFieldsAreIgnored(t *testing.T) {
	h := newTestHandler(t, &fakeStore{})
	rec := do(t, h, http.MethodPost, "/internal/wallet/debit",
		`{"playerId":42,"amount":100,"idempotencyKey":"k","futureField":"whatever"}`)

	if rec.Code != http.StatusOK {
		t.Errorf("狀態碼 = %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}
}

// ── X-Internal-Secret ───────────────────────────────────────────────────────

// TestInternalSecret 重現 Java InternalSecretFilter 的行為。
func TestInternalSecret(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		secret     string
		setSecret  bool
		wantStatus int
	}{
		{
			name: "沒帶 header → 401", method: http.MethodPost, path: "/internal/wallet/debit",
			setSecret: false, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "帶錯 secret → 401", method: http.MethodPost, path: "/internal/wallet/debit",
			secret: "wrong", setSecret: true, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "空字串 secret → 401", method: http.MethodPost, path: "/internal/wallet/debit",
			secret: "", setSecret: true, wantStatus: http.StatusUnauthorized,
		},
		{
			// ⭐ 這一格是「為什麼中介層掛全域而不是掛路由群組」的證據：
			// 掛群組的話，不存在的 /internal/ 路徑會直接落到 404 而不驗證，
			// 於是沒有 secret 的人可以用「401 還是 404」探測哪些內部端點存在。
			name: "不存在的 /internal/ 路徑也要先驗 secret", method: http.MethodPost,
			path: "/internal/wallet/nope", setSecret: false, wantStatus: http.StatusUnauthorized,
		},
		{
			// /healthz 不在 /internal/ 底下，對齊 Java 的 shouldNotFilter。
			name: "healthz 不需要 secret", method: http.MethodGet, path: "/healthz",
			setSecret: false, wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			h := newTestHandler(t, store)

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(
				`{"playerId":42,"amount":100,"idempotencyKey":"k"}`))
			req.Header.Set("Content-Type", "application/json")
			if tt.setSecret {
				req.Header.Set(internalSecretHeader, tt.secret)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("狀態碼 = %d, want %d（body=%s）", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusUnauthorized {
				// 沒過驗證就**絕對不可以**碰到帳務層。
				if store.calls != 0 {
					t.Errorf("未通過驗證卻呼叫了帳務層 %d 次", store.calls)
				}
				// 訊息逐字對齊 Java 的 InternalSecretFilter:39。
				const want = `{"success":false,"data":null,"message":"Unauthorized"}`
				if got := rec.Body.String(); got != want {
					t.Errorf("401 body = %s, want %s", got, want)
				}
			}
		})
	}
}

// TestNewRejectsEmptySecret 釘住「設定漏了就開不起來」。
//
// ⚠️ 空 secret 會讓 subtle.ConstantTimeCompare 對「同樣沒帶 header」的請求
// 回 1 —— 也就是 /internal/** 對全世界敞開，而服務看起來完全正常。
// 這種「漏設定卻跑得起來」正是本專案最想避免的形狀。
func TestNewRejectsEmptySecret(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(&fakeStore{}, logger, ""); err == nil {
		t.Fatal("internalSecret 為空時 New 必須回錯誤")
	}
	if _, err := New(nil, logger, testSecret); err == nil {
		t.Fatal("store 為 nil 時 New 必須回錯誤")
	}
}
