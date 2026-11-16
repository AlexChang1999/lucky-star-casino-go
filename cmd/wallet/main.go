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
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/kafka"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/migrate"
	platformstore "github.com/AlexChang1999/lucky-star-casino-go/internal/platform/store"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/handler"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/memberconsume"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/outbox"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/quota"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/service"
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
	kafkaCfg, kafkaErr := config.LoadKafka()
	walletCfg, walletErr := config.LoadWallet()
	redisCfg, redisErr := config.LoadRedis()
	if err := errors.Join(mysqlErr, kafkaErr, walletErr, redisErr); err != nil {
		return fmt.Errorf("載入設定失敗（是不是忘了 set -a; . deploy/.env; set +a）: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: walletCfg.LogLevel}))
	slog.SetDefault(logger)

	// ⚠️ 這裡刻意**不印** InternalSecret。「只是啟動日誌，印出來比較好查」是
	// secret 外洩最常見的路徑——日誌通常比設定檔傳得更廣、留得更久。
	logger.Info("wallet 啟動中",
		"port", walletCfg.HTTP.Port,
		"mysql", fmt.Sprintf("%s:%d/%s", mysqlCfg.Host, mysqlCfg.Port, mysqlCfg.Database),
		"kafka", kafkaCfg.Brokers,
		"sqlLog", walletCfg.SQLLog,
		"logLevel", walletCfg.LogLevel.String(),
	)

	// ── 關機訊號 ────────────────────────────────────────────────────────
	// ⚠️ 一定要收 SIGTERM，那是 `docker stop` 與 K8s 送的訊號。只收 SIGINT
	// 的話容器每次都會被等到寬限期結束再 SIGKILL——而 SIGKILL 不會送
	// Kafka 的 LeaveGroup，於是留下幽靈 member（AGENTS.md 地雷 #25）。
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
	// 依賴方向只往下：handler → service → store（docs/ADR-004）。
	repo := walletstore.NewRepository(db, logger)

	// Redis 額度層：**不是快取**而是額度狀態本身（地雷 #16），故障策略是 fail-closed。
	// ⚠️ 這裡不 Ping：Redis 掛了不該讓帳務服務開不起來（debit/credit 用不到它），
	// 只有破產救助會在請求時回 500。與下面 Kafka 不做開機自檢是同一個取捨。
	redisClient := redis.NewClient(&redis.Options{Addr: redisCfg.Addr()})
	defer func() {
		if cerr := redisClient.Close(); cerr != nil {
			logger.Error("關閉 Redis 連線失敗", "err", cerr)
		}
	}()
	quotaStore, err := quota.New(redisClient, logger)
	if err != nil {
		return err
	}

	walletSvc := service.New(repo, quotaStore, logger)
	walletHandler, err := handler.New(walletSvc, logger)
	if err != nil {
		return err
	}
	app, err := handler.NewApp(walletHandler, walletCfg.InternalSecret, walletCfg.HTTP)
	if err != nil {
		return err
	}

	// ── Transactional Outbox 的投遞側（AGENTS.md 地雷 #5）───────────────
	// ⚠️ 這裡**刻意不做 Kafka 的開機自檢**，與上面兩項 schema 自檢相反。
	// 理由是兩者的失敗後果不同：schema 錯了會算錯帳，所以寧可開不起來；
	// Kafka 掛了則只是事件送不出去——而帳務照樣可以正確地寫進 MySQL 與 outbox，
	// 等 Kafka 回來再補送。**為了下游而讓帳務服務開不起來，是把可用性倒過來換。**
	// （kafka-go 的 writer 本來就是延遲連線的，這裡不連也不會有事。）
	publisher := outbox.NewPublisher(
		outbox.NewWriter(kafkaCfg.Brokers, walletCfg.Outbox.BatchSize, logger), logger)
	poller := outbox.NewPoller(repo, publisher.Publish, walletCfg.Outbox, logger)
	purger := outbox.NewPurger(repo, walletCfg.Outbox.Retention, logger)

	// ── member.registered 消費端（AGENTS.md 地雷 #19/#20/#22/#25/#38/#40）──
	// wallet 服務**唯一**建立錢包的路徑：沒有任何 HTTP 端點會 lazy-create。
	memberReader, err := kafka.NewReader(
		kafkaCfg.Brokers, domain.TopicMemberRegistered, kafkaCfg.MemberRegisteredGroupID, logger)
	if err != nil {
		return err
	}
	memberHandler := memberconsume.NewHandler(repo, kafkaCfg.Brokers, logger)
	memberConsumer := kafka.NewConsumer(memberReader, "wallet-member-registered", memberHandler.Handle, logger)

	// ⚠️ 收工順序是**由外往內**，而且不能靠 defer 的 LIFO 去湊：
	//   ① serve 返回（ctx 已取消）→ ② 等三個背景 goroutine 跑完當前這一輪
	//   → ③ 關 member consumer 與它的 DLT writer → ④ 關 outbox Kafka writer
	//   → ⑤ 關 MySQL（上面那個 defer，LIFO 排在最後）
	// 順序反了的症狀都很難查：先關 writer，最後一輪會把已寫入 DB 的事件
	// 誤判成投遞失敗；先關 DB，poller 會在標記 SENT 那一步炸掉。member
	// consumer 排在 outbox writer 之前關，是因為它讀 Kafka、跟 outbox 寫
	// Kafka 邏輯上獨立，但兩者都是「先停背景 goroutine、才收尾巴的 Kafka
	// 資源」，跟著同一個外往內的次序走，比另外發明一套順序更好記。
	var background sync.WaitGroup
	background.Add(3)
	go func() {
		defer background.Done()
		poller.Run(ctx)
	}()
	go func() {
		defer background.Done()
		purger.Run(ctx)
	}()
	go func() {
		defer background.Done()
		if err := memberConsumer.Run(ctx); err != nil {
			logger.Error("member.registered consumer 意外結束", "err", err)
		}
	}()
	defer func() {
		background.Wait()
		if err := memberConsumer.Close(); err != nil {
			logger.Error("關閉 member.registered consumer 失敗", "err", err)
		}
		if err := memberHandler.Close(); err != nil {
			logger.Error("關閉 member.registered DLT writer 失敗", "err", err)
		}
		if err := publisher.Close(); err != nil {
			logger.Error("關閉 Kafka writer 失敗", "err", err)
		}
	}()

	return serve(ctx, logger, walletCfg.HTTP, app)
}

// serve 啟動 Fiber 伺服器並在收到關機訊號時優雅收工。
func serve(ctx context.Context, logger *slog.Logger, cfg config.HTTPServer, app *fiber.App) error {
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP 伺服器開始接受連線", "addr", cfg.Addr())
		// ⚠️ ListenerNetwork 明寫 tcp：Fiber 預設是 tcp4（只綁 IPv4），與 net/http 的
		// 預設（雙堆疊）不同，於是「埠已被占用」的偵測行為在不同平台上會不一樣。
		// 啟動橫幅關掉：它往 stdout 印純文字，混進 JSON 日誌會讓收集器只解得開一半。
		serveErr <- app.Listen(cfg.Addr(), fiber.ListenConfig{
			ListenerNetwork:       fiber.NetworkTCP,
			DisableStartupMessage: true,
		})
	}()

	select {
	case err := <-serveErr:
		// Listen 在 Shutdown 之前返回，一律是異常（例如埠被占用）。
		if err != nil {
			return fmt.Errorf("HTTP 伺服器意外結束: %w", err)
		}
		return errors.New("HTTP 伺服器在收到關機訊號前就結束了")
	case <-ctx.Done():
		logger.Info("收到關機訊號，開始優雅關機")
	}

	// ⚠️ context.WithoutCancel：ctx 在這一刻**已經被取消了**（那正是我們走到這裡的原因）。
	// 直接拿它去 WithTimeout，得到的 context 一出生就是 done，Shutdown 會立刻放棄、
	// 把還在跑的請求硬斷掉——優雅關機變成不優雅關機，而且完全沒有錯誤訊息。
	// 與 Kafka 收工時 commit offset 是同一個坑（地雷 #20）。
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		return fmt.Errorf("優雅關機逾時（%s），仍有請求未完成: %w", cfg.ShutdownTimeout, err)
	}
	// Shutdown 回來之後 Listen 那個 goroutine 一定已經返回，收掉它的結果，
	// 順便讓 -race 看得出這裡沒有洩漏的 goroutine。
	if err := <-serveErr; err != nil {
		return fmt.Errorf("HTTP 伺服器結束時回報錯誤: %w", err)
	}
	logger.Info("wallet 已停止")
	return nil
}
