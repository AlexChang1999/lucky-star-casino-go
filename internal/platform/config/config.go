// Package config 從環境變數載入各服務共用的基礎設施設定。
//
// 設計原則（沿用 lucky-star-notify-go 的做法）：
//   - 缺必填或值不合法一律**拒絕啟動**，不要帶著半殘的設定跑起來
//   - 一次回報**所有**問題（errors.Join），不要修一個才發現下一個
//
// 為什麼不用 viper：本專案只讀環境變數，不需要多來源合併、熱重載、
// 設定檔格式轉換。標準庫的 os.Getenv 加上這裡的驗證就夠了。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// MySQL 是帳務寫入主庫的連線設定（docs/ADR-001）。
type MySQL struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string

	// 連線池參數。這三個是常見的 Go 面試題，所以刻意顯式設定而不是吃預設值：
	//
	//   MaxOpenConns  同時開啟的連線上限。⚠️ 這個值乘上「服務副本數」不可超過
	//                 MySQL 的 max_connections（compose 設 300），否則尖峰時
	//                 拿不到連線，症狀是請求逾時而不是明確的錯誤。
	//   MaxIdleConns  閒置保留數。設太低會讓連線反覆建立/關閉（TCP 三次握手 +
	//                 MySQL 認證，每次數毫秒），設太高則佔著伺服器資源。
	//                 慣例是與 MaxOpenConns 相同，避免高負載後的「連線抖動」。
	//   ConnMaxLifetime 連線最長壽命。⚠️ **必須小於 MySQL 的 wait_timeout**
	//                 （預設 8 小時），否則伺服器已經關掉的連線還留在池裡，
	//                 下次借出去就是 invalid connection——而且是隨機發生的。
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DSN 組出 go-sql-driver/mysql 的連線字串。
//
// parseTime=true 讓 DATETIME/TIMESTAMP 直接掃進 time.Time，否則會拿到 []byte。
// loc=UTC 與 compose 的 --default-time-zone=+00:00 成對：一律用 UTC 存、
// 在應用層轉。時區在兩處不一致時，結算日界會差一天而且不會報錯。
func (m MySQL) DSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=UTC&charset=utf8mb4&collation=utf8mb4_0900_ai_ci",
		m.User, m.Password, m.Host, m.Port, m.Database,
	)
}

// Mongo 是 CQRS 讀端的連線設定（docs/ADR-001）。
//
// ⚠️ 讀端是**衍生資料**：由 Kafka 事件投影而成，壞掉就重放重建。
// 它絕不可以成為任何欄位的唯一真相——帳務真相永遠在 MySQL。
type Mongo struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

// URI 組出 mongo-driver 的連線字串。
//
// authSource=admin：MONGO_INITDB_ROOT_USERNAME 建立的 root 帳號存在 admin 庫，
// 不寫這個參數的話 driver 會拿業務庫當認證來源，錯誤訊息是
// "Authentication failed"——看起來像密碼錯，其實是找錯地方認證。
func (m Mongo) URI() string {
	return fmt.Sprintf("mongodb://%s:%s@%s:%d/%s?authSource=admin",
		m.User, m.Password, m.Host, m.Port, m.Database)
}

// Redis 的連線設定。
//
// ⚠️ Redis 在本專案是**主儲存不是快取**（AGENTS.md 地雷 #16）：
// 排行榜 ZSET、捕魚 session、後台停用標記都只存在這裡。
type Redis struct {
	Host string
	Port int
}

func (r Redis) Addr() string { return fmt.Sprintf("%s:%d", r.Host, r.Port) }

// Infra 是三個資料儲存的完整設定。
type Infra struct {
	MySQL MySQL
	Mongo Mongo
	Redis Redis
}

// Kafka 是事件匯流排的連線設定。
//
// ⚠️ 它**不放進 Infra**，理由與 LoadMySQL 從 LoadInfra 拆出來完全相同：
// `cmd/migrate` 一輩子不會碰 Kafka，讓它因為 KAFKA_BOOTSTRAP_SERVERS 打錯字
// 而拒絕啟動，錯誤訊息就指不到真因了。
type Kafka struct {
	// Brokers 是 bootstrap 位址清單。多個 broker 時逗號分隔——**這是 bootstrap
	// 用的種子清單，不是完整叢集清單**：client 連上任一個之後會自己去要
	// metadata，拿到真正的 broker 位址（那些位址由 advertised.listeners 決定，
	// 見 compose 檔裡的說明）。
	Brokers []string
}

// LoadKafka 載入事件匯流排設定。
//
// 預設 localhost:9095 對齊 deploy/.env.example（團隊 repo 是 9092、
// lucky-star-notify-go 是 9094，三套要能同時跑，地雷 #28）。
func LoadKafka() (Kafka, error) {
	raw := stringEnv("KAFKA_BOOTSTRAP_SERVERS", "localhost:9095")

	var brokers []string
	for _, addr := range strings.Split(raw, ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			brokers = append(brokers, addr)
		}
	}
	if len(brokers) == 0 {
		// 只有 `KAFKA_BOOTSTRAP_SERVERS=","` 這種值會走到這裡。空字串走預設值，
		// 但「設了一個解析出來是空的值」是打錯字，不該被靜默當成沒設。
		return Kafka{}, fmt.Errorf("KAFKA_BOOTSTRAP_SERVERS=%q 解析不出任何位址", raw)
	}
	return Kafka{Brokers: brokers}, nil
}

// LoadMySQL 只載入帳務寫入主庫的設定。
//
// 為什麼要跟 LoadInfra 分開：`cmd/migrate` 只碰 MySQL。走 LoadInfra 的話，
// 忘了設 `MONGO_ROOT_PASSWORD` 會讓 migrate 拒絕啟動，而那個錯誤訊息與它
// 要做的事完全無關——「錯誤訊息指不到真因」正是本專案最想避免的形狀。
func LoadMySQL() (MySQL, error) {
	var errs []error

	port, err := intEnv("MYSQL_PORT", 3308)
	errs = append(errs, err)

	cfg := MySQL{
		Host:     stringEnv("MYSQL_HOST", "localhost"),
		Port:     port,
		User:     os.Getenv("MYSQL_USER"),
		Password: os.Getenv("MYSQL_PASSWORD"),
		Database: stringEnv("MYSQL_DATABASE", "lucky_star_casino"),

		// 保守的起始值。真正的數字要壓測後才知道（藍圖 §3.4），
		// 現在寫死是為了「顯式優於隱式」，不是因為量過。
		MaxOpenConns:    25,
		MaxIdleConns:    25,
		ConnMaxLifetime: 30 * time.Minute,
	}

	// 密碼沒有合理的預設值——給預設等於讓「忘了設」變成一個能跑起來的狀態，
	// 然後在連線時才失敗，而那時候的錯誤訊息指不到這裡。
	if cfg.User == "" {
		errs = append(errs, errors.New("MYSQL_USER 未設定"))
	}
	if cfg.Password == "" {
		errs = append(errs, errors.New("MYSQL_PASSWORD 未設定"))
	}

	return cfg, errors.Join(errs...)
}

// LoadInfra 從環境變數讀取設定並驗證。
//
// 回傳的 error 可能包含多個問題（errors.Join）——一次看完比修一個跑一次快。
func LoadInfra() (Infra, error) {
	var errs []error

	mysqlCfg, err := LoadMySQL()
	errs = append(errs, err)
	mongoPort, err := intEnv("MONGO_PORT", 27018)
	errs = append(errs, err)
	redisPort, err := intEnv("REDIS_PORT", 6380)
	errs = append(errs, err)

	cfg := Infra{
		MySQL: mysqlCfg,
		Mongo: Mongo{
			Host:     stringEnv("MONGO_HOST", "localhost"),
			Port:     mongoPort,
			User:     os.Getenv("MONGO_ROOT_USER"),
			Password: os.Getenv("MONGO_ROOT_PASSWORD"),
			Database: stringEnv("MONGO_DATABASE", "lucky_star_read"),
		},
		Redis: Redis{
			Host: stringEnv("REDIS_HOST", "localhost"),
			Port: redisPort,
		},
	}

	// MySQL 的必填由 LoadMySQL 驗過了，這裡只補 Mongo 的。
	if cfg.Mongo.User == "" {
		errs = append(errs, errors.New("MONGO_ROOT_USER 未設定"))
	}
	if cfg.Mongo.Password == "" {
		errs = append(errs, errors.New("MONGO_ROOT_PASSWORD 未設定"))
	}

	return cfg, errors.Join(errs...)
}

func stringEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// intEnv 解析整數環境變數。空字串走預設值；有值但不是數字則是錯誤——
// 「打錯字」與「沒設定」是兩件事，靜默套用預設會把前者藏起來。
func intEnv(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q 不是合法的整數: %w", key, raw, err)
	}
	return v, nil
}

// durationEnv 解析 Go 時距字串（`200ms`、`1s`、`24h`）。
//
// ⚠️ 刻意**不接受純數字**。Java 那邊是 `wallet.outbox.poll-interval-ms: 200`，
// 單位藏在變數名字裡；照搬過來會得到「`WALLET_OUTBOX_POLL_INTERVAL=200`
// 到底是 200 毫秒還是 200 秒」這種只能靠讀原始碼回答的問題。
// 帶單位的字串讓值自己說清楚，而 `200` 這種寫法會直接被 time.ParseDuration 擋下。
func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s=%q 不是合法的時距（例：200ms / 5s / 24h）: %w", key, raw, err)
	}
	return v, nil
}
