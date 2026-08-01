// Package store 建立三個資料儲存的連線。
//
// 這裡**只負責連線與連線池設定**，不含任何業務查詢——各服務的 repository
// 拿到 *gorm.DB / *mongo.Database / *redis.Client 之後自己組。
//
// ⚠️ 這個套件刻意**沒有定義 interface**。CLAUDE.md §2 的規則是
// 「interface 放在消費端與外部邊界」——需要替身的是 repository（業務查詢），
// 不是連線本身。在這裡定義 `type DB interface{...}` 只會多一層沒有人需要
// 替換的間接。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

// OpenMySQL 連上帳務寫入主庫並套用連線池設定。
//
// ⚠️ GORM 的 Open **不會真的建立連線**（database/sql 是惰性的），
// 所以這裡明確 Ping 一次。少了它，設定錯誤要等到第一個查詢才爆，
// 而那時候的堆疊指向業務程式碼，不是這裡。
func OpenMySQL(ctx context.Context, cfg config.MySQL) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(cfg.DSN()), &gorm.Config{
		// 預設的 logger 會把每一條 SQL 印出來，在正式環境是雜訊也是效能負擔。
		// ⚠️ 但帳務服務要反過來——見 docs/ADR-001 後果第 2 點，
		// 那裡要 db.Debug() 把 SQL 攤開，因為冪等鍵與樂觀鎖正是
		// 「ORM 最容易騙人」的地方。
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
		// 命名策略走 GORM 預設（表名自動複數化）。這剛好對得上團隊 repo
		// 的既有表名——wallets、wallet_transactions、game_rounds 都是複數，
		// 所以不需要覆寫。⚠️ 遇到不是這個形狀的表名時要在該 model 上
		// 明寫 TableName()，不要為了一張表改全域策略。
	})
	if err != nil {
		return nil, fmt.Errorf("開啟 MySQL 連線失敗: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("取得 MySQL 底層 *sql.DB 失敗: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("MySQL Ping 失敗（%s:%d）: %w", cfg.Host, cfg.Port, err)
	}
	return db, nil
}

// OpenMongo 連上 CQRS 讀端。
//
// ⚠️ mongo.Connect 同樣不會立刻建立連線，必須 Ping 才知道帳密與位址對不對。
func OpenMongo(ctx context.Context, cfg config.Mongo) (*mongo.Client, *mongo.Database, error) {
	opts := options.Client().
		ApplyURI(cfg.URI()).
		// 讀端的查詢應該要快。逾時設短一點，讓「讀端掛了」變成一個明確的
		// 錯誤而不是慢慢拖垮上游——讀端是可重建的衍生資料，
		// 降級（改查 MySQL 或直接回錯）遠比拖著好。
		SetServerSelectionTimeout(5 * time.Second).
		SetConnectTimeout(5 * time.Second)

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("開啟 MongoDB 連線失敗: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, nil, fmt.Errorf("MongoDB Ping 失敗（%s:%d）: %w", cfg.Host, cfg.Port, err)
	}
	return client, client.Database(cfg.Database), nil
}

// OpenRedis 連上 Redis。
//
// ⚠️ 這裡存的是**業務資料不是快取**（AGENTS.md 地雷 #16）：
// 排行榜 ZSET、捕魚 session、後台停用標記。所以「Redis 連不上」
// 不能當成「快取 miss」降級處理，必須是啟動失敗。
func OpenRedis(ctx context.Context, cfg config.Redis) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: cfg.Addr(),
		// go-redis 預設池大小是 10 × GOMAXPROCS，對本專案夠用，
		// 所以不覆蓋——沒有量過就不要調，寫死一個猜的數字比預設更糟。
	})
	if err := client.Ping(ctx).Err(); err != nil {
		// ST1005（error string 不可大寫開頭）在這裡是誤判：那條規則的本意是
		// 「除非是專有名詞或縮寫」，而 staticcheck 的啟發式只認得**含兩個以上
		// 大寫字母**的字——所以上面的 MySQL / MongoDB 不會被標，只有 Redis 被標。
		// 訊息形狀刻意與另外兩個保持一致，不為了討好 linter 而寫成別的樣子。
		//nolint:staticcheck // ST1005: Redis 是專有名詞
		return nil, fmt.Errorf("Redis Ping 失敗（%s）: %w", cfg.Addr(), err)
	}
	return client, nil
}
