//go:build infra

// 需要真的跑起來的 MySQL：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	go test -race -tags=infra ./internal/wallet/store/
//
// ⚠️ 這一檔的每個測試都在**自己的臨時資料庫**上跑（mysqltest.NewScratchDB），
// 不像 schema_infra_test.go 共用開發庫。理由有兩個：
//   - 併發測試要斷言「總共只有 N 筆流水」，共用的庫裡有別人的資料就驗不了
//   - 從空庫開始才驗得到 migration 真的建得出可用的 schema
package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/migrate"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/mysqltest"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// ── 測試環境 ────────────────────────────────────────────────────────────────

type testEnv struct {
	repo *Repository
	db   *gorm.DB
	logs *syncBuffer
	ctx  context.Context
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	sqlDB := mysqltest.NewScratchDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	provider, err := migrate.New(sqlDB)
	if err != nil {
		t.Fatalf("建立 migration provider 失敗: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("套用 migration 失敗: %v", err)
	}

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("包裝 GORM 失敗: %v", err)
	}

	logs := &syncBuffer{}
	return &testEnv{
		repo: NewRepository(db, slog.New(slog.NewTextHandler(logs, nil))),
		db:   db,
		logs: logs,
		ctx:  ctx,
	}
}

func (e *testEnv) count(t *testing.T, table string) int64 {
	t.Helper()
	var n int64
	// table 一律是測試裡寫死的字面值，沒有注入面。
	if err := e.db.WithContext(e.ctx).Raw("SELECT COUNT(*) FROM " + table).Row().Scan(&n); err != nil {
		t.Fatalf("統計 %s 筆數失敗: %v", table, err)
	}
	return n
}

// syncBuffer 是加了鎖的 bytes.Buffer。
//
// ⚠️ 併發測試裡多個 goroutine 可能同時觸發 slog 輸出，裸的 bytes.Buffer
// 會被 -race 抓到（而且是真的 race，不是誤報）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// mustDebit 是「這次扣款預期要成功」的簡寫。
func mustDebit(t *testing.T, e *testEnv, playerID int64, amount domain.Amount, key string) DebitResult {
	t.Helper()
	m, err := domain.NewDebit(playerID, amount, "", key, "")
	if err != nil {
		t.Fatalf("建立扣款意圖失敗: %v", err)
	}
	res, err := e.repo.Debit(e.ctx, m)
	if err != nil {
		t.Fatalf("扣款失敗: %v", err)
	}
	return res
}

// ── 單次呼叫的四種結局 ──────────────────────────────────────────────────────

func TestDebit(t *testing.T) {
	const player = 42

	tests := []struct {
		name string
		// seedBalance 為負代表**不建錢包**，用來測 ErrWalletNotFound。
		seedBalance    int64
		amount         domain.Amount
		subType        domain.SubType
		referenceID    string
		wantErr        error
		wantBefore     domain.Amount
		wantAfter      domain.Amount
		wantBalance    int64
		wantVersion    int64
		wantTxRows     int64
		wantOutboxRows int64
	}{
		{
			name:           "餘額足夠則扣款成功並落一筆 outbox",
			seedBalance:    1000,
			amount:         300,
			wantBefore:     1000,
			wantAfter:      700,
			wantBalance:    700,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
		},
		{
			name:           "剛好扣到零是合法的",
			seedBalance:    300,
			amount:         300,
			wantBefore:     300,
			wantAfter:      0,
			wantBalance:    0,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
		},
		{
			name:           "商城兌換帶 SHOP_PURCHASE",
			seedBalance:    1000,
			amount:         250,
			subType:        domain.SubTypeShopPurchase,
			referenceID:    "order-77",
			wantBefore:     1000,
			wantAfter:      750,
			wantBalance:    750,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
		},
		{
			// ⚠️ 零副作用是這一格的重點：不可以有流水、不可以有 outbox、
			// version 不可以動。少檢查 version 的話，「扣款失敗但版本號跳號」
			// 這種讓其他寫入方的樂觀鎖無故衝突的 bug 就漏掉了。
			name:           "餘額不足則零副作用",
			seedBalance:    100,
			amount:         300,
			wantErr:        ErrInsufficientBalance,
			wantBalance:    100,
			wantVersion:    0,
			wantTxRows:     0,
			wantOutboxRows: 0,
		},
		{
			name:           "錢包不存在",
			seedBalance:    -1,
			amount:         100,
			wantErr:        ErrWalletNotFound,
			wantTxRows:     0,
			wantOutboxRows: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tt.seedBalance >= 0 {
				seedWallet(t, env.ctx, env.db, player, tt.seedBalance)
			}

			m, err := domain.NewDebit(player, tt.amount, tt.subType, "key-"+tt.name, tt.referenceID)
			if err != nil {
				t.Fatalf("建立扣款意圖失敗: %v", err)
			}
			got, err := env.repo.Debit(env.ctx, m)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("扣款失敗: %v", err)
				}
				if got.Idempotent {
					t.Error("首次扣款不該是冪等命中")
				}
				if got.TransactionID == 0 {
					t.Error("TransactionID 應由 LastInsertId 填回，得到 0")
				}
				if got.BalanceBefore != tt.wantBefore || got.BalanceAfter != tt.wantAfter {
					t.Errorf("before/after = %d/%d, want %d/%d",
						got.BalanceBefore, got.BalanceAfter, tt.wantBefore, tt.wantAfter)
				}
				if got.Amount != tt.amount {
					t.Errorf("Amount = %d, want %d", got.Amount, tt.amount)
				}
			}

			if tt.seedBalance >= 0 {
				balance, version := readWallet(t, env.ctx, env.db, player)
				if balance != tt.wantBalance || version != tt.wantVersion {
					t.Errorf("錢包 balance/version = %d/%d, want %d/%d",
						balance, version, tt.wantBalance, tt.wantVersion)
				}
			}
			if n := env.count(t, "wallet_transactions"); n != tt.wantTxRows {
				t.Errorf("流水筆數 = %d, want %d", n, tt.wantTxRows)
			}
			if n := env.count(t, "wallet_outbox"); n != tt.wantOutboxRows {
				t.Errorf("outbox 筆數 = %d, want %d", n, tt.wantOutboxRows)
			}
		})
	}
}

// TestDebitRejectsNonDebitMovement 確認方向搞反時**不會**寫進帳務表。
//
// domain.NewDebit 產不出 CREDIT 的 Movement，但零值或手工組出來的可以。
// 這條防線的成本是一個 if，漏掉的成本是一筆 type='CREDIT' 的扣款流水——
// 而 DB 的 CHECK 是各查各的白名單，擋不住方向配錯（見 domain.subTypeDirection）。
func TestDebitRejectsNonDebitMovement(t *testing.T) {
	env := newTestEnv(t)
	seedWallet(t, env.ctx, env.db, 42, 1000)

	credit, err := domain.NewCredit(42, 100, domain.SubTypeWin, "wrong-direction", "", 0)
	if err != nil {
		t.Fatalf("建立入帳意圖失敗: %v", err)
	}
	if _, err := env.repo.Debit(env.ctx, credit); !errors.Is(err, domain.ErrUnknownTxType) {
		t.Fatalf("err = %v, want %v", err, domain.ErrUnknownTxType)
	}
	if n := env.count(t, "wallet_transactions"); n != 0 {
		t.Errorf("方向錯的 Movement 不該留下任何流水，得到 %d 筆", n)
	}
}

// ── 冪等 ────────────────────────────────────────────────────────────────────

// TestDebitIsIdempotent 是整個 wallet 最重要的一條測試。
//
// 重送同一把冪等鍵必須：不再扣款、不再寫流水、**不再發一次事件**。
// 最後一項最容易漏——餘額對得起來，但下游會收到兩次 wallet.debit，
// 而消費端若是非冪等累加（例如統計），就會多算一次（AGENTS.md 地雷 #6）。
func TestDebitIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	const player, key = 42, "bet-42-9527"
	seedWallet(t, env.ctx, env.db, player, 1000)

	first := mustDebit(t, env, player, 300, key)
	if first.Idempotent {
		t.Fatal("第一次扣款不該是冪等命中")
	}

	second := mustDebit(t, env, player, 300, key)
	if !second.Idempotent {
		t.Error("重送同一把冪等鍵應回 Idempotent=true")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("冪等命中應回**原本那筆**流水 id：得到 %d, want %d",
			second.TransactionID, first.TransactionID)
	}
	if second.BalanceBefore != first.BalanceBefore || second.BalanceAfter != first.BalanceAfter {
		t.Errorf("冪等命中應回原交易的餘額：得到 %d/%d, want %d/%d",
			second.BalanceBefore, second.BalanceAfter, first.BalanceBefore, first.BalanceAfter)
	}

	balance, version := readWallet(t, env.ctx, env.db, player)
	if balance != 700 || version != 1 {
		t.Errorf("重送不可再扣款：balance/version = %d/%d, want 700/1", balance, version)
	}
	if n := env.count(t, "wallet_transactions"); n != 1 {
		t.Errorf("流水筆數 = %d, want 1", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 1 {
		t.Errorf("⭐ 重送不可再發一次事件：outbox 筆數 = %d, want 1", n)
	}
}

// TestDebitIdempotencyKeyIsCaseSensitive 是地雷 #30 在**應用層**的表現。
//
// schema_infra_test.go 已經釘住 DB 那一層（UNIQUE 索引視大小寫為不同鍵）；
// 這裡驗的是「經過 Repository 之後行為仍然正確」——定序被改掉的話，
// 第二筆會被當成冪等命中而**少扣一次錢**，且沒有任何錯誤訊息。
func TestDebitIdempotencyKeyIsCaseSensitive(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	seedWallet(t, env.ctx, env.db, player, 1000)

	lower := mustDebit(t, env, player, 100, "checkin-42")
	upper := mustDebit(t, env, player, 100, "CHECKIN-42")

	if upper.Idempotent {
		t.Fatal("只有大小寫不同的鍵是**兩把不同的鍵**，第二次不該被判成冪等命中——" +
			"idempotency_key 的 COLLATE utf8mb4_bin 是不是被拿掉了？")
	}
	if upper.TransactionID == lower.TransactionID {
		t.Error("兩把不同的鍵應產生兩筆流水")
	}
	if balance, _ := readWallet(t, env.ctx, env.db, player); balance != 800 {
		t.Errorf("兩筆都該扣款：balance = %d, want 800", balance)
	}
}

// TestDebitCrossPlayerIdempotencyCollision 釘住一個**刻意保留的怪行為**。
//
// 冪等鍵跨玩家碰撞時，Java 版回的是原交易值（含**別人的** playerId）而不是拋錯，
// 只留一行 log.error（WalletService.toIdempotentResponse 128-134）。
// 這看起來像 bug，但改掉就是行為漂移：現有呼叫端會從「拿到別人的結果」變成 500。
// 正解是照做 + 大聲留痕，並記進「重寫比原版好」清單當成之後刻意的改進。
func TestDebitCrossPlayerIdempotencyCollision(t *testing.T) {
	env := newTestEnv(t)
	const alice, bob = 42, 43
	const key = "collision-key"
	seedWallet(t, env.ctx, env.db, alice, 1000)
	seedWallet(t, env.ctx, env.db, bob, 1000)

	aliceRes := mustDebit(t, env, alice, 300, key)
	bobRes := mustDebit(t, env, bob, 500, key)

	if !bobRes.Idempotent {
		t.Fatal("同一把鍵被別的玩家用過時應回冪等命中")
	}
	if bobRes.PlayerID != alice || bobRes.TransactionID != aliceRes.TransactionID {
		t.Errorf("應原樣回**贏家**那筆交易：playerID=%d txID=%d, want %d/%d",
			bobRes.PlayerID, bobRes.TransactionID, alice, aliceRes.TransactionID)
	}
	if balance, version := readWallet(t, env.ctx, env.db, bob); balance != 1000 || version != 0 {
		t.Errorf("bob 的錢包不該被動到：balance/version = %d/%d, want 1000/0", balance, version)
	}
	if logs := env.logs.String(); !strings.Contains(logs, "冪等鍵跨玩家碰撞") {
		t.Errorf("跨玩家碰撞必須留下 ERROR 級別的痕跡，否則只會無聲吞掉。日誌內容:\n%s", logs)
	}
}

// ── Outbox ──────────────────────────────────────────────────────────────────

// TestDebitWritesOutboxPayload 驗 outbox 那一列逐欄位長什麼樣。
//
// payload 比對的是**字串**：poller 會原封不動把它搬進 Kafka，
// 跨語言契約測試會直接 diff 它（見 domain.DebitEvent 的註解）。
func TestDebitWritesOutboxPayload(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	seedWallet(t, env.ctx, env.db, player, 1000)

	m, err := domain.NewDebit(player, 300, "", "bet-42-9527", "round-9527")
	if err != nil {
		t.Fatalf("建立扣款意圖失敗: %v", err)
	}
	res, err := env.repo.Debit(env.ctx, m)
	if err != nil {
		t.Fatalf("扣款失敗: %v", err)
	}

	var got struct {
		Topic    string
		KafkaKey string
		Payload  string
		Status   string
		Retry    int
		SentAt   *time.Time
	}
	err = env.db.WithContext(env.ctx).Raw(`
		SELECT topic, kafka_key, payload, status, retry_count, sent_at FROM wallet_outbox`).
		Row().Scan(&got.Topic, &got.KafkaKey, &got.Payload, &got.Status, &got.Retry, &got.SentAt)
	if err != nil {
		t.Fatalf("讀取 outbox 失敗: %v", err)
	}

	if got.Topic != "wallet.debit" {
		t.Errorf("topic = %q, want %q（wallet.debit 是**事件**，地雷 #2）", got.Topic, "wallet.debit")
	}
	// kafka_key 用 playerId：同玩家的事件才會落在同一個 partition 而保持有序。
	if got.KafkaKey != "42" {
		t.Errorf("kafka_key = %q, want %q", got.KafkaKey, "42")
	}
	if got.Status != "PENDING" || got.Retry != 0 || got.SentAt != nil {
		t.Errorf("新寫入的列應是 PENDING/0/NULL，得到 %s/%d/%v", got.Status, got.Retry, got.SentAt)
	}

	want := fmt.Sprintf(`{"transactionId":%d,"playerId":42,"amount":300,`+
		`"balanceBefore":1000,"balanceAfter":700,"subType":"BET",`+
		`"idempotencyKey":"bet-42-9527","referenceId":"round-9527"}`, res.TransactionID)
	if got.Payload != want {
		t.Errorf("payload 與 Java 版的 WalletDebitEvent 不一致\ngot:  %s\nwant: %s", got.Payload, want)
	}
}

// TestDebitEmptyReferenceIDBecomesNull 釘住地雷 #33 在 SQL 這一側。
//
// Go 的 string 零值是 ""，Java 的 String 沒帶是 null。直接寫進去的話
// reference_id 會存成空字串，於是 `WHERE reference_id IS NULL` 的對帳查詢
// 一筆都找不到——**而且兩邊都不報錯**。
func TestDebitEmptyReferenceIDBecomesNull(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	seedWallet(t, env.ctx, env.db, player, 1000)
	mustDebit(t, env, player, 100, "no-ref")

	var ref *string
	if err := env.db.WithContext(env.ctx).
		Raw(`SELECT reference_id FROM wallet_transactions`).Row().Scan(&ref); err != nil {
		t.Fatalf("讀取 reference_id 失敗: %v", err)
	}
	if ref != nil {
		t.Errorf("未帶 referenceID 時應存 NULL，得到 %q", *ref)
	}

	var payload string
	if err := env.db.WithContext(env.ctx).
		Raw(`SELECT payload FROM wallet_outbox`).Row().Scan(&payload); err != nil {
		t.Fatalf("讀取 payload 失敗: %v", err)
	}
	if !strings.Contains(payload, `"referenceId":null`) {
		t.Errorf("事件裡也應是 null 而不是空字串，得到:\n%s", payload)
	}
}

// ── 併發 ────────────────────────────────────────────────────────────────────

// TestDebitConcurrentSamePlayer 是防超扣的實證。
//
// 20 個 goroutine 同時對同一個餘額 1000 的錢包各扣 100：
// 必須**恰好** 10 筆成功、10 筆餘額不足，餘額歸零、流水與事件各 10 筆。
// 少了條件 UPDATE 裡的餘額守衛（或把它拆成先查後扣），這個測試會出現
// 「餘額變負數」或「成功筆數 > 10」——而正式環境不會有人告訴你。
//
// ⚠️ 必須跑在 -race 下才算數（AGENTS.md §4）。
func TestDebitConcurrentSamePlayer(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	const workers = 20
	const each = domain.Amount(100)
	seedWallet(t, env.ctx, env.db, player, 1000)

	var (
		mu        sync.Mutex
		succeeded int
		rejected  int
		other     []error
		wg        sync.WaitGroup
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := domain.NewDebit(player, each, "", fmt.Sprintf("race-%d", i), "")
			if err != nil {
				mu.Lock()
				other = append(other, err)
				mu.Unlock()
				return
			}
			_, err = env.repo.Debit(env.ctx, m)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrInsufficientBalance):
				rejected++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("出現預期外的錯誤: %v", other)
	}
	if succeeded != 10 || rejected != 10 {
		t.Errorf("成功/拒絕 = %d/%d, want 10/10", succeeded, rejected)
	}
	balance, version := readWallet(t, env.ctx, env.db, player)
	if balance != 0 {
		t.Errorf("⭐ 餘額 = %d, want 0——負數代表防超扣破了", balance)
	}
	if version != 10 {
		t.Errorf("version = %d, want 10（每次成功扣款 +1）", version)
	}
	if n := env.count(t, "wallet_transactions"); n != 10 {
		t.Errorf("流水筆數 = %d, want 10", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 10 {
		t.Errorf("outbox 筆數 = %d, want 10", n)
	}
}

// TestDebitConcurrentSameKey 驗「同一把冪等鍵被同時送多次」的不變量。
//
// ⚠️ 這裡刻意**不斷言走了哪一條路徑**，只斷言結果：
// 只能扣一次、只能有一筆流水、只能有一則事件。
//
// ⭐ 實測發現（AGENTS.md 地雷 #34）：在 READ COMMITTED 下，後到者走的是
// **補償路徑**而不是冷路徑。原因是 RC 的快照是 per-statement 的——後到者的
// UPDATE 在被行鎖擋住之前就已經取好快照，那個快照裡贏家的流水還不存在，
// 於是 NOT EXISTS 成立、它照樣扣了款，直到 INSERT 撞 1062 才回沖。
// Java 版在 PostgreSQL 上把這條路徑描述為「極窄競態」，在 MySQL 上它是**常態**。
//
// ⚠️ 所以 version **不是**「餘額變動次數」：每個回沖的 loser 會讓它 +2
// （扣一次、加回來一次）。淨額正確，但拿 version 當計數器會得到錯的答案。
// Java 版的 restoreBalance 同樣會 version+1，差別只在頻率。
func TestDebitConcurrentSameKey(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	const workers = 10
	const key = "same-key-for-all"
	seedWallet(t, env.ctx, env.db, player, 1000)

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := domain.NewDebit(player, 300, "", key, "")
			if err == nil {
				_, err = env.repo.Debit(env.ctx, m)
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("同鍵併發不該有任何失敗（後到者應回冪等命中）: %v", errs)
	}
	balance, version := readWallet(t, env.ctx, env.db, player)
	if balance != 700 {
		t.Errorf("⭐ 只能扣一次：balance = %d, want 700", balance)
	}
	// version 只斷言「有動過」。實際值取決於有多少個 loser 走了補償回沖，
	// 那是排程決定的，釘死它等於把測試綁在一個不保證的時序上。
	if version < 1 {
		t.Errorf("version = %d, want >= 1", version)
	}
	if n := env.count(t, "wallet_transactions"); n != 1 {
		t.Errorf("流水筆數 = %d, want 1", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 1 {
		t.Errorf("outbox 筆數 = %d, want 1", n)
	}
}

// ── MySQL 行為（docs/ADR-002 的立論基礎）────────────────────────────────────

// TestIsolationLevelDecidesWinnerVisibility 釘住地雷 #32，也是
// debitTxOptions 選 READ COMMITTED 的**第二個**理由的唯一證據。
//
// ⭐ MySQL 預設 REPEATABLE READ：交易的快照在**第一次一致性讀**時固定，
// 之後別人提交的列一律看不到。PostgreSQL 預設 READ COMMITTED，每條語句重取快照。
//
// 對 debit 的實際影響：compensate 路徑在點查餘額（一致性讀）之後才撞到 1062，
// 而贏家是在那之後才提交的——在 RR 下回查不到它，會誤判成「衝突了卻找不到贏家」。
//
// 這個測試看起來像在測資料庫而不是測自己的程式。**是刻意的**：
// 它是隔離級別為什麼要明寫的唯一證據，MySQL 哪天改了預設行為，
// 應該是這裡先紅，而不是帳先錯。
func TestIsolationLevelDecidesWinnerVisibility(t *testing.T) {
	const player = 42
	const key = "winner-key"

	tests := []struct {
		name      string
		txOpts    *sql.TxOptions
		wantFound bool
		reason    string
	}{
		{
			name:      "REPEATABLE_READ 看不到後來提交的贏家",
			txOpts:    &sql.TxOptions{Isolation: sql.LevelRepeatableRead},
			wantFound: false,
			reason:    "MySQL 的預設。快照已經固定，這正是不能沿用它的理由",
		},
		{
			name:      "READ_COMMITTED 看得到",
			txOpts:    &sql.TxOptions{Isolation: sql.LevelReadCommitted},
			wantFound: true,
			reason:    "PostgreSQL 的預設，也是 debitTxOptions 選的那個",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			seedWallet(t, env.ctx, env.db, player, 1000)

			reader := env.db.WithContext(env.ctx).Begin(tt.txOpts)
			if reader.Error != nil {
				t.Fatalf("開啟交易失敗: %v", reader.Error)
			}
			t.Cleanup(func() { _ = reader.Rollback() })

			// ① 先做一次一致性讀——對應 debit 熱路徑的「點查扣款後餘額」。
			var balance int64
			if err := reader.Raw(selectBalance, player).Row().Scan(&balance); err != nil {
				t.Fatalf("建立快照失敗: %v", err)
			}

			// ② 另一條連線寫入並提交——對應併發的「贏家」。
			if err := env.db.WithContext(env.ctx).Exec(`
				INSERT INTO wallet_transactions
				       (player_id, type, sub_type, amount, balance_before, balance_after, idempotency_key)
				VALUES (?, 'DEBIT', 'BET', 300, 1000, 700, ?)`, player, key).Error; err != nil {
				t.Fatalf("寫入贏家紀錄失敗: %v", err)
			}

			// ③ 回查——compensate 走的就是這一句。
			_, found, err := findTxByKey(reader, key)
			if err != nil {
				t.Fatalf("回查失敗: %v", err)
			}
			if found != tt.wantFound {
				t.Errorf("回查到贏家 = %v, want %v（%s）", found, tt.wantFound, tt.reason)
			}
		})
	}
}
