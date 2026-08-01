// Package migrate 管理 MySQL 寫入主庫的 schema 版本。
//
// 為什麼需要一個 migration 工具（決策全文見 docs/ADR-003）：
// 在這之前 schema 是一份掛在 /docker-entrypoint-initdb.d/ 的 .sql，而官方映像
// **只在 volume 全新時執行它**（AGENTS.md 地雷 #17）。也就是說第一次改欄位
// 就會出現「新環境有、舊環境沒有」而且**兩邊都不報錯**的狀態。
//
// ⚠️ 這個套件刻意**沒有包一層 facade**。它只擁有三件 goose 本身不知道的事：
// migration 檔在哪（embed）、方言是 MySQL、以及開機時要怎麼判斷「版本落後」。
// `Up` / `Down` / `Status` 由呼叫端直接對 *goose.Provider 下——多包一層
// 只會讓人得多讀一份文件才知道 goose 原本就有什麼（CLAUDE.md §2）。
//
// ⚠️ migration **不在服務啟動時自動執行**。它是 cmd/migrate 這個獨立步驟
// （K8s 用 Job / initContainer）。理由是 MySQL 沒有交易式 DDL（地雷 #31），
// 多副本同時啟動一起下 DDL 會留下半套 schema，而 goose 對 MySQL 沒有
// 內建的 session lock 可以擋。服務端要做的是 VerifyVersion——**檢查**而不是**修改**。
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// migrationsFS 把 migration 檔編進 binary。
//
// 為什麼用 embed 而不是讀檔：migration 與程式碼必須是**同一個版本**。
// 讀檔的話，「binary 是新的、掛進去的 SQL 目錄是舊的」是一個跑得起來的狀態，
// 而它的症狀是欄位不存在——指不到真因。embed 讓這件事在編譯期就綁死。
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir 是 migration 檔在 embed FS 裡的目錄名。
const migrationsDir = "migrations"

// New 建立一個綁好本專案 migration 的 goose provider。
//
// ⚠️ 呼叫端**不要**對回傳的 provider 呼叫 Close()——goose 的 Close 會關掉
// 傳進來的那個 *sql.DB，而連線池的所有權在呼叫端手上。
func New(db *sql.DB, opts ...goose.ProviderOption) (*goose.Provider, error) {
	// goose 期待 fsys 的根目錄就是 migration 所在處，所以要先剝掉 migrations/。
	sub, err := fs.Sub(migrationsFS, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("讀取內嵌 migration 目錄失敗: %w", err)
	}

	// WithDisableGlobalRegistry：goose 有一個 package 級的全域 registry，
	// 讓任何被 import 的套件都能在 init() 裡塞 migration 進來。本專案的
	// migration 全部來自上面那個 embed，關掉全域註冊等於宣告
	// 「這裡列出的就是全部」——避免某天多一個 import 就多一條 migration。
	base := []goose.ProviderOption{goose.WithDisableGlobalRegistry(true)}

	provider, err := goose.NewProvider(goose.DialectMySQL, db, sub, append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("建立 goose provider 失敗: %w", err)
	}
	return provider, nil
}

// VerifyVersion 確認資料庫的 schema 版本與這個 binary 帶的一致。
//
// ⭐ 這是地雷 #17 的**真正解法**，而 internal/wallet/store.VerifyWalletSchema
// 只是它的補集：
//
//   - VerifyVersion 是**通用**的——任何未來的 migration 忘了跑都會被抓到，
//     不需要有人記得去更新一份硬編碼的檢查清單。
//   - VerifyWalletSchema 是**具體**的——它釘住「定序、CHECK、UNIQUE 這些
//     東西真的長對了」，那是版本號看不出來的（版本號只說「跑過了」，
//     不說「跑出來的結果對不對」，例如有人手動 ALTER 過）。
//
// 兩個都要，而且都只在開機時跑一次，不在熱路徑上。
//
// ⚠️ 它只**讀**不**寫**：發現版本落後是回傳錯誤讓服務開不起來，
// 不是順手幫忙 migrate。自動 migrate 在多副本下是併發 DDL（地雷 #31）。
func VerifyVersion(ctx context.Context, db *sql.DB) error {
	provider, err := New(db)
	if err != nil {
		return err
	}

	pending, err := provider.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("查詢 schema 版本失敗（是不是從來沒跑過 migration？"+
			"執行 `go run ./cmd/migrate up`）: %w", err)
	}
	if !pending {
		return nil
	}

	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("查詢 schema 版本失敗: %w", err)
	}
	return fmt.Errorf("schema 版本落後：資料庫在 %d，這個 binary 帶到 %d——"+
		"請先執行 `go run ./cmd/migrate up`", current, target)
}
