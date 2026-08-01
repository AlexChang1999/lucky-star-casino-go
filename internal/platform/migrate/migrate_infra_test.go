//go:build infra

// 需要真的跑起來的 MySQL：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	set -a && . deploy/.env && set +a
//	go test -race -tags=infra ./internal/platform/migrate/
//
// ⚠️ 這裡驗的是 **migration 機制**（套用、冪等、回退、版本守門），
// 不是「schema 長得對不對」——後者是 internal/wallet/store.VerifyWalletSchema
// 的職責，而且那邊有一個測試會把兩者接起來（migration 的產出必須通過帳務自檢）。
// 兩邊各驗各的，不要在這裡複製一份欄位清單，那會變成第二份會漂移的真相。
package migrate

import (
	"context"
	"testing"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/mysqltest"
)

func TestMigrationsApplyFromEmptyDatabase(t *testing.T) {
	db := mysqltest.NewScratchDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// ⭐ 最重要的一條：完全沒跑過 migration 的資料庫必須被擋下。
	// 這正是地雷 #17（舊 volume 缺 migration）的守門員——它讓「忘了跑」
	// 在**開機時**失敗，而不是等到第一筆下注才炸在業務程式碼裡。
	if err := VerifyVersion(ctx, db); err == nil {
		t.Fatal("空資料庫應該被 VerifyVersion 擋下，卻通過了")
	}

	provider, err := New(db)
	if err != nil {
		t.Fatalf("建立 provider 失敗: %v", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("套用 migration 失敗: %v", err)
	}
	if want := len(provider.ListSources()); len(results) != want {
		t.Fatalf("套用了 %d 個 migration，內嵌的有 %d 個", len(results), want)
	}

	for _, table := range []string{"wallets", "wallet_transactions", "wallet_outbox"} {
		if !mysqltest.TableExists(ctx, t, db, table) {
			t.Errorf("migration 跑完之後 %s 不存在", table)
		}
	}
	if err := VerifyVersion(ctx, db); err != nil {
		t.Fatalf("套用完 migration 之後 VerifyVersion 仍失敗: %v", err)
	}

	t.Run("重複執行是 no-op", func(t *testing.T) {
		// 部署腳本重跑、K8s Job 重試都會發生這件事。第二次必須什麼都不做，
		// 而不是報錯——否則「重試」本身就變成一個失敗來源。
		again, err := provider.Up(ctx)
		if err != nil {
			t.Fatalf("第二次 up 失敗: %v", err)
		}
		if len(again) != 0 {
			t.Errorf("第二次 up 套用了 %d 個 migration，應該是 0 個", len(again))
		}
	})

	t.Run("down 之後表消失且版本檢查會擋", func(t *testing.T) {
		if _, err := provider.Down(ctx); err != nil {
			t.Fatalf("回退失敗: %v", err)
		}
		if mysqltest.TableExists(ctx, t, db, "wallets") {
			t.Error("down 之後 wallets 應該不存在")
		}
		if err := VerifyVersion(ctx, db); err == nil {
			t.Error("回退之後 VerifyVersion 應該擋下，卻通過了")
		}
	})
}
