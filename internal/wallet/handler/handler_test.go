package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

const testSecret = "test-internal-secret"

func testConfig() config.HTTPServer {
	return config.HTTPServer{ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestApp(t *testing.T, h *Handler) *fiber.App {
	t.Helper()
	app, err := NewApp(h, testSecret, testConfig())
	if err != nil {
		t.Fatalf("組裝 app 失敗: %v", err)
	}
	return app
}

// send 送出請求並回傳 (狀態碼, body)。setSecret=false 代表完全不帶 header。
func send(t *testing.T, app *fiber.App, method, path, body string, secret *string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != nil {
		req.Header.Set(internalSecretHeader, *secret)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("讀回應失敗: %v", err)
	}
	return resp.StatusCode, string(b)
}

// TestInternalSecret 重現 Java InternalSecretFilter 的行為。
func TestInternalSecret(t *testing.T) {
	good, wrong, empty := testSecret, "wrong", ""
	tests := []struct {
		name       string
		method     string
		path       string
		secret     *string
		wantStatus int
	}{
		{"沒帶 header → 401", http.MethodPost, "/internal/wallet/debit", nil, http.StatusUnauthorized},
		{"帶錯 secret → 401", http.MethodPost, "/internal/wallet/debit", &wrong, http.StatusUnauthorized},
		{"空字串 secret → 401", http.MethodPost, "/internal/wallet/debit", &empty, http.StatusUnauthorized},
		// ⭐ 「為什麼中介層掛全域而不是路由群組」的證據：不存在的 /internal/ 路徑也要先驗，
		// 否則沒有 secret 的人可以用「401 還是 404」探測哪些內部端點存在。
		{"不存在的 /internal/ 路徑也要先驗 secret", http.MethodPost, "/internal/wallet/nope", nil, http.StatusUnauthorized},
		{"帶對 secret 但路徑不存在 → 404", http.MethodPost, "/internal/wallet/nope", &good, http.StatusNotFound},
		// /healthz 不在 /internal/ 底下，對齊 Java 的 shouldNotFilter。
		{"healthz 不需要 secret", http.MethodGet, "/healthz", nil, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t, New(discardLogger()))
			status, body := send(t, app, tt.method, tt.path, `{}`, tt.secret)
			if status != tt.wantStatus {
				t.Fatalf("狀態碼 = %d, want %d（body=%s）", status, tt.wantStatus, body)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				// 訊息逐字對齊 Java 的 InternalSecretFilter:39。
				const want = `{"success":false,"data":null,"message":"Unauthorized"}`
				if body != want {
					t.Errorf("401 body = %s, want %s", body, want)
				}
			}
		})
	}
}

// TestNewAppRejectsEmptySecret 釘住「設定漏了就開不起來」。
func TestNewAppRejectsEmptySecret(t *testing.T) {
	if _, err := NewApp(New(discardLogger()), "", testConfig()); err == nil {
		t.Fatal("internalSecret 為空時 NewApp 必須回錯誤")
	}
	if _, err := NewApp(nil, testSecret, testConfig()); err == nil {
		t.Fatal("handler 為 nil 時 NewApp 必須回錯誤")
	}
}

func TestHealthz(t *testing.T) {
	app := newTestApp(t, New(discardLogger()))
	status, body := send(t, app, http.MethodGet, "/healthz", "", nil)
	if status != http.StatusOK || body != `{"status":"ok"}` {
		t.Errorf("healthz = %d %s, want 200 {\"status\":\"ok\"}", status, body)
	}
}
