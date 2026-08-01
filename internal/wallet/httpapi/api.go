// Package httpapi 是 wallet 的 HTTP 邊界。
//
// 這一層**很薄，而且必須保持很薄**：帳務語義全部在 internal/wallet/store，
// 驗證規則全部在 internal/wallet/domain。這裡只做三件事——
// 解 JSON、把錯誤翻譯成狀態碼、把結果包成 Java 版的回應信封。
//
// ⭐ 為什麼「翻譯成狀態碼」值得一個獨立的層：Go 的 error 是**分類**
// （sentinel），HTTP 的狀態碼是**對外契約**，兩者一對一但不是同一件事。
// 內部錯誤訊息是中文、帶 playerID 與冪等鍵，方便查問題；對外訊息必須逐字
// 等於 Java 版，因為契約測試會 diff 它。把兩者混在一起的話，改一句中文
// 除錯訊息就會弄壞契約測試——而那個因果關係完全看不出來。
//
// ⚠️ 端點路徑、回應信封、狀態碼、訊息文字全部照抄 Java 版，來源是
// 逐檔讀過的原始碼（`InternalWalletController`、`GlobalExceptionHandler`、
// `ApiResponse`、`InternalSecretFilter`）。**不要照直覺改**，
// 差一個狀態碼就是契約測試紅一格。
package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// Store 是這一層需要的帳務能力。
//
// ⭐ 它定義在**消費端**（CLAUDE.md §2），不是定義在 store 套件裡再回頭實作。
// 這是 Go 與 Java 差最多的地方之一：Java 的慣例是先寫 `WalletRepository` 介面、
// 再寫 `WalletRepositoryImpl`；Go 的慣例是**使用者宣告自己需要什麼**，
// 提供者根本不知道這個介面存在（`*walletstore.Repository` 沒有 import 這裡）。
//
// 好處是具體的：這個介面只有兩個方法，所以測試的假物件也只要兩個方法。
// 若介面由 store 那邊定義，它會長成「Repository 的全部方法」，
// 而每次 store 多一個方法，這裡的假物件就得跟著長——那是 Java 那套的痛點。
type Store interface {
	Debit(ctx context.Context, m domain.Movement) (walletstore.DebitResult, error)
	Credit(ctx context.Context, m domain.Movement) (walletstore.CreditResult, error)
}

const (
	// internalSecretHeader 與 internalPathPrefix 對齊 Java 的 InternalSecretFilter。
	internalSecretHeader = "X-Internal-Secret"
	internalPathPrefix   = "/internal/"
)

// API 持有這一層需要的兩個依賴。沒有第三個——有第三個就代表有邏輯跑錯層了。
type API struct {
	store  Store
	logger *slog.Logger
}

// New 組出 wallet 的 HTTP handler。
//
// ⚠️ internalSecret 為空字串一律回錯誤。空 secret 會讓 subtle.ConstantTimeCompare
// 對「同樣沒帶 header」的請求回 1，於是 `/internal/**` 對全世界敞開——
// 而服務看起來、log 看起來、健康檢查看起來全部正常。
// 這種「設定漏了卻跑得起來」正是本專案最想避免的形狀。
func New(store Store, logger *slog.Logger, internalSecret string) (http.Handler, error) {
	if store == nil {
		return nil, errors.New("httpapi.New: store 不可為 nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if internalSecret == "" {
		return nil, errors.New("httpapi.New: internalSecret 不可為空——空值等於 /internal/** 沒有保護")
	}

	// ⚠️ 這是全域設定，所以只在這裡設一次。不設的話 gin 會在每次啟動時往
	// stderr 印一段「running in debug mode」的警告，而 debug 模式還會多做
	// 一次 route dump 與更囉唆的錯誤輸出——在 JSON log 的環境裡那是雜訊。
	gin.SetMode(gin.ReleaseMode)

	api := &API{store: store, logger: logger}

	// ⚠️ 用 gin.New() 而不是 gin.Default()：後者塞的是 gin 自己的 Logger 與
	// Recovery，兩個都往 stdout 印**純文字**。本專案的 log 一律走 slog 的
	// 結構化輸出（藍圖 §3.6），混兩種格式會讓日誌收集器只解得開一半。
	r := gin.New()
	r.Use(api.requestLogger(), api.recovery(), api.requireInternalSecret(internalSecret))

	// ⚠️ 路由順序：具體路徑必須排在 catch-all 之前（AGENTS.md 地雷 #13）。
	// 目前沒有 catch-all，但這個順序要求在 Go 的 router 一樣成立，
	// 之後加代理路由時症狀是「某條 API 永遠打到別的地方」。
	r.GET("/healthz", api.health)
	r.POST("/internal/wallet/debit", api.debit)
	r.POST("/internal/wallet/credit", api.credit)

	return r, nil
}

// health 是給編排器用的存活探針。
//
// ⚠️ **不是 Java 契約的一部分**：Java 版走 Spring Actuator 的 `/actuator/health`
// （回 `{"status":"UP",...}`）。這裡刻意不模仿那個形狀——為了一個探針把
// Actuator 的 JSON 結構搬過來，是把 Spring 的實作細節當成契約。
// 契約測試若需要等待就緒，那是測試框架的事，兩邊各自設定探針路徑即可。
//
// ⚠️ 它刻意**不查 DB**：存活探針要回答的是「這個進程還活著嗎」。
// 探針裡查 DB 會讓「DB 抖一下」變成「編排器把服務殺掉重啟」，
// 而重啟解決不了 DB 的問題，只會讓可用的副本更少。
func (a *API) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// requireInternalSecret 重現 Java 的 InternalSecretFilter。
//
// ⚠️ 判斷「要不要驗」用的是路徑前綴而不是 gin 的路由群組，這是刻意的：
// 掛在群組上的話，`POST /internal/wallet/nonexistent` 會直接落到 404 而
// **不經過驗證**——於是沒有 secret 的人可以用「401 還是 404」來探測
// 哪些內部端點存在。Java 的 filter 跑在路由之前，這裡用全域中介層對齊它。
func (a *API) requireInternalSecret(secret string) gin.HandlerFunc {
	expected := []byte(secret)
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, internalPathPrefix) {
			c.Next()
			return
		}
		// ⚠️ 定值時間比對，對齊 Java 的 MessageDigest.isEqual。
		// 用 `provided == secret` 會在第一個不同的位元組就返回，
		// 於是攻擊方可以靠反覆計時一個位元組一個位元組地把 secret 猜出來。
		if subtle.ConstantTimeCompare([]byte(c.GetHeader(internalSecretHeader)), expected) != 1 {
			a.logger.Warn("拒絕缺少或錯誤 X-Internal-Secret 的請求",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"remoteAddr", c.ClientIP(),
			)
			// ⚠️ 訊息逐字對齊 Java：`{"success":false,"data":null,"message":"Unauthorized"}`。
			// 也刻意**不說**是「沒帶」還是「帶錯」——那個區分只對攻擊方有用。
			c.AbortWithStatusJSON(http.StatusUnauthorized, errorEnvelope("Unauthorized"))
			return
		}
		c.Next()
	}
}

// requestLogger 是結構化的存取日誌。
//
// ⚠️ 只記路徑不記 body。body 裡有冪等鍵（可以記）但也可能被之後的端點加進
// 敏感欄位，而「哪天多一個欄位就開始外洩」是沒有人會發現的那種問題。
// 要查特定一筆帳務時，冪等鍵已經在 store 層的 SQL log 裡了。
func (a *API) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"elapsedMs", time.Since(start).Milliseconds(),
		}
		// 5xx 用 Error：它代表**我們**壞了。4xx 用 Info，因為那是呼叫端的問題，
		// 而 409（樂觀鎖衝突）在 credit 的併發下是常態（地雷 #36）——
		// 把它記成 Warn 會讓告警一直響，然後大家就開始忽略告警。
		if status >= http.StatusInternalServerError {
			a.logger.Error("HTTP 請求失敗", attrs...)
			return
		}
		a.logger.Info("HTTP 請求", attrs...)
	}
}

// recovery 把 panic 變成 500，並確保進程不會因為一個壞請求而死掉。
//
// ⚠️ 寫進 io.Discard 是因為 gin 內建的 recovery 會把堆疊往 stderr 印純文字。
// 這裡改成用 slog 記，理由同 requestLogger。
func (a *API) recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		a.logger.Error("HTTP handler panic",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"panic", recovered,
			"stack", string(debug.Stack()),
		)
		// 對齊 Java 的 handleGeneral：500 + "Internal server error"，不外洩細節。
		c.AbortWithStatusJSON(http.StatusInternalServerError, errorEnvelope(internalErrorMessage))
	})
}

// ── 回應信封 ────────────────────────────────────────────────────────────────

// envelope 對齊 Java 的 `com.luckystar.wallet.common.ApiResponse`。
//
// ⚠️ 三個欄位都**不可以加 omitempty**（AGENTS.md 地雷 #33）。
// Java 版沒有設定 `NON_NULL`，所以成功時會輸出 `"message": null`、
// 失敗時會輸出 `"data": null`。加了 omitempty 會讓欄位整個消失——
// 契約測試 diff 到的是「少一個 key」，而不是值不同，
// 而下游若用 `"message" in resp` 判斷，行為會靜靜地改變。
type envelope struct {
	Success bool    `json:"success"`
	Data    any     `json:"data"`
	Message *string `json:"message"`
}

func okEnvelope(data any) envelope {
	return envelope{Success: true, Data: data, Message: nil}
}

func errorEnvelope(message string) envelope {
	return envelope{Success: false, Data: nil, Message: &message}
}

func (a *API) respondOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, okEnvelope(data))
}

func (a *API) respondError(c *gin.Context, status int, message string) {
	c.JSON(status, errorEnvelope(message))
}
