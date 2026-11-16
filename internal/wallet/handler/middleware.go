package handler

import (
	"crypto/subtle"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	// internalSecretHeader 與 internalPathPrefix 對齊 Java 的 InternalSecretFilter。
	internalSecretHeader = "X-Internal-Secret"
	internalPathPrefix   = "/internal/"
)

// requestLogger 是結構化的存取日誌。
//
// ⚠️ 只記路徑不記 body：body 可能被之後的端點加進敏感欄位，
// 而「哪天多一個欄位就開始外洩」是沒有人會發現的那種問題。
func requestLogger(logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		status := c.Response().StatusCode()
		attrs := []any{
			"method", c.Method(),
			"path", c.Path(),
			"status", status,
			"elapsedMs", time.Since(start).Milliseconds(),
		}
		// 5xx 用 Error（代表我們壞了）；4xx 用 Info，因為 409 在 credit 併發下是常態
		// （地雷 #36），記成 Warn 會讓告警一直響，然後大家開始忽略告警。
		if status >= fiber.StatusInternalServerError {
			logger.Error("HTTP 請求失敗", attrs...)
		} else {
			logger.Info("HTTP 請求", attrs...)
		}
		return err
	}
}

// recovery 把 panic 變成 500，確保一個壞請求不會讓進程死掉。
// 自己寫而不用 fiber 內建的，理由同 requestLogger：日誌一律走 slog 的結構化輸出。
func recovery(logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("HTTP handler panic",
					"method", c.Method(),
					"path", c.Path(),
					"panic", r,
					"stack", string(debug.Stack()),
				)
				// 對齊 Java 的 handleGeneral：500 + 固定訊息，不外洩細節。
				err = respondError(c, fiber.StatusInternalServerError, internalErrorMessage)
			}
		}()
		return c.Next()
	}
}

// requireInternalSecret 重現 Java 的 InternalSecretFilter。
//
// ⚠️ 判斷「要不要驗」用路徑前綴而不是路由群組，是刻意的：掛在群組上的話，
// `POST /internal/wallet/nonexistent` 會直接落到 404 而**不經過驗證**，
// 於是沒有 secret 的人可以用「401 還是 404」探測哪些內部端點存在。
// Java 的 filter 跑在路由之前，這裡用全域中介層對齊它。
func requireInternalSecret(logger *slog.Logger, secret string) fiber.Handler {
	expected := []byte(secret)
	return func(c fiber.Ctx) error {
		if !strings.HasPrefix(c.Path(), internalPathPrefix) {
			return c.Next()
		}
		// ⚠️ 定值時間比對，對齊 Java 的 MessageDigest.isEqual。
		// 用 `==` 會在第一個不同的位元組就返回，攻擊方可靠計時逐位元組猜出 secret。
		if subtle.ConstantTimeCompare([]byte(c.Get(internalSecretHeader)), expected) != 1 {
			logger.Warn("拒絕缺少或錯誤 X-Internal-Secret 的請求",
				"method", c.Method(),
				"path", c.Path(),
				"remoteAddr", c.IP(),
			)
			// 訊息逐字對齊 Java，也刻意不說是「沒帶」還是「帶錯」。
			return respondError(c, fiber.StatusUnauthorized, unauthorizedMessage)
		}
		return c.Next()
	}
}
