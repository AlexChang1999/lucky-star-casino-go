// Package store 是 wallet 的 MySQL 存取層。
//
// schema.go 是開機自檢，repository.go 是帳務語句本體（docs/ADR-002）。
// credit 的實作接在後面。
package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// binCollation 是帳務字串欄位必須採用的定序。
//
// MySQL 8.4 的預設 utf8mb4_0900_ai_ci **不分大小寫也不分音標**，會同時
// 弱化兩件事（AGENTS.md 地雷 #30，schema 檔頭有完整說明）：
//   - UNIQUE 冪等鍵：'checkin-42' 與 'CHECKIN-42' 被當成同一把 → 少入一筆帳
//   - CHECK IN 列舉：放行小寫 'debit' 並原樣存入 → Go 端字串比對失敗
const binCollation = "utf8mb4_bin"

// requiredBinCollation 列出必須是 utf8mb4_bin 的欄位。
//
// ⚠️ 不是所有字串欄位都該進這張表：reference_id、topic、kafka_key 用預設
// 定序沒問題，因為它們不參與相等性判定或列舉約束。定序是 per-column 的
// 正確性選擇，不是全域開關。
var requiredBinCollation = map[string][]string{
	"wallet_transactions": {"idempotency_key", "type", "sub_type"},
	"wallet_outbox":       {"status"},
}

// requiredTables 是 wallet 開機必須存在的三張表。
var requiredTables = []string{"wallets", "wallet_transactions", "wallet_outbox"}

// requiredChecks 是必須存在的 CHECK 約束。
//
// 這些約束是帳務的**最後一道防線**：應用層驗證會被繞過（直接下 SQL、
// 別的服務誤接同一個庫），DB 約束不會。
var requiredChecks = []string{
	"chk_wallets_balance",       // balance >= 0，防超扣
	"chk_wallets_frozen_amount", // frozen_amount >= 0
	"chk_wt_type",
	"chk_wt_sub_type",
	"chk_wt_amount", // amount > 0，防負數扣款變相入帳
	"chk_wallet_outbox_status",
}

// VerifyWalletSchema 在服務啟動時確認 schema 已正確套用。
//
// ⚠️ 它與 internal/platform/migrate.VerifyVersion 是**互補**的，不是重複：
//   - VerifyVersion 問「migration 跑到最新了嗎」——通用，任何未來的
//     migration 忘了跑都會被抓到，但它只看版本號。
//   - 這個函式問「跑出來的東西長得對嗎」——具體，釘住定序、CHECK、UNIQUE
//     這些版本號看不出來的事（有人手動 ALTER 過、或 baseline 的
//     `CREATE TABLE IF NOT EXISTS` 在既有的表上整段跳過）。
//
// 兩個都要，而且都只在開機時跑一次，不在熱路徑上。
//
// ⚠️ 它一次回報**所有**問題（errors.Join）而不是遇到第一個就返回，
// 沿用 internal/platform/config 的做法：修一個、重跑、再看到下一個，
// 對「忘了跑 migration」這種一次錯一整批的情境是純粹的浪費。
func VerifyWalletSchema(ctx context.Context, db *gorm.DB) error {
	var problems []error

	if err := verifyTables(ctx, db); err != nil {
		// 表都不在就不必往下驗欄位了——後面每一項都會失敗，
		// 噴一整頁雜訊反而蓋掉真正的那一行。
		return err
	}
	if err := verifyCollations(ctx, db); err != nil {
		problems = append(problems, err)
	}
	if err := verifyChecks(ctx, db); err != nil {
		problems = append(problems, err)
	}
	if err := verifyIdempotencyUnique(ctx, db); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func verifyTables(ctx context.Context, db *gorm.DB) error {
	var found []string
	err := db.WithContext(ctx).Raw(`
		SELECT TABLE_NAME FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (?)`, requiredTables).
		Scan(&found).Error
	if err != nil {
		return fmt.Errorf("查詢 wallet 資料表失敗: %w", err)
	}
	if len(found) == len(requiredTables) {
		return nil
	}
	present := make(map[string]bool, len(found))
	for _, name := range found {
		present[name] = true
	}
	var missing []string
	for _, name := range requiredTables {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	return fmt.Errorf("wallet schema 尚未套用，缺少資料表 %v"+
		"（執行 `go run ./cmd/migrate up`）", missing)
}

func verifyCollations(ctx context.Context, db *gorm.DB) error {
	type row struct {
		TableName     string
		ColumnName    string
		CollationName string
	}
	var rows []row
	err := db.WithContext(ctx).Raw(`
		SELECT TABLE_NAME AS table_name, COLUMN_NAME AS column_name, COLLATION_NAME AS collation_name
		  FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (?)`, requiredTables).
		Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("查詢欄位定序失敗: %w", err)
	}

	actual := make(map[string]string, len(rows))
	for _, r := range rows {
		actual[r.TableName+"."+r.ColumnName] = r.CollationName
	}

	var problems []error
	for table, columns := range requiredBinCollation {
		for _, col := range columns {
			key := table + "." + col
			got, ok := actual[key]
			if !ok {
				problems = append(problems, fmt.Errorf("找不到欄位 %s", key))
				continue
			}
			if got != binCollation {
				problems = append(problems, fmt.Errorf(
					"%s 的定序是 %s，必須是 %s——預設定序不分大小寫，會讓冪等鍵與列舉約束失效（地雷 #30）",
					key, got, binCollation))
			}
		}
	}
	return errors.Join(problems...)
}

func verifyChecks(ctx context.Context, db *gorm.DB) error {
	var found []string
	err := db.WithContext(ctx).Raw(`
		SELECT CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS
		 WHERE TABLE_SCHEMA = DATABASE() AND CONSTRAINT_TYPE = 'CHECK'`).
		Scan(&found).Error
	if err != nil {
		return fmt.Errorf("查詢 CHECK 約束失敗: %w", err)
	}
	present := make(map[string]bool, len(found))
	for _, name := range found {
		present[name] = true
	}
	var missing []string
	for _, name := range requiredChecks {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少 CHECK 約束 %v——它們是帳務的最後一道防線，不可省略", missing)
	}
	return nil
}

// verifyIdempotencyUnique 確認冪等鍵的 UNIQUE 索引存在。
//
// ⚠️ 這是全專案最重要的一條約束。冪等靠的是**UNIQUE 索引衝突**，
// 不是「先 SELECT 再 INSERT」（那中間有 race，AGENTS.md 地雷 #3）。
// 索引不在的話，重複入帳不會有任何錯誤訊息，只會多一筆錢。
func verifyIdempotencyUnique(ctx context.Context, db *gorm.DB) error {
	var nonUnique []int
	err := db.WithContext(ctx).Raw(`
		SELECT NON_UNIQUE FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'wallet_transactions'
		   AND COLUMN_NAME = 'idempotency_key'`).Scan(&nonUnique).Error
	if err != nil {
		return fmt.Errorf("查詢冪等鍵索引失敗: %w", err)
	}
	for _, n := range nonUnique {
		if n == 0 {
			return nil // NON_UNIQUE = 0 代表這是唯一索引
		}
	}
	return errors.New("wallet_transactions.idempotency_key 沒有 UNIQUE 索引——" +
		"冪等靠的是唯一索引衝突，少了它重複入帳不會有任何錯誤訊息")
}
