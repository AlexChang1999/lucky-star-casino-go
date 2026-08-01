// Command migrate 管理 MySQL 寫入主庫的 schema 版本（決策見 docs/ADR-003）。
//
//	set -a && . deploy/.env && set +a
//	go run ./cmd/migrate up        # 套用所有未執行的 migration
//	go run ./cmd/migrate status    # 列出每個版本的狀態
//	go run ./cmd/migrate version   # 只印資料庫目前的版本
//	go run ./cmd/migrate down -yes # ⚠️ 退回一個版本，會刪資料
//
// 為什麼是獨立的 binary 而不是在服務啟動時自動跑：MySQL 沒有交易式 DDL
// （AGENTS.md 地雷 #31），多副本同時開機一起下 DDL 會留下半套 schema，
// 而 goose 對 MySQL 沒有內建的 session lock 能擋。服務端負責的是
// migrate.VerifyVersion——**檢查**版本，不**修改** schema。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"text/tabwriter"
	"time"

	// database/sql 的 driver 註冊是 side effect，所以是空白 import。
	// migrate 走 database/sql 而不是 GORM：goose 要的就是 *sql.DB，
	// 為了一個只跑 DDL 的工具把 ORM 拖進來沒有任何好處。
	_ "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/migrate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate 失敗: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: migrate <up|down|status|version>")
	}
	command := args[0]

	cfg, err := config.LoadMySQL()
	if err != nil {
		return fmt.Errorf("載入設定失敗（是不是忘了 set -a; . deploy/.env; set +a）: %w", err)
	}

	db, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return fmt.Errorf("開啟 MySQL 連線失敗: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "關閉連線失敗: %v\n", cerr)
		}
	}()
	// migration 是單執行緒的一連串 DDL，開一條連線就夠。
	// 沿用 25 條的業務設定只是讓 MySQL 多開 24 條沒人用的連線。
	db.SetMaxOpenConns(1)

	// ⚠️ 這裡刻意**不接 SIGINT**。DDL 在 MySQL 會隱式 commit（地雷 #31），
	// 中途 Ctrl-C 不會讓已經執行的語句回滾，只會讓版本表沒被更新——
	// 也就是把「乾淨的失敗」換成「半套 schema 且工具以為沒跑過」。
	// 讓它跑完，再用 status 看結果，是比較安全的形狀。
	ctx := context.Background()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("MySQL Ping 失敗（%s:%d）: %w", cfg.Host, cfg.Port, err)
	}

	provider, err := migrate.New(db)
	if err != nil {
		return err
	}

	switch command {
	case "up":
		return commandUp(ctx, provider)
	case "down":
		return commandDown(ctx, provider, args[1:])
	case "status":
		return commandStatus(ctx, provider)
	case "version":
		return commandVersion(ctx, provider)
	default:
		return fmt.Errorf("未知的指令 %q，可用的是 up / down / status / version", command)
	}
}

func commandUp(ctx context.Context, provider *goose.Provider) error {
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("套用 migration 失敗: %w", err)
	}
	if len(results) == 0 {
		fmt.Println("沒有待套用的 migration，schema 已是最新。")
		return nil
	}
	for _, r := range results {
		fmt.Printf("已套用 %05d %s（%s）\n", r.Source.Version, path.Base(r.Source.Path), r.Duration.Round(time.Millisecond))
	}
	return nil
}

// commandDown 退回一個版本。
//
// ⚠️ 要求明寫 -yes 不是繁瑣，是因為 00001 的 Down 會 DROP 掉全部帳務表。
// 這個專案裡「打錯一個字就沒有餘額了」的指令只有這一個，值得多一道閘。
// 正式環境的回退手段是備份還原，不是這個。
func commandDown(ctx context.Context, provider *goose.Provider, rest []string) error {
	if !slices.Contains(rest, "-yes") {
		return errors.New("down 會刪掉該版本建立的資料表（含帳務資料）。" +
			"確定的話請明寫: migrate down -yes")
	}
	result, err := provider.Down(ctx)
	if err != nil {
		return fmt.Errorf("回退 migration 失敗: %w", err)
	}
	fmt.Printf("已回退 %05d %s\n", result.Source.Version, path.Base(result.Source.Path))
	return nil
}

func commandStatus(ctx context.Context, provider *goose.Provider) error {
	statuses, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("查詢 migration 狀態失敗: %w", err)
	}

	// tabwriter 會把全部內容緩衝到 Flush 才對齊輸出，所以中途每一次寫入的
	// error 都不是真正的失敗點——真的寫不出去會在 Flush 回報。這裡明寫
	// `_, _ =` 而不是留白，是為了讓「我知道這裡有 error 且刻意不看」
	// 與「忘了檢查」在程式碼上長得不一樣。
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "版本\t狀態\t套用時間\t檔名")
	for _, s := range statuses {
		appliedAt := "-"
		if !s.AppliedAt.IsZero() {
			appliedAt = s.AppliedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "%05d\t%s\t%s\t%s\n", s.Source.Version, s.State, appliedAt, path.Base(s.Source.Path))
	}
	return w.Flush()
}

func commandVersion(ctx context.Context, provider *goose.Provider) error {
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("查詢版本失敗: %w", err)
	}
	fmt.Printf("資料庫版本 %d / binary 帶到 %d\n", current, target)
	if current != target {
		// 用非零 exit code 是為了讓 CI 或部署腳本能直接判斷，
		// 不必去解析輸出字串。
		return errors.New("schema 版本落後，請執行 migrate up")
	}
	return nil
}
