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

	// Outbox 是 Transactional Outbox 投遞器與清理排程的參數（AGENTS.md 地雷 #5）。
	Outbox Outbox

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

// Outbox 是 outbox 投遞器（poller）與保留期清理排程的參數。
//
// 三個值全部對齊 Java 版的 `wallet.outbox.*`（`application.yml`），
// 因為它們是**實測調出來的**，不是隨手填的：poll-interval 從 1000ms 下修到
// 200ms、batch-size 從寫死的 100 改成可調的 500，都出自團隊 T-090 的遠端壓測
// （2026-07-23）。照抄一組已經被壓測驗證過的數字，比自己重新猜一組好。
type Outbox struct {
	// PollInterval 是**上一輪跑完之後**再等多久跑下一輪（fixed delay），
	// 不是固定頻率（fixed rate）。理由見 outbox.Poller.Run。
	PollInterval time.Duration

	// BatchSize 是單輪最多撈幾筆待送事件。
	//
	// ⚠️ 它同時是 Kafka writer 的 BatchSize（見 outbox.NewWriter）：
	// 兩者不一致的話，一輪撈 500 筆卻只有 100 筆能塞進一個 batch，
	// 剩下的要等下一次 flush——延遲從 10ms 級跳到秒級，而**日誌與指標都看不出來**
	// （AGENTS.md 地雷 #21 的變形）。
	BatchSize int

	// Retention 是 SENT 的列保留多久才刪。
	//
	// ⚠️ 只刪 SENT，PENDING 無論多舊都不刪（地雷 #5）。7 天與下游去重標記的
	// TTL 對齊——兩者都對應「最大重送窗口」，保留期短於去重 TTL 會出現
	// 「事件已刪、去重標記還在」的無法對照狀態。
	Retention time.Duration
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
	outbox, err := loadOutbox()
	errs = append(errs, err)

	cfg := Wallet{
		Outbox: outbox,
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

// loadOutbox 讀 outbox 投遞器的三個參數，並擋掉三個「值合法但語義有害」的設定。
//
// ⚠️ 這三個檢查不是防禦性程式碼潔癖，它們各自對應一個**不會報錯**的故障：
//   - PollInterval <= 0 → 迴圈變成忙碌輪詢，把 CPU 與 DB 連線吃光
//   - BatchSize <= 0 → `LIMIT 0` 永遠撈不到東西，outbox 只進不出而服務一切正常
//   - Retention <= 0 → 清理排程會刪掉**剛剛才送出去**的列，事故時完全無跡可循
func loadOutbox() (Outbox, error) {
	var errs []error

	interval, err := durationEnv("WALLET_OUTBOX_POLL_INTERVAL", 200*time.Millisecond)
	errs = append(errs, err)
	batchSize, err := intEnv("WALLET_OUTBOX_BATCH_SIZE", 500)
	errs = append(errs, err)
	// 用「天」而不是時距字串，因為保留期本來就是以天在談的
	// （Java 是 `wallet.outbox.retention-days`），寫 `168h` 只會讓人多算一次。
	retentionDays, err := intEnv("WALLET_OUTBOX_RETENTION_DAYS", 7)
	errs = append(errs, err)

	if interval <= 0 {
		errs = append(errs, fmt.Errorf("WALLET_OUTBOX_POLL_INTERVAL=%s 必須大於 0", interval))
	}
	if batchSize <= 0 {
		errs = append(errs, fmt.Errorf("WALLET_OUTBOX_BATCH_SIZE=%d 必須大於 0——0 會讓 LIMIT 0 永遠撈不到事件", batchSize))
	}
	if retentionDays <= 0 {
		errs = append(errs, fmt.Errorf("WALLET_OUTBOX_RETENTION_DAYS=%d 必須大於 0——0 會刪掉剛送出的事件", retentionDays))
	}

	return Outbox{
		PollInterval: interval,
		BatchSize:    batchSize,
		Retention:    time.Duration(retentionDays) * 24 * time.Hour,
	}, errors.Join(errs...)
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
