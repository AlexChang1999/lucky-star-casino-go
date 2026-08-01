//go:build infra

// 這個檔需要真的跑起來的基礎設施，所以放在 infra build tag 後面：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	go test -race -tags=infra ./internal/platform/store/
//
// 為什麼用 build tag 而不是「偵測不到就 t.Skip」：
// skip 會讓「基礎設施沒起來」與「測試通過」在輸出上長得幾乎一樣，
// 於是 CI 綠燈其實什麼都沒驗到。build tag 是**明確的選擇**，不會誤判。
package store

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

// loadOrSkip 讀設定；缺必填就直接失敗而不是 skip——都指定 -tags=infra 了，
// 代表使用者**預期**這些測試要跑。
func loadOrSkip(t *testing.T) config.Infra {
	t.Helper()
	cfg, err := config.LoadInfra()
	if err != nil {
		t.Fatalf("載入設定失敗（是不是忘了 set -a; source deploy/.env）: %v", err)
	}
	return cfg
}

func TestOpenMySQL(t *testing.T) {
	cfg := loadOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := OpenMySQL(ctx, cfg.MySQL)
	if err != nil {
		t.Fatalf("連 MySQL 失敗: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("取得底層 *sql.DB 失敗: %v", err)
	}
	defer sqlDB.Close()

	t.Run("連線池參數有真的套用", func(t *testing.T) {
		// 這裡驗的是「我們設的值有生效」，不是「值選得對」——
		// 後者要壓測才知道（藍圖 §3.4）。
		if got := sqlDB.Stats().MaxOpenConnections; got != cfg.MySQL.MaxOpenConns {
			t.Errorf("MaxOpenConnections = %d, want %d", got, cfg.MySQL.MaxOpenConns)
		}
	})

	t.Run("交易可用（帳務的前提）", func(t *testing.T) {
		var one int
		if err := db.Raw("SELECT 1").Scan(&one).Error; err != nil {
			t.Fatalf("查詢失敗: %v", err)
		}
		if one != 1 {
			t.Errorf("SELECT 1 得到 %d", one)
		}
	})

	t.Run("SKIP LOCKED 可用", func(t *testing.T) {
		// ⚠️ 這一項是刻意存在的：團隊 ADR-001 否決「單一 MySQL」的理由是
		// 「FOR UPDATE SKIP LOCKED 支援較弱」，而 docs/ADR-001 主張那條
		// 在 MySQL 8.0 起就不成立。與其在文件裡宣稱，不如讓測試證明它。
		if err := db.Exec(`CREATE TABLE IF NOT EXISTS _skip_locked_probe (
			id BIGINT PRIMARY KEY
		) ENGINE=InnoDB`).Error; err != nil {
			t.Fatalf("建立探測表失敗: %v", err)
		}
		defer db.Exec("DROP TABLE IF EXISTS _skip_locked_probe")

		var ids []int64
		if err := db.Raw("SELECT id FROM _skip_locked_probe FOR UPDATE SKIP LOCKED").Scan(&ids).Error; err != nil {
			t.Fatalf("SKIP LOCKED 不被支援，ADR-001 的主張需要修正: %v", err)
		}
	})
}

func TestOpenMongo(t *testing.T) {
	cfg := loadOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, db, err := OpenMongo(ctx, cfg.Mongo)
	if err != nil {
		t.Fatalf("連 MongoDB 失敗: %v", err)
	}
	defer func() {
		if err := client.Disconnect(context.Background()); err != nil {
			t.Errorf("關閉 MongoDB 連線失敗: %v", err)
		}
	}()

	if got := db.Name(); got != cfg.Mongo.Database {
		t.Errorf("資料庫名 = %q, want %q", got, cfg.Mongo.Database)
	}

	t.Run("可寫可讀（讀端投影的前提）", func(t *testing.T) {
		coll := db.Collection("_smoke_probe")
		defer coll.Drop(ctx)

		if _, err := coll.InsertOne(ctx, map[string]any{"probe": true}); err != nil {
			t.Fatalf("寫入失敗: %v", err)
		}
		n, err := coll.CountDocuments(ctx, map[string]any{"probe": true})
		if err != nil {
			t.Fatalf("查詢失敗: %v", err)
		}
		if n != 1 {
			t.Errorf("文件數 = %d, want 1", n)
		}
	})
}

func TestOpenRedis(t *testing.T) {
	cfg := loadOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := OpenRedis(ctx, cfg.Redis)
	if err != nil {
		t.Fatalf("連 Redis 失敗: %v", err)
	}
	defer client.Close()

	t.Run("ZSET 可用（排行榜是主儲存）", func(t *testing.T) {
		const key = "_smoke_probe:zset"
		defer client.Del(ctx, key)

		alice := redis.Z{Score: 100, Member: "alice"}
		bob := redis.Z{Score: 200, Member: "bob"}
		if err := client.ZAdd(ctx, key, alice, bob).Err(); err != nil {
			t.Fatalf("ZAdd 失敗: %v", err)
		}
		top, err := client.ZRevRange(ctx, key, 0, 0).Result()
		if err != nil {
			t.Fatalf("ZRevRange 失敗: %v", err)
		}
		if len(top) != 1 || top[0] != "bob" {
			t.Errorf("排行榜第一名 = %v, want [bob]", top)
		}
	})
}

func TestMissingEnvIsRejectedNotDefaulted(t *testing.T) {
	// 密碼留空時必須拒絕，而不是用某個預設值連上去。
	// 「忘了設密碼卻連得上」在正式環境是安全問題。
	t.Setenv("MYSQL_PASSWORD", "")
	if _, err := config.LoadInfra(); err == nil {
		t.Fatal("MYSQL_PASSWORD 為空時應該被拒絕")
	}
}
