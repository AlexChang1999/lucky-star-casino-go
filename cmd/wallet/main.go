// Command wallet 是帳務服務的進入點——本專案第一個「跑得起來的服務」。
//
//	set -a && . deploy/.env && set +a
//	go run ./cmd/migrate up      # schema 不會自己跑（docs/ADR-003）
//	go run ./cmd/wallet
//
// ⭐ 依賴組裝全部在這個檔裡，用**明確傳參**完成，不引入 wire / fx / dig
// （CLAUDE.md §2）。Java 那邊 `@Autowired` 一寫，「誰依賴誰」要靠 IDE 追；
// 這裡由上往下讀一次就看得完：設定 → 連線 → repository → handler → 伺服器。
// **那是優點不是缺點**，而且是面試講得出來的判斷。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/migrate"
	platformstore "github.com/AlexChang1999/lucky-star-casino-go/internal/platform/store"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/httpapi"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// startupTimeout 是「連上 DB + 兩項開機自檢」的總預算。
//
// ⚠️ 一定要有上限。沒有的話，MySQL 還沒起來時服務會**永遠**卡在連線上，
// 而編排器看到的是「容器活著但一直沒就緒」——比明確的啟動失敗難查得多。
const startupTimeout = 20 * time.Second

func main() {
	if err := run(); err != nil {
		// ⚠️ 用 slog.Default() 而不是 fmt.Fprintln：啟動失敗的訊息與其他日誌
		// 走同一個格式，日誌收集器才收得到。這是「錯誤要看得見」的一部分。
		slog.Error("wallet 啟動失敗", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// ── 設定 ────────────────────────────────────────────────────────────
	// ⚠️ 兩個 loader 各自回錯、最後 Join：缺 MYSQL_PASSWORD 與缺
	// INTERNAL_SECRET 要能**一次看完**，而不是修一個、重跑、再看到下一個。
	mysqlCfg, mysqlErr := config.LoadMySQL()
	walletCfg, walletErr := config.LoadWallet()
	if err := errors.Join(mysqlErr, walletErr); err != nil {
		return fmt.Errorf("載入設定失敗（是不是忘了 set -a; . deploy/.env; set +a）: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: walletCfg.LogLevel}))
	slog.SetDefault(logger)

	// ⚠️ 這裡刻意**不印** InternalSecret。「只是啟動日誌，印出來比較好查」是
	// secret 外洩最常見的路徑——日誌通常比設定檔傳得更廣、留得更久。
	logger.Info("wallet 啟動中",
		"port", walletCfg.HTTP.Port,
		"mysql", fmt.Sprintf("%s:%d/%s", mysqlCfg.Host, mysqlCfg.Port, mysqlCfg.Database),
		"sqlLog", walletCfg.SQLLog,
		"logLevel", walletCfg.LogLevel.String(),
	)

	// ── 關機訊號 ────────────────────────────────────────────────────────
	// ⚠️ 一定要收 SIGTERM，那是 `docker stop` 與 K8s 送的訊號。只收 SIGINT
	// 的話容器每次都會被等到寬限期結束再 SIGKILL——而 SIGKILL 不會送
	// Kafka 的 LeaveGroup，於是留下幽靈 member（AGENTS.md 地雷 #25）。
	// 這個服務目前還沒有 consumer，但這個習慣要從第一個服務就建立。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	defer cancelStartup()

	// ── MySQL ───────────────────────────────────────────────────────────
	// ⭐ 帳務服務把 SQL 攤開在結構化 log 裡（藍圖 §3.2）。這是 CHANGELOG 在
	// debit 那一輪記下的待辦「db.Debug() 尚未接上，因為還沒有 cmd/wallet」——
	// 現在有了。
	db, err := platformstore.OpenMySQL(startupCtx, mysqlCfg,
		platformstore.NewGormLogger(logger, walletCfg.SQLLog))
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("取得底層 *sql.DB 失敗: %w", err)
	}
	defer func() {
		if cerr := sqlDB.Close(); cerr != nil {
			logger.Error("關閉 MySQL 連線失敗", "err", cerr)
		}
	}()

	// ── 開機自檢 ────────────────────────────────────────────────────────
	// 兩項是**互補**不是重複（各自的 doc comment 有完整說明）：
	//   VerifyVersion       問「migration 跑到最新了嗎」——通用，版本號
	//   VerifyWalletSchema  問「跑出來的東西長得對嗎」——具體，定序 / CHECK / UNIQUE
	//
	// ⚠️ 兩者都只**讀**不**寫**。服務啟動時自動 migrate 在多副本下是併發 DDL，
	// 而 MySQL 沒有交易式 DDL，結果是半套 schema（地雷 #31、docs/ADR-003）。
	// ⚠️ 失敗就開不起來，這是刻意的：帶著錯的 schema 跑起來，第一個症狀會是
	// 一筆對不起來的帳，而不是一則錯誤訊息。
	if err := migrate.VerifyVersion(startupCtx, sqlDB); err != nil {
		return err
	}
	if err := walletstore.VerifyWalletSchema(startupCtx, db); err != nil {
		return fmt.Errorf("wallet schema 自檢失敗: %w", err)
	}
	logger.Info("開機自檢通過（schema 版本與帳務約束）")

	// ── 組裝 ────────────────────────────────────────────────────────────
	// 一路傳值下去，沒有容器、沒有反射、沒有 init()。
	repo := walletstore.NewRepository(db, logger)
	handler, err := httpapi.New(repo, logger, walletCfg.InternalSecret)
	if err != nil {
		return err
	}

	return serve(ctx, logger, walletCfg.HTTP, handler)
}

// serve 啟動 HTTP 伺服器並在收到關機訊號時優雅收工。
func serve(ctx context.Context, logger *slog.Logger, cfg config.HTTPServer, handler http.Handler) error {
	srv := &http.Server{
		Addr:    cfg.Addr(),
		Handler: handler,
		// ⚠️ 四個逾時全部顯式設定。`http.Server` 的零值是「永不逾時」，
		// 少設任何一個都等於留一條可以被慢速連線佔住的路（見 config.HTTPServer）。
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		// net/http 內部的錯誤（例如 TLS handshake 失敗）預設走 log 標準庫直接
		// 印到 stderr，格式與其他日誌不同。轉接到 slog 讓輸出只有一種形狀。
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP 伺服器開始接受連線", "addr", srv.Addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			// 這是 Shutdown 造成的正常結束，不是錯誤。
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("HTTP 伺服器意外結束: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("收到關機訊號，開始優雅關機")
	}

	// ⚠️ context.WithoutCancel：ctx 在這一刻**已經被取消了**（那正是我們走到
	// 這裡的原因）。直接拿它去 WithTimeout，得到的 context 一出生就是 done，
	// Shutdown 會立刻放棄、把還在跑的請求硬斷掉——優雅關機變成不優雅關機，
	// 而且完全沒有錯誤訊息。與 Kafka 收工時 commit offset 是同一個坑（地雷 #20）。
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("優雅關機逾時（%s），仍有請求未完成: %w", cfg.ShutdownTimeout, err)
	}
	// Shutdown 回來之後 ListenAndServe 那個 goroutine 一定已經返回，收掉它的結果，
	// 順便讓 -race 看得出這裡沒有洩漏的 goroutine。
	if err := <-serveErr; err != nil {
		return fmt.Errorf("HTTP 伺服器結束時回報錯誤: %w", err)
	}
	logger.Info("wallet 已停止")
	return nil
}
