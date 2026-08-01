// Package mysqltest 提供 infra 測試共用的 MySQL 輔助工具。
//
// 為什麼是一個**正式套件**而不是複製一份 helper 到每個測試檔：Go 的
// `_test.go` 不能跨套件共用，而「建一個臨時資料庫、用完就丟」這件事
// internal/platform/migrate 與 internal/wallet/store 都需要，之後每個服務的
// store 測試也會需要。標準庫的 `net/http/httptest` 就是同一個形狀——
// **測試支援程式碼是正式程式碼，只是沒有人在正式路徑上 import 它**。
//
// ⚠️ 它在非 _test.go 檔裡 import `testing`，這是刻意的（httptest 也是）。
// 代價是任何 import 它的套件都會把 testing 連進去，所以**只准測試檔 import**。
package mysqltest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

// NewScratchDB 建一個用完就丟的資料庫，回傳連上它的 *sql.DB。
//
// 為什麼要臨時資料庫而不是直接用 lucky_star_casino：
//   - migration 的 down 會 DROP 掉帳務表，在共用的開發庫上跑等於清掉別人的資料
//   - 更重要的是，只有從**空的**資料庫開始，才能證明「這份 migration 真的
//     建得出正確的 schema」。在一個已經有表的庫上跑 `CREATE TABLE IF NOT EXISTS`
//     什麼都沒驗到，測試會綠得毫無意義
//
// ⚠️ 用 root 連線是刻意的：MySQL 官方映像只把 MYSQL_DATABASE 的權限授給
// MYSQL_USER（`GRANT ALL ON `lucky_star_casino`.*`），業務帳號**建不了新資料庫**。
// 那個限制是對的，不要為了測試方便去放寬它。
//
// 資料庫會在測試結束時（t.Cleanup）自動刪除。
func NewScratchDB(t *testing.T) *sql.DB {
	t.Helper()

	cfg, err := config.LoadMySQL()
	if err != nil {
		t.Fatalf("載入設定失敗（是不是忘了 set -a; . deploy/.env; set +a）: %v", err)
	}
	rootPassword := os.Getenv("MYSQL_ROOT_PASSWORD")
	if rootPassword == "" {
		t.Fatal("MYSQL_ROOT_PASSWORD 未設定——建立臨時資料庫需要它")
	}

	// Database 留空時 DSN 會是 root:pw@tcp(host:port)/?params：連得上伺服器但不選庫。
	adminCfg := cfg
	adminCfg.User = "root"
	adminCfg.Password = rootPassword
	adminCfg.Database = ""

	admin, err := sql.Open("mysql", adminCfg.DSN())
	if err != nil {
		t.Fatalf("連上 MySQL（root）失敗: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("關閉 root 連線失敗: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 名字帶 nano 時間戳，讓平行執行的測試不會互相踩。
	name := fmt.Sprintf("scratch_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatalf("建立臨時資料庫失敗: %v", err)
	}
	t.Cleanup(func() {
		// ⚠️ 這裡開一個新的 context 而不是沿用上面那個：測試結束時原本的
		// ctx 早就取消了，直接沿用會讓 DROP 送不出去、臨時庫永遠留在伺服器上。
		// 這與收工時 Kafka commit offset 要用 context.WithoutCancel 是同一類問題
		// （AGENTS.md 地雷 #20）——**清理路徑不可以吃取消訊號**。
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Errorf("刪除臨時資料庫 %s 失敗（要手動清）: %v", name, err)
		}
	})

	scratchCfg := adminCfg
	scratchCfg.Database = name
	db, err := sql.Open("mysql", scratchCfg.DSN())
	if err != nil {
		t.Fatalf("連上臨時資料庫失敗: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉臨時資料庫連線失敗: %v", err)
		}
	})
	return db
}

// TableExists 回答「目前這個資料庫裡有沒有這張表」。
func TableExists(ctx context.Context, t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&count)
	if err != nil {
		t.Fatalf("查詢資料表 %s 是否存在失敗: %v", table, err)
	}
	return count > 0
}
