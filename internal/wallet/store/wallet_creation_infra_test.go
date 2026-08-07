//go:build infra

// 需要真的跑起來的 MySQL：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	go test -race -tags=infra ./internal/wallet/store/
package store

import (
	"sync"
	"testing"
)

// countWalletRows 統計 wallets/diamond_wallets 裡某個玩家的列數（0 或 1）。
func countWalletRows(t *testing.T, e *testEnv, table string, playerID int64) int64 {
	t.Helper()
	var n int64
	// table 一律是測試裡寫死的字面值，沒有注入面（同 testEnv.count 的判準）。
	err := e.db.WithContext(e.ctx).
		Raw("SELECT COUNT(*) FROM "+table+" WHERE player_id = ?", playerID).
		Row().Scan(&n)
	if err != nil {
		t.Fatalf("統計 %s 筆數失敗: %v", table, err)
	}
	return n
}

// TestCreateWalletIsIdempotent 對齊 Java 的 existsById 預檢：
// 呼叫兩次，第二次是 no-op，資料庫只留一列。
func TestCreateWalletIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	const player = 101

	if err := env.repo.CreateWallet(env.ctx, player); err != nil {
		t.Fatalf("第一次 CreateWallet 失敗: %v", err)
	}
	if err := env.repo.CreateWallet(env.ctx, player); err != nil {
		t.Fatalf("第二次 CreateWallet 應該是冪等 no-op，卻回傳 error: %v", err)
	}

	if n := countWalletRows(t, env, "wallets", player); n != 1 {
		t.Errorf("wallets 列數 = %d, want 1", n)
	}
	balance, version := readWallet(t, env.ctx, env.db, player)
	if balance != 0 {
		t.Errorf("balance = %d, want 0", balance)
	}
	if version != 0 {
		t.Errorf("version = %d, want 0", version)
	}
}

// TestCreateDiamondWalletIsIdempotent 是星幣那個測試的鑽石版本。
func TestCreateDiamondWalletIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	const player = 102

	if err := env.repo.CreateDiamondWallet(env.ctx, player); err != nil {
		t.Fatalf("第一次 CreateDiamondWallet 失敗: %v", err)
	}
	if err := env.repo.CreateDiamondWallet(env.ctx, player); err != nil {
		t.Fatalf("第二次 CreateDiamondWallet 應該是冪等 no-op，卻回傳 error: %v", err)
	}

	if n := countWalletRows(t, env, "diamond_wallets", player); n != 1 {
		t.Errorf("diamond_wallets 列數 = %d, want 1", n)
	}
}

// ⭐ TestCreateWalletConcurrentDuplicateInsert 驗「同一個 playerID 被併發建立
// 多次」的不變量：零 error、剛好一列。這是地雷 #26 教條（冪等鍵走 UNIQUE
// 索引衝突而非先查後寫）在建立錢包這個場景下的直接證據——如果這裡改回
// SELECT-then-INSERT，這個測試會在併發下不穩定地紅（偶爾兩個 goroutine
// 都通過 SELECT、都嘗試 INSERT，其中一個仍會撞 1062，但如果沒有 isDupEntry
// 兜底就會整個失敗，暴露出「先查後寫」中間的 race）。
func TestCreateWalletConcurrentDuplicateInsert(t *testing.T) {
	env := newTestEnv(t)
	const player = 103
	const workers = 10

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := env.repo.CreateWallet(env.ctx, player); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("併發建立同一個錢包不該有任何失敗（撞到的都該被 isDupEntry 吞掉）: %v", errs)
	}
	if n := countWalletRows(t, env, "wallets", player); n != 1 {
		t.Errorf("wallets 列數 = %d, want 1", n)
	}
}

// TestCreateDiamondWalletConcurrentDuplicateInsert 是上面那個測試的鑽石版本。
func TestCreateDiamondWalletConcurrentDuplicateInsert(t *testing.T) {
	env := newTestEnv(t)
	const player = 104
	const workers = 10

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := env.repo.CreateDiamondWallet(env.ctx, player); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("併發建立同一個鑽石錢包不該有任何失敗: %v", errs)
	}
	if n := countWalletRows(t, env, "diamond_wallets", player); n != 1 {
		t.Errorf("diamond_wallets 列數 = %d, want 1", n)
	}
}
