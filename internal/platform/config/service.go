package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// HTTPServer 是業務服務對外的 HTTP 伺服器設定。
//
// ⚠️ 四個逾時全部**顯式設定**，因為 `net/http.Server` 的零值代表「永不逾時」。
// 這是 Go 相對 Spring Boot 最容易吃虧的地方：Tomcat 有一整組預設值
// （`connectionTimeout` 20s 等），Go 的預設是**沒有**。不設的後果是一條半開的
// 連線可以永久佔著一個 goroutine 與一個 fd——沒有錯誤訊息，只有連線數慢慢爬。
type HTTPServer struct {
	Port int

	// ReadHeaderTimeout 是 Slowloris 的解藥：攻擊方一秒送一個 header 位元組，
	// 沒有這個逾時就能用極少的資源把 fd 耗光。
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// ShutdownTimeout 是收工時等待既有請求跑完的上限。
	// ⚠️ 必須小於編排器送出 SIGTERM 之後到 SIGKILL 的寬限期
	// （docker stop 預設 10s、K8s 預設 30s），否則優雅關機還沒做完就被砍。
	ShutdownTimeout time.Duration
}

// Addr 是 http.Server.Addr 的格式。
//
// 只給埠、不綁介面：容器裡要監聽 0.0.0.0 才收得到宿主轉進來的流量，
// 寫死 127.0.0.1 的話本機測得到、進了容器就完全不通。
func (h HTTPServer) Addr() string { return fmt.Sprintf(":%d", h.Port) }

// Wallet 是 wallet 服務自己的設定（基礎設施走 LoadMySQL / LoadInfra）。
//
// 為什麼每個服務一個 struct，而不是一個共用的大 Config：服務要什麼就宣告什麼，
// 這樣「忘了設某個變數」的錯誤訊息才會指向真正需要它的服務
// （沿用 LoadMySQL 與 LoadInfra 分開的同一個判準）。
type Wallet struct {
	HTTP HTTPServer

	// InternalSecret 對應 Java 的 `internal.secret`（`InternalSecretFilter`）。
	// ⚠️ **沒有預設值**，且空字串一律拒絕啟動——空 secret 會讓
	// `/internal/**` 對全世界敞開，而服務看起來完全正常。
	// Java 那邊寫的是 `${INTERNAL_SECRET:?...}`，語義相同。
	InternalSecret string

	// SQLLog 決定要不要把 GORM 產生的 SQL 印進結構化 log。
	//
	// ⚠️ 預設 **true**，這是 wallet 刻意與其他服務不同的地方（藍圖 §3.2）：
	// 帳務正確性建立在「冪等鍵 UNIQUE 衝突」與「樂觀鎖比對後的受影響列數」
	// 兩件事上，而那正是 ORM 最容易騙人的地方。看得見 SQL 是硬性要求，
	// 不是除錯選項。壓測時可以用 WALLET_SQL_LOG=false 關掉。
	SQLLog bool

	LogLevel slog.Level
}

// walletDefaultPort 是本專案 wallet 的對外埠。
//
// ⚠️ 8182 而不是 Java 版的 8082（地雷 #28）：三套環境要能同時跑才做得成
// A/B 對照。本專案的業務服務一律是「Java 版的埠 + 100」，
// 所以 gateway 8180 / member 8181 / wallet 8182 / game 8183 …，
// 一眼就看得出對應關係。⚠️ 8088 已經被 lucky-star-notify-go 用掉了。
const walletDefaultPort = 8182

// LoadWallet 載入 wallet 服務自己的設定。
//
// 與 LoadMySQL / LoadInfra 一樣：缺必填一律拒絕啟動，且**一次回報所有問題**。
func LoadWallet() (Wallet, error) {
	var errs []error

	port, err := intEnv("WALLET_HTTP_PORT", walletDefaultPort)
	errs = append(errs, err)
	sqlLog, err := boolEnv("WALLET_SQL_LOG", true)
	errs = append(errs, err)
	level, err := LoadLogLevel()
	errs = append(errs, err)

	cfg := Wallet{
		HTTP: HTTPServer{
			Port: port,
			// 這五個目前寫死。真正的數字要壓測後才知道（藍圖 §3.4），
			// 現在寫死是為了「顯式優於隱式」，不是因為量過。
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
			// 5s < docker stop 的 10s 寬限期。
			ShutdownTimeout: 5 * time.Second,
		},
		InternalSecret: os.Getenv("INTERNAL_SECRET"),
		SQLLog:         sqlLog,
		LogLevel:       level,
	}

	if cfg.InternalSecret == "" {
		errs = append(errs, errors.New(
			"INTERNAL_SECRET 未設定——它保護 /internal/**，空值等於沒有保護"))
	}

	return cfg, errors.Join(errs...)
}

// LoadLogLevel 解析 LOG_LEVEL（debug / info / warn / error），預設 info。
//
// 與 intEnv 同一個判準：**打錯字是錯誤，不是靜默走預設**。
// `LOG_LEVEL=verbose` 若被當成 info，你會以為自己開了詳細日誌。
func LoadLogLevel() (slog.Level, error) {
	raw := os.Getenv("LOG_LEVEL")
	if raw == "" {
		return slog.LevelInfo, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL=%q 不是合法的等級（debug / info / warn / error）", raw)
	}
}

// boolEnv 解析布林環境變數。空字串走預設值；有值但不是布林則是錯誤。
//
// ⚠️ 只接受 true/false/1/0（大小寫不拘）。刻意**不接受** yes/on——
// 那會讓「支援哪些寫法」變成一份要背的清單，而 `WALLET_SQL_LOG=on`
// 被當成 false 是完全無聲的。
func boolEnv(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q 不是合法的布林值（true / false / 1 / 0）", key, raw)
	}
}
