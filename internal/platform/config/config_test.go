package config

import (
	"strings"
	"testing"
	"time"
)

func TestMySQLDSN(t *testing.T) {
	cfg := MySQL{
		Host: "localhost", Port: 3308,
		User: "lucky_user", Password: "pw",
		Database: "lucky_star_casino",
	}
	got := cfg.DSN()

	// 逐項斷言而不是比對整串：整串比對在加參數時會壞掉，
	// 但真正要釘住的是「這幾個參數一定要在」。
	tests := []struct {
		name string
		want string
		why  string
	}{
		{"帳密與位址", "lucky_user:pw@tcp(localhost:3308)/lucky_star_casino", "基本連線資訊"},
		{"parseTime", "parseTime=true", "沒有它 DATETIME 會掃成 []byte 而不是 time.Time"},
		{"UTC", "loc=UTC", "與 compose 的 --default-time-zone=+00:00 成對，不一致時結算日界會差一天且不報錯"},
		{"utf8mb4", "charset=utf8mb4", "utf8 在 MySQL 是三位元組，存不下 emoji 暱稱"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(got, tt.want) {
				t.Errorf("DSN 缺少 %q（%s）\nDSN = %s", tt.want, tt.why, got)
			}
		})
	}
}

func TestMongoURIAuthSource(t *testing.T) {
	cfg := Mongo{Host: "localhost", Port: 27018, User: "admin", Password: "pw", Database: "read"}
	got := cfg.URI()

	// authSource=admin 是最容易漏的一項：MONGO_INITDB_ROOT_USERNAME 建的帳號
	// 存在 admin 庫，漏掉會拿業務庫去認證，錯誤訊息是 "Authentication failed"
	// ——看起來像密碼錯，其實是找錯地方認證。
	if !strings.Contains(got, "authSource=admin") {
		t.Errorf("URI 缺少 authSource=admin，會在業務庫認證而失敗\nURI = %s", got)
	}
}

func TestIntEnv(t *testing.T) {
	tests := []struct {
		name     string
		set      bool
		value    string
		fallback int
		want     int
		wantErr  bool
	}{
		{name: "未設定時走預設", set: false, fallback: 3308, want: 3308},
		{name: "空字串視同未設定", set: true, value: "", fallback: 3308, want: 3308},
		{name: "正常數值", set: true, value: "3306", fallback: 3308, want: 3306},
		// 打錯字必須是錯誤而不是靜默套用預設——否則「我明明設了 PORT
		// 為什麼連到別的埠」會查很久。
		{name: "非數字要報錯不可靜默走預設", set: true, value: "3308a", fallback: 3308, wantErr: true},
		{name: "負數目前不擋（連線時才會失敗）", set: true, value: "-1", fallback: 3308, want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "TEST_INT_ENV"
			if tt.set {
				t.Setenv(key, tt.value)
			}
			got, err := intEnv(key, tt.fallback)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("預期要有錯誤，卻得到 %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDurationEnv(t *testing.T) {
	tests := []struct {
		name     string
		set      bool
		value    string
		fallback time.Duration
		want     time.Duration
		wantErr  bool
	}{
		{name: "未設定時走預設", set: false, fallback: 200 * time.Millisecond, want: 200 * time.Millisecond},
		{name: "空字串視同未設定", set: true, value: "", fallback: 200 * time.Millisecond, want: 200 * time.Millisecond},
		{name: "帶單位的毫秒", set: true, value: "50ms", fallback: time.Second, want: 50 * time.Millisecond},
		{name: "前後空白要吃掉", set: true, value: "  1s  ", fallback: time.Second, want: time.Second},
		// ⭐ 這一條是這個 helper 存在的理由：Java 那邊是 poll-interval-ms: 200，
		// 單位藏在名字裡。純數字若被當成「200 奈秒」（time.Duration 的底層單位）
		// 就會變成忙碌輪詢，而且沒有任何錯誤訊息。
		{name: "純數字必須報錯", set: true, value: "200", fallback: time.Second, wantErr: true},
		{name: "亂寫必須報錯", set: true, value: "soon", fallback: time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "TEST_DURATION_ENV"
			if tt.set {
				t.Setenv(key, tt.value)
			}
			got, err := durationEnv(key, tt.fallback)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("預期要有錯誤，卻得到 %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLoadKafka(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr bool
	}{
		{name: "未設定走預設", value: "", want: []string{"localhost:9095"}},
		{name: "單一位址", value: "kafka:9092", want: []string{"kafka:9092"}},
		// bootstrap 清單允許多個位址，空白與尾逗號是手寫設定檔的常態，要吃掉。
		{name: "多位址與空白", value: " a:1 , b:2 ,", want: []string{"a:1", "b:2"}},
		// 「設了但解析不出東西」是打錯字，不該被靜默當成沒設定。
		{name: "只有逗號要報錯", value: ",,", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KAFKA_BOOTSTRAP_SERVERS", tt.value)

			got, err := LoadKafka()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("預期要有錯誤，卻得到 %v", got.Brokers)
				}
				return
			}
			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if strings.Join(got.Brokers, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %v, want %v", got.Brokers, tt.want)
			}
		})
	}
}

// TestLoadOutboxRejectsHarmlessLookingValues 釘住三個「合法但有害」的設定值。
//
// ⭐ 三個都是**不會報錯**的故障，所以只能靠設定驗證擋下來：
// 0 的輪詢間隔＝忙碌輪詢吃光 CPU 與連線、0 的批次＝LIMIT 0 永遠撈不到東西
// （outbox 只進不出而服務一切正常）、0 的保留期＝清理排程刪掉剛送出的列。
func TestLoadOutboxRejectsHarmlessLookingValues(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{name: "預設值", env: map[string]string{}},
		{
			name:    "輪詢間隔為 0",
			env:     map[string]string{"WALLET_OUTBOX_POLL_INTERVAL": "0s"},
			wantErr: true,
		},
		{
			name:    "批次大小為 0",
			env:     map[string]string{"WALLET_OUTBOX_BATCH_SIZE": "0"},
			wantErr: true,
		},
		{
			name:    "保留天數為 0",
			env:     map[string]string{"WALLET_OUTBOX_RETENTION_DAYS": "0"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// ⚠️ 三個變數一律先清空再套用案例值。少了這一步，開發者
			// `set -a && . deploy/.env` 之後跑測試會拿到自己機器上的值，
			// 於是「預設值」這個案例在 CI 綠、在本機紅——最難查的那種失敗。
			for _, key := range []string{
				"WALLET_OUTBOX_POLL_INTERVAL",
				"WALLET_OUTBOX_BATCH_SIZE",
				"WALLET_OUTBOX_RETENTION_DAYS",
			} {
				t.Setenv(key, tt.env[key])
			}
			got, err := loadOutbox()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("預期要拒絕，卻通過了: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			// 預設值與 deploy/.env.example、與 Java 的 wallet.outbox.* 三處成對。
			if got.PollInterval != 200*time.Millisecond {
				t.Errorf("PollInterval = %s, want 200ms", got.PollInterval)
			}
			if got.BatchSize != 500 {
				t.Errorf("BatchSize = %d, want 500", got.BatchSize)
			}
			if got.Retention != 7*24*time.Hour {
				t.Errorf("Retention = %s, want 168h（7 天，與下游去重標記 TTL 對齊）", got.Retention)
			}
		})
	}
}

func TestLoadInfraReportsAllMissingSecretsAtOnce(t *testing.T) {
	// 刻意四個必填全部留空。重點不是「會失敗」，而是**一次回報全部**——
	// 一次修一個、跑一次、再發現下一個，是設定驗證最浪費時間的形狀。
	t.Setenv("MYSQL_USER", "")
	t.Setenv("MYSQL_PASSWORD", "")
	t.Setenv("MONGO_ROOT_USER", "")
	t.Setenv("MONGO_ROOT_PASSWORD", "")

	_, err := LoadInfra()
	if err == nil {
		t.Fatal("必填為空時應該要拒絕，卻通過了")
	}
	for _, want := range []string{"MYSQL_USER", "MYSQL_PASSWORD", "MONGO_ROOT_USER", "MONGO_ROOT_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("錯誤訊息沒有提到 %s，代表沒有一次回報所有問題\n實際: %v", want, err)
		}
	}
}

func TestLoadInfraDefaults(t *testing.T) {
	t.Setenv("MYSQL_USER", "u")
	t.Setenv("MYSQL_PASSWORD", "p")
	t.Setenv("MONGO_ROOT_USER", "u")
	t.Setenv("MONGO_ROOT_PASSWORD", "p")

	cfg, err := LoadInfra()
	if err != nil {
		t.Fatalf("非預期錯誤: %v", err)
	}

	// 這些預設值與 deploy/.env.example 是成對的。改一邊沒改另一邊時，
	// 「本機跑得起來、容器裡跑不起來」會很難查。
	tests := []struct {
		name      string
		got, want any
	}{
		{"MySQL 埠", cfg.MySQL.Port, 3308},
		{"Mongo 埠", cfg.Mongo.Port, 27018},
		{"Redis 埠", cfg.Redis.Port, 6380},
		{"連線壽命短於 MySQL wait_timeout(8h)", cfg.MySQL.ConnMaxLifetime < 8*time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}
