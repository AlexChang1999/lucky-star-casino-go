//go:build infra

// 需要真的跑起來的 MySQL：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	go test -race -tags=infra ./internal/wallet/store/
//
// 這裡面的測試分兩類，混在一起是刻意的：
//   - 「我們的 schema 對不對」——VerifyWalletSchema
//   - 「MySQL 到底怎麼行為」——定序、CHECK、RowsAffected、重複鍵錯誤碼
//
// 第二類看起來像在測資料庫而不是測自己的程式，但它們正是 docs/ADR-002
// 的立論基礎。與其在 ADR 裡宣稱「MySQL 會這樣」，不如讓測試釘住它：
// 哪天升版行為變了，這裡會紅，而不是帳先錯。
package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/migrate"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/mysqltest"
	platformstore "github.com/AlexChang1999/lucky-star-casino-go/internal/platform/store"
)

// mysqlErrDupEntry 是 MySQL 的重複鍵錯誤碼（ER_DUP_ENTRY）。
//
// ⚠️ 為什麼要認錯誤碼而不是用 INSERT IGNORE：IGNORE 會把**所有**錯誤
// 降級成警告（截斷、NOT NULL、外鍵），於是一筆壞資料靜默變成 no-op——
// 而餘額已經扣掉了。認 1062 只吞重複鍵這一種，其餘照樣往外炸。
const mysqlErrDupEntry = 1062

func openDB(t *testing.T) (*gorm.DB, context.Context) {
	t.Helper()
	cfg, err := config.LoadInfra()
	if err != nil {
		t.Fatalf("載入設定失敗（是不是忘了 set -a; source deploy/.env）: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	db, err := platformstore.OpenMySQL(ctx, cfg.MySQL)
	if err != nil {
		t.Fatalf("連 MySQL 失敗: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err != nil {
			t.Errorf("取得底層 *sql.DB 失敗: %v", err)
			return
		}
		if err := sqlDB.Close(); err != nil {
			t.Errorf("關閉 MySQL 連線失敗: %v", err)
		}
	})
	return db, ctx
}

func TestVerifyWalletSchema(t *testing.T) {
	db, ctx := openDB(t)

	if err := VerifyWalletSchema(ctx, db); err != nil {
		t.Fatalf("schema 自檢失敗（是不是忘了跑 `go run ./cmd/migrate up`？）:\n%v", err)
	}
}

// TestMigrationProducesVerifiableSchema 把 migration 與帳務自檢接起來。
//
// ⭐ 這是兩者之間唯一的接縫測試，而且它必須跑在**空的**資料庫上。
// 上面那個 TestVerifyWalletSchema 跑在共用的開發庫，而那個庫的表可能是
// initdb.d 時代留下來的——也就是說，就算有人在 migration 裡把 COLLATE 刪掉，
// 在既有的庫上也**驗不出來**：00001 是 `CREATE TABLE IF NOT EXISTS`，
// 表已存在就整段跳過，測試照樣綠。
//
// 換句話說：沒有這個測試的話，「schema 定義」與「schema 檢查」是兩份
// 各自為政的清單，改了一邊不會有人告訴你另一邊沒跟上。
func TestMigrationProducesVerifiableSchema(t *testing.T) {
	sqlDB := mysqltest.NewScratchDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	provider, err := migrate.New(sqlDB)
	if err != nil {
		t.Fatalf("建立 migration provider 失敗: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("套用 migration 失敗: %v", err)
	}

	// 把 GORM 包在既有的 *sql.DB 上，而不是另開一條連線：臨時資料庫的
	// 生命週期由 mysqltest 管，多開一條連線就多一個要記得關的東西。
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("包裝 GORM 失敗: %v", err)
	}

	if err := VerifyWalletSchema(ctx, db); err != nil {
		t.Fatalf("migration 產出的 schema 沒有通過帳務自檢——"+
			"migration 與 VerifyWalletSchema 兩邊有一邊沒跟上:\n%v", err)
	}
}

// TestIdempotencyKeyIsCaseSensitiveInDB 釘住地雷 #30 的第一個方向。
//
// 若有人把 idempotency_key 的 COLLATE utf8mb4_bin 拿掉，
// 'checkin-42' 與 'CHECKIN-42' 會被 UNIQUE 索引判定為重複，
// 第二筆入帳被當成「冪等命中」跳過 —— **少入一筆帳，且沒有錯誤訊息**。
func TestIdempotencyKeyIsCaseSensitiveInDB(t *testing.T) {
	db, ctx := openDB(t)
	const player = 990001
	seedWallet(t, ctx, db, player, 100000)

	insert := func(key string) error {
		return db.WithContext(ctx).Exec(`
			INSERT INTO wallet_transactions (player_id, type, sub_type, amount, idempotency_key)
			VALUES (?, 'DEBIT', 'BET', 100, ?)`, player, key).Error
	}

	if err := insert("case-probe-checkin"); err != nil {
		t.Fatalf("插入小寫鍵失敗: %v", err)
	}
	if err := insert("CASE-PROBE-CHECKIN"); err != nil {
		t.Fatalf("大小寫不同的鍵應該是**不同**的鍵，卻被拒絕了——"+
			"idempotency_key 的 COLLATE utf8mb4_bin 是不是被拿掉了？: %v", err)
	}
	// 同一把鍵重複才該被擋。
	if err := insert("case-probe-checkin"); !isDupEntry(err) {
		t.Fatalf("同一把冪等鍵重複插入應回 %d，得到 %v", mysqlErrDupEntry, err)
	}
}

// TestSubTypeCheckIsCaseSensitive 釘住地雷 #30 的第二個方向。
//
// 預設的 ci 定序下，`CHECK (sub_type IN ('BET',...))` 會**放行小寫 'bet'**
// 並原樣存入，於是 Go 端 tx.SubType == "BET" 是 false、
// 事件 payload 帶著小寫進 Kafka，下游 switch 落到 default。
func TestSubTypeCheckIsCaseSensitive(t *testing.T) {
	db, ctx := openDB(t)
	const player = 990002
	seedWallet(t, ctx, db, player, 100000)

	err := db.WithContext(ctx).Exec(`
		INSERT INTO wallet_transactions (player_id, type, sub_type, amount, idempotency_key)
		VALUES (?, 'DEBIT', 'bet', 100, 'case-probe-subtype')`, player).Error
	if err == nil {
		t.Fatal("小寫 sub_type 'bet' 應該被 CHECK 約束擋下——" +
			"sub_type 的 COLLATE utf8mb4_bin 是不是被拿掉了？")
	}
}

// TestConditionalDebit 釘住 docs/ADR-002 的 debit 熱路徑語意。
//
// Java 版在 PostgreSQL 上用 `UPDATE ... RETURNING balance` 一次拿到扣款後餘額；
// MySQL 沒有 RETURNING（地雷 #26），所以改成「條件 UPDATE + 檢查 RowsAffected
// + 點查餘額」。這個測試證明三件事在 MySQL 上與 PG 版等價：
//  1. 餘額足夠且冪等鍵未用過 → 扣款成功、version +1
//  2. 餘額不足 → RowsAffected == 0 且**零副作用**
//  3. 冪等鍵已存在 → RowsAffected == 0 且**零副作用**（不會重複扣款）
func TestConditionalDebit(t *testing.T) {
	db, ctx := openDB(t)
	const player = 990003
	seedWallet(t, ctx, db, player, 1000)

	const conditionalDebit = `
		UPDATE wallets
		   SET balance = balance - ?, version = version + 1, updated_at = CURRENT_TIMESTAMP(6)
		 WHERE player_id = ?
		   AND balance - frozen_amount >= ?
		   AND NOT EXISTS (SELECT 1 FROM wallet_transactions t WHERE t.idempotency_key = ?)`

	t.Run("餘額足夠則扣款成功", func(t *testing.T) {
		res := db.WithContext(ctx).Exec(conditionalDebit, 300, player, 300, "debit-probe-1")
		if res.Error != nil {
			t.Fatalf("條件扣款失敗: %v", res.Error)
		}
		// ⚠️ MySQL driver 預設回的是「**changed** rows」不是「matched rows」。
		// 這裡之所以不會誤判，是因為每次扣款都 version + 1——恆有異動。
		// 這也是「樂觀鎖 UPDATE 一定要動 version」的真正理由之一，
		// 不只是為了版本號本身。
		if res.RowsAffected != 1 {
			t.Fatalf("RowsAffected = %d, want 1", res.RowsAffected)
		}
		balance, version := readWallet(t, ctx, db, player)
		if balance != 700 || version != 1 {
			t.Errorf("扣款後 balance=%d version=%d, want 700/1", balance, version)
		}
	})

	t.Run("餘額不足則零副作用", func(t *testing.T) {
		res := db.WithContext(ctx).Exec(conditionalDebit, 99999, player, 99999, "debit-probe-2")
		if res.Error != nil {
			t.Fatalf("條件扣款失敗: %v", res.Error)
		}
		if res.RowsAffected != 0 {
			t.Fatalf("RowsAffected = %d, want 0", res.RowsAffected)
		}
		balance, version := readWallet(t, ctx, db, player)
		if balance != 700 || version != 1 {
			t.Errorf("餘額不足時不可有任何異動，得到 balance=%d version=%d", balance, version)
		}
	})

	t.Run("冪等鍵已存在則零副作用", func(t *testing.T) {
		const key = "debit-probe-used"
		if err := db.WithContext(ctx).Exec(`
			INSERT INTO wallet_transactions (player_id, type, sub_type, amount, idempotency_key)
			VALUES (?, 'DEBIT', 'BET', 100, ?)`, player, key).Error; err != nil {
			t.Fatalf("預備流水失敗: %v", err)
		}

		res := db.WithContext(ctx).Exec(conditionalDebit, 100, player, 100, key)
		if res.Error != nil {
			t.Fatalf("條件扣款失敗: %v", res.Error)
		}
		if res.RowsAffected != 0 {
			t.Fatalf("冪等鍵已用過時 RowsAffected = %d, want 0", res.RowsAffected)
		}
		balance, _ := readWallet(t, ctx, db, player)
		if balance != 700 {
			t.Errorf("冪等命中不可扣款，餘額變成 %d", balance)
		}
	})
}

// TestDupEntryDoesNotAbortTransaction 記錄一個 MySQL 相對 PostgreSQL 的**優勢**。
//
// PostgreSQL 裡任何錯誤都會讓整筆交易進入 aborted 狀態，之後所有語句都被拒絕，
// 所以 Java 版必須寫 `ON CONFLICT DO NOTHING` 來避免炸掉交易。
// InnoDB 的重複鍵錯誤只是**語句級**失敗，交易仍然可用——
// 於是 Go 版可以直接捕捉 1062，不需要 ON CONFLICT 的等價物。
//
// ⚠️ 這條「比原版簡單」是有代價的：它讓人以為 MySQL 的錯誤都不影響交易，
// 但死鎖（1213）與鎖等待逾時（1205）**會**回滾整筆交易。認錯誤碼要精確。
func TestDupEntryDoesNotAbortTransaction(t *testing.T) {
	db, ctx := openDB(t)
	const player = 990004
	seedWallet(t, ctx, db, player, 1000)

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		const key = "dup-probe-key"
		insert := func() error {
			return tx.Exec(`
				INSERT INTO wallet_transactions (player_id, type, sub_type, amount, idempotency_key)
				VALUES (?, 'DEBIT', 'BET', 100, ?)`, player, key).Error
		}
		if err := insert(); err != nil {
			return err
		}
		if err := insert(); !isDupEntry(err) {
			return errors.New("第二次插入同鍵應回 1062，得到: " + errString(err))
		}
		// 關鍵斷言：交易被重複鍵錯誤打過之後，仍然可以繼續寫。
		if err := tx.Exec(`UPDATE wallets SET version = version + 1 WHERE player_id = ?`, player).Error; err != nil {
			return errors.New("重複鍵錯誤之後交易應仍可用，卻失敗了: " + errString(err))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	_, version := readWallet(t, ctx, db, player)
	if version != 1 {
		t.Errorf("交易應成功提交，version = %d, want 1", version)
	}
}

// --- 測試輔助 ---

// seedWallet 建立一個乾淨的測試錢包。玩家 ID 用 99xxxx 區段避開真實資料。
func seedWallet(t *testing.T, ctx context.Context, db *gorm.DB, playerID int64, balance int64) {
	t.Helper()
	cleanup := func() {
		db.WithContext(context.WithoutCancel(ctx)).Exec(`DELETE FROM wallet_transactions WHERE player_id = ?`, playerID)
		db.WithContext(context.WithoutCancel(ctx)).Exec(`DELETE FROM wallets WHERE player_id = ?`, playerID)
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := db.WithContext(ctx).Exec(
		`INSERT INTO wallets (player_id, balance) VALUES (?, ?)`, playerID, balance).Error; err != nil {
		t.Fatalf("建立測試錢包失敗: %v", err)
	}
}

func readWallet(t *testing.T, ctx context.Context, db *gorm.DB, playerID int64) (balance, version int64) {
	t.Helper()
	row := struct {
		Balance int64
		Version int64
	}{}
	if err := db.WithContext(ctx).Raw(
		`SELECT balance, version FROM wallets WHERE player_id = ?`, playerID).Scan(&row).Error; err != nil {
		t.Fatalf("讀取錢包失敗: %v", err)
	}
	return row.Balance, row.Version
}

func isDupEntry(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDupEntry
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
