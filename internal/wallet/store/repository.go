package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// mysqlErrDupEntry 是 MySQL 的重複鍵錯誤碼（ER_DUP_ENTRY）。
//
// ⚠️ 為什麼要認錯誤碼而不是用 INSERT IGNORE：IGNORE 會把**所有**錯誤
// 降級成警告（截斷、NOT NULL、外鍵），於是一筆壞資料靜默變成 no-op——
// 而餘額已經扣掉了。認 1062 只吞重複鍵這一種，其餘照樣往外炸（docs/ADR-002 決策 2）。
//
// ⚠️ 認錯誤碼必須精確（docs/ADR-002 決策 4）：1062 只讓**該語句**失敗，交易仍可用；
// 1213 / 1205 則會讓 InnoDB **回滾整筆交易**，只能重開一筆重做。
// 「MySQL 的錯誤都不影響交易」是錯的。
const (
	mysqlErrDupEntry        = 1062 // ER_DUP_ENTRY：語句級失敗，交易仍可用
	mysqlErrDeadlock        = 1213 // ER_LOCK_DEADLOCK：整筆交易已被回滾
	mysqlErrLockWaitTimeout = 1205 // ER_LOCK_WAIT_TIMEOUT：同樣視為可重試
)

// 扣款的 sentinel error。
//
// 為什麼是 sentinel 而不是自訂型別：呼叫端只需要**分類**（對應到哪個 HTTP 狀態、
// 該不該重試），不需要從 error 裡撈欄位。有需求再升級，現在升級只是多寫程式碼
// （沿用 internal/wallet/domain 的同一個判準）。
var (
	// ErrWalletNotFound 對應 Java 的 WalletNotFoundException（HTTP 404）。
	ErrWalletNotFound = errors.New("錢包不存在")
	// ErrInsufficientBalance 對應 Java 的 InsufficientBalanceException（HTTP 400）。
	ErrInsufficientBalance = errors.New("可用餘額不足")
	// ErrIdempotencyWinnerMissing 對應 Java 的 IllegalStateException：
	// 撞到冪等鍵衝突、卻回查不到那筆贏家紀錄。這代表資料庫狀態自相矛盾，
	// 不是可以吞掉的例外——讓整筆交易回滾（淨額歸零）並讓錯誤浮上來。
	ErrIdempotencyWinnerMissing = errors.New("冪等鍵衝突但找不到贏家紀錄")
	// ErrTransactionBalanceMissing 是本專案相對 Java 版**刻意的分歧**，理由見 debitResultOf。
	ErrTransactionBalanceMissing = errors.New("流水缺少扣款前後餘額")
	// ErrUnexpectedRowsAffected 代表條件 UPDATE 動到了非預期的列數。
	// 主鍵條件下只可能是 0 或 1，出現其他值代表 SQL 被改壞了。
	ErrUnexpectedRowsAffected = errors.New("條件更新影響列數異常")
	// ErrConcurrentModification 是 credit 的樂觀鎖衝突，對應 Java 的
	// ObjectOptimisticLockingFailureException（GlobalExceptionHandler:127 → HTTP 409）。
	//
	// ⚠️ 這是**呼叫端可以重試**的錯誤，但本層刻意不自動重試：Java 版把它原樣拋到
	// HTTP 邊界，內部悄悄重掉會讓呼叫端從此再也看不到 409，那是契約漂移不是修 bug。
	// 重試時必須帶原本那把冪等鍵（地雷 #4），所以重試權在知道鍵怎麼來的那一層。
	ErrConcurrentModification = errors.New("錢包已被其他交易更新（樂觀鎖衝突）")
)

// ── 帳務語句 ────────────────────────────────────────────────────────────────
//
// 這四條刻意寫成套件層級的常數而不是散在函式裡，理由有兩個：
//  1. 帳務 SQL 是這個專案最該被逐字審查的東西，集中在一處才審得動。
//  2. infra 測試直接引用同一份常數。測試若自己複製一份 SQL，
//     就變成「兩份各自為政的定義」——改了實作而測試照樣綠，是最糟的形狀。

// conditionalDebit 是 debit 的熱路徑：一條語句完成「冪等預檢 + 可用餘額守衛 +
// 扣款 + version 遞增」（docs/ADR-002 決策 1，對齊 Java 版 T-090 B2 的往返 1）。
//
// ⚠️ 三個都不可以「順手簡化」的地方：
//   - `balance - frozen_amount >= ?` 不是 `balance >= ?`。凍結金額目前恆為 0，
//     但簡化掉之後，哪天啟用凍結流程就會超扣。
//   - `NOT EXISTS (...)` 是冪等**預檢**，不是冪等保證。真正的保證是流水表的
//     UNIQUE 索引衝突（AGENTS.md 地雷 #3）——先查後寫中間有 race。
//   - `version = version + 1` 不只是為了版本號。MySQL driver 預設回的
//     RowsAffected 是 **changed rows** 而不是 matched rows，恆有異動才能讓
//     「RowsAffected == 0」只有一種解讀（docs/ADR-002 決策 1 的註記）。
const conditionalDebit = `
	UPDATE wallets
	   SET balance = balance - ?, version = version + 1, updated_at = CURRENT_TIMESTAMP(6)
	 WHERE player_id = ?
	   AND balance - frozen_amount >= ?
	   AND NOT EXISTS (SELECT 1 FROM wallet_transactions t WHERE t.idempotency_key = ?)`

// selectBalance 是 MySQL 沒有 RETURNING 的代價（AGENTS.md 地雷 #26）。
// PostgreSQL 可以在上面那條 UPDATE 直接 `RETURNING balance`，MySQL 要多跑這一趟。
// 它是同一筆交易內、主鍵點查、且該列的行鎖已經在手，必定命中 buffer pool。
const selectBalance = `SELECT balance FROM wallets WHERE player_id = ?`

// restoreBalance 是併發同鍵競態的補償回沖（淨額歸零，不多寫一筆流水）。
const restoreBalance = `
	UPDATE wallets
	   SET balance = balance + ?, version = version + 1, updated_at = CURRENT_TIMESTAMP(6)
	 WHERE player_id = ?`

// ── credit 的帳務語句 ───────────────────────────────────────────────────────
//
// credit 與 debit **形狀不同**，不是漏抄：團隊沒有對 credit 做過 T-090 B2 那次
// 「壓成一條語句」的改寫，它到現在仍是 JPA 的讀改寫 + `@Version` 樂觀鎖
// （`WalletService.java:168-260`）。照著 debit 的樣子把 credit 也壓成條件 UPDATE
// 是「品味」而不是「等價」——那樣做會讓樂觀鎖衝突（409）這個既有的對外行為消失。

// loadWalletForCredit 是 credit 的往返 2，對齊 Java 的 `walletRepository.findById`。
//
// ⚠️ 刻意是**非鎖定讀**。改成 `FOR UPDATE` 會變成悲觀鎖——那樣也是對的，
// 但樂觀鎖衝突從此不會發生，`ErrConcurrentModification` 變成死碼，409 消失。
// credit 的併發保護在往返 3 的 `WHERE version = ?`，不在這裡。
const loadWalletForCredit = `SELECT balance, frozen_amount, version FROM wallets WHERE player_id = ?`

// applyCredit 是 credit 的樂觀鎖存檔（往返 3），對齊 JPA `@Version` 產生的 UPDATE。
//
// ⚠️ 三個都不可以「順手簡化」的地方：
//   - `balance = ?` 是**絕對值**，不是 `balance = balance + ?`。改成增量看起來
//     更原子、更安全，但那樣 `WHERE version = ?` 就變成裝飾品，樂觀鎖衝突
//     再也不會發生。要等價的是整組語義，不是單一語句的原子性。
//   - `WHERE version = ?` 是樂觀鎖本體。少了它就是無聲覆蓋別人的更新
//     （lost update）——沒有錯誤訊息，只有一個對不起來的餘額。
//   - `version = version + 1` 讓「有匹配」必然「有異動」，於是
//     RowsAffected == 0 只有一種解讀（docs/ADR-002 決策 1 的註記）。
const applyCredit = `
	UPDATE wallets
	   SET balance = ?, frozen_amount = ?, version = version + 1, updated_at = CURRENT_TIMESTAMP(6)
	 WHERE player_id = ?
	   AND version = ?`

// ⭐ restoreCredit 是 credit 版的補償回沖（AGENTS.md 地雷 #35）。
//
// 為什麼 credit 也需要補償，而 Java 版看起來沒有：Java 的 catch（:220-233）在
// PostgreSQL 上其實回不了正常值——PG 的唯一鍵衝突讓**整筆交易 aborted**，
// catch 裡那句回查自己也會炸，結局是交易回滾（餘額沒多加）。
// MySQL 的 1062 只是語句級失敗，交易還活著（docs/ADR-002 決策 4），
// 逐行照抄就會變成「餘額加了、流水沒寫、正常 commit」——**重複入帳，零錯誤訊息**。
//
// ⚠️ 第二個參數是**實際解凍量**，不是請求帶進來的 `unfreezeAmount`。
// Java 的 `max(0, frozen - unfreeze)` 會夾住超額的請求（:200），兩者可能不同；
// 加回請求值會讓 frozen_amount 憑空長大 → 可用餘額變小 → **假的餘額不足**。
// schema 的 CHECK 只擋負數，擋不住這個方向。
const restoreCredit = `
	UPDATE wallets
	   SET balance = balance - ?, frozen_amount = frozen_amount + ?, version = version + 1, updated_at = CURRENT_TIMESTAMP(6)
	 WHERE player_id = ?`

// ⭐ debitTxOptions：扣款交易跑在 READ COMMITTED，而不是 MySQL 預設的
// REPEATABLE READ。這是本檔最重要的一行，理由有兩個，**兩個都是實測出來的**
// （AGENTS.md 地雷 #32、#34，證據見 repository_infra_test.go）：
//
//  1. **RR 會死鎖**。條件 UPDATE 裡的 `NOT EXISTS` 子查詢在鍵不存在時，會對
//     冪等鍵索引的 supremum 下 **S 型 gap lock**；另一個已持有 wallets 行鎖的
//     交易要 INSERT 同一個 gap，需要 insert intention lock —— 兩者互斥，
//     於是「同一個玩家的併發下注」穩定死鎖（實測 20 筆有 19 筆 1213）。
//     RC 不對搜尋下 gap lock，循環等待的環就斷了。
//  2. **RR 的快照會讓補償路徑回查不到贏家**。交易的快照在第一次一致性讀
//     （點查餘額）就固定，之後對手才提交 → 普通 SELECT 看不到它。
//
// ⚠️ 這不是效能調校，是**等價**：Java 版跑在 PostgreSQL 上，而 PG 的預設就是
// READ COMMITTED。沿用 MySQL 的預設等於憑空引入兩個原版不存在的失敗模式。
// ⚠️ 只設在這一筆交易上，不改全域也不改連線預設——別的地方要不要 RC
// 是別的地方自己的決定，偷偷改掉全域預設會讓後來的人完全看不出哪裡變了。
var debitTxOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// ⭐ creditTxOptions：入帳交易同樣跑在 READ COMMITTED，但**理由只有一半**。
//
// 與 debit 的差異值得記住，因為它示範了「同一個結論可以有不同的推導」：
//
//   - 地雷 #34（gap lock 死鎖）**不適用**。那個死鎖來自條件 UPDATE 裡的
//     `NOT EXISTS` 子查詢，credit 沒有它——往返 1 的冪等檢查是一次普通的
//     一致性讀，在 RR 之下不上任何鎖，自然也不會下 gap lock。
//   - 地雷 #32（RR 快照）**適用，而且換了個入口**。往返 1 就是這筆交易的
//     第一次一致性讀，快照在那一刻固定；往返 2 讀到的 version 因此可能是舊的，
//     而往返 3 的 UPDATE 是鎖定讀、看的是最新值 → `WHERE version = ?` 恆不成立
//     → 憑空多出一批 Java 版不會有的 409。
//     連帶地，1062 之後的回查也看不到贏家（與 debit 同一個機制）。
//
// PostgreSQL 的預設就是 READ COMMITTED，所以 Java 版兩件事都沒遇過。
// ⚠️ 與 debitTxOptions 值相同但**刻意分成兩個變數**：隔離級別是 per-transaction
// 的決定（docs/ADR-002 決策 6），合併成一個共用變數會讓「改一個等於改兩個」，
// 而它們的理由並不相同。
var creditTxOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// maxDeadlockRetries 是縱深防禦。
//
// 換成 RC 之後 debit 的死鎖已經消失（測試釘住），但「死鎖不可能發生」在
// MySQL 上永遠不是能保證的事——鎖順序會隨資料分布與執行計畫改變。
// ⚠️ 重試安全的前提是**冪等鍵不變**（AGENTS.md 地雷 #4）：Movement 是傳值的
// 唯讀資料，重試用的是同一把鍵，所以重做一次不會重複入帳。
// 換鍵重試就是**重複扣款**，這條界線不可以模糊。
const maxDeadlockRetries = 3

// Repository 是 wallet 的 MySQL 存取層。
//
// ⚠️ 這裡刻意**不定義 interface**。Go 的慣例是在**消費端**定義小介面
// （CLAUDE.md §2），所以「wallet service 需要什麼」由 service 那一層自己宣告，
// 不是由這裡先發明一個 WalletRepository 介面再回頭實作。
type Repository struct {
	db     *gorm.DB
	logger *slog.Logger
}

// NewRepository 組裝存取層。
//
// ⚠️ db 應該是**已經掛好 SQL logger** 的那一個（main.go 用 db.Debug() 或自訂
// logger）。帳務關鍵路徑要看得見 SQL 是藍圖 §3.2 的硬性要求——GORM 做得到
// 冪等與樂觀鎖，問題在它預設把產生的 SQL 藏起來，而那正是最容易出錯的地方。
func NewRepository(db *gorm.DB, logger *slog.Logger) *Repository {
	if logger == nil {
		logger = slog.Default()
	}
	return &Repository{db: db, logger: logger}
}

// DebitResult 是一次扣款的結果，對齊 Java 的 DebitResponse。
//
// Idempotent 為 true 代表**這次呼叫沒有真的扣款**，回的是原交易的值——
// 包含 PlayerID 與 Amount 也都是原交易的（Java 的 toIdempotentResponse 就是
// 這樣：`tx.getPlayerId()` / `tx.getAmount()`，不是請求帶進來的值）。
// 冪等鍵跨玩家碰撞時這兩個值會與請求不同，那是刻意保留的診斷訊號。
type DebitResult struct {
	TransactionID int64
	PlayerID      int64
	Amount        domain.Amount
	BalanceBefore domain.Amount
	BalanceAfter  domain.Amount
	Idempotent    bool
}

// Debit 執行一次扣款，並在**同一筆交易**內把 wallet.debit 事件寫進 outbox。
//
// 流程（docs/ADR-002，對齊 Java WalletService.debit 66-126）：
//
//	往返 1  條件 UPDATE：冪等預檢 + 餘額守衛 + 扣款 + version+1
//	        └ 0 列 = 冷路徑：依序區分「冪等命中 → 錢包不存在 → 餘額不足」，皆零副作用
//	往返 2  點查扣款後餘額（MySQL 沒有 RETURNING）
//	往返 3  INSERT 流水
//	        └ 1062 = 併發同鍵競態 → 同交易內補償回沖 + 回查贏家，**不 rollback**
//	往返 4  INSERT outbox（同一交易，地雷 #5）
//
// ⚠️ outbox 與帳務必須在同一筆交易裡。改成「交易外再送 Kafka」的話，
// 「DB 成功、送 Kafka 失敗」會讓事件永遠遺失，而餘額已經變了。
//
// ⚠️ 交易跑在 READ COMMITTED（debitTxOptions），不是 MySQL 的預設 —— 少了它，
// 「兩個人同時下注」會穩定死鎖。理由見 debitTxOptions 與 docs/ADR-002 決策 6。
// ⚠️ Java 版把 1062 那條路徑稱為「極窄競態」，那是 PostgreSQL 的情況；
// 在 MySQL 的 RC 之下它是**常態**（AGENTS.md 地雷 #34）。
func (r *Repository) Debit(ctx context.Context, m domain.Movement) (DebitResult, error) {
	// Movement 只能由 domain.NewDebit / NewCredit 產出，但零值 Movement 傳得進來。
	// 擋在這裡的成本是一個 if，漏掉的成本是一筆 type='CREDIT' 的扣款流水。
	if m.Type != domain.TxTypeDebit {
		return DebitResult{}, fmt.Errorf("%w: Debit 只接受 DEBIT，得到 %q", domain.ErrUnknownTxType, m.Type)
	}

	return withRetry(ctx, r, debitTxOptions, m, r.debitTx)
}

// withRetry 把「開一筆交易跑 fn，遇到可重試的鎖衝突就重開一筆重做」包起來。
//
// 為什麼是泛型自由函式而不是方法：Go 的**方法不能有型別參數**，
// 而 debit 與 credit 的回傳型別不同。這是 Java 泛型的類比破功處之一——
// Java 可以寫 `<T> T withRetry(...)` 當成實例方法，Go 不行。
//
// ⚠️ 重試安全的前提是**冪等鍵不變**（AGENTS.md 地雷 #4）：Movement 是傳值的
// 唯讀資料，重做用的是同一把鍵，所以不會重複入帳／扣款。換鍵重試就是重複記帳。
// ⚠️ 只重試 1213 / 1205（見 isRetryable）。1062 重試一萬次還是 1062。
func withRetry[T any](
	ctx context.Context,
	r *Repository,
	opts *sql.TxOptions,
	m domain.Movement,
	fn func(tx *gorm.DB, m domain.Movement) (T, error),
) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= maxDeadlockRetries; attempt++ {
		if attempt > 0 {
			// 退避讓對手先跑完。加 jitter 是因為死鎖的雙方會**同時**被喚醒，
			// 固定間隔會讓它們原地再撞一次。
			delay := time.Duration(attempt)*2*time.Millisecond + rand.N(3*time.Millisecond)
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(delay):
			}
		}

		var result T
		err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var err error
			result, err = fn(tx, m)
			return err
		}, opts)
		switch {
		case err == nil:
			return result, nil
		case !isRetryable(err):
			return zero, err
		}
		lastErr = err
		// 用 Warn 而不是吞掉：死鎖重試成功時外部完全無感，
		// 但「重試率突然升高」是鎖競爭惡化的早期訊號，要看得見。
		r.logger.Warn("帳務交易遇到可重試的鎖衝突，重試",
			"attempt", attempt+1,
			"type", m.Type,
			"playerID", m.PlayerID,
			"idempotencyKey", m.IdempotencyKey,
			"err", err,
		)
	}
	return zero, fmt.Errorf("帳務交易重試 %d 次仍失敗（type=%s, key=%q）: %w",
		maxDeadlockRetries, m.Type, m.IdempotencyKey, lastErr)
}

func (r *Repository) debitTx(tx *gorm.DB, m domain.Movement) (DebitResult, error) {
	amount := int64(m.Amount)

	// ── 往返 1：條件扣款 ────────────────────────────────────────────────
	res := tx.Exec(conditionalDebit, amount, m.PlayerID, amount, m.IdempotencyKey)
	if res.Error != nil {
		return DebitResult{}, fmt.Errorf("條件扣款失敗: %w", res.Error)
	}
	switch res.RowsAffected {
	case 0:
		return r.coldPath(tx, m)
	case 1:
		// 熱路徑，往下走。
	default:
		// 主鍵條件下不可能，出現代表 WHERE 被改壞了——寧可整筆交易死掉。
		return DebitResult{}, fmt.Errorf("%w: 扣款影響了 %d 列", ErrUnexpectedRowsAffected, res.RowsAffected)
	}

	// ── 往返 2：點查扣款後餘額（沒有 RETURNING 的代價）────────────────
	var balanceAfter int64
	if err := tx.Raw(selectBalance, m.PlayerID).Row().Scan(&balanceAfter); err != nil {
		// 剛剛才更新成功的列不可能消失。真的發生就是資料庫狀態異常，不要吞。
		return DebitResult{}, fmt.Errorf("讀取扣款後餘額失敗（playerID=%d）: %w", m.PlayerID, err)
	}
	balanceBefore := balanceAfter + amount

	// ── 往返 3：寫流水。UNIQUE 索引衝突才是冪等的真正保證 ──────────────
	row := transactionRow{
		PlayerID:       m.PlayerID,
		Type:           string(m.Type),
		SubType:        string(m.SubType),
		Amount:         amount,
		BalanceBefore:  &balanceBefore,
		BalanceAfter:   &balanceAfter,
		IdempotencyKey: m.IdempotencyKey,
		ReferenceID:    domain.OptionalString(m.ReferenceID),
	}
	// GORM 的 Create 會用 LastInsertId() 把主鍵填回 row.ID——那正是
	// docs/ADR-002 決策 3 說的「RETURNING id 的 MySQL 等價物」。
	// ⚠️ 只在單列插入成立：批次插入時 LastInsertId() 回的是**第一筆**（地雷 #26）。
	err := tx.Create(&row).Error
	switch {
	case isDupEntry(err):
		// 極窄競態：兩個請求同時通過往返 1 的 NOT EXISTS 預檢。
		// InnoDB 的重複鍵只是語句級失敗，交易仍可用（docs/ADR-002 決策 4），
		// 所以可以就地補償而不必像 PostgreSQL 那樣先用 ON CONFLICT 迴避。
		return r.compensate(tx, m)
	case err != nil:
		return DebitResult{}, fmt.Errorf("寫入帳務流水失敗: %w", err)
	}

	// ── 往返 4：事件進 outbox（同一交易）──────────────────────────────
	event := domain.DebitEvent{
		TransactionID:  row.ID,
		PlayerID:       m.PlayerID,
		Amount:         m.Amount,
		BalanceBefore:  domain.Amount(balanceBefore),
		BalanceAfter:   domain.Amount(balanceAfter),
		SubType:        m.SubType,
		IdempotencyKey: m.IdempotencyKey,
		ReferenceID:    domain.OptionalString(m.ReferenceID),
	}
	if err := appendOutbox(tx, domain.TopicWalletDebit, m.PlayerID, event); err != nil {
		return DebitResult{}, err
	}

	return DebitResult{
		TransactionID: row.ID,
		PlayerID:      m.PlayerID,
		Amount:        m.Amount,
		BalanceBefore: domain.Amount(balanceBefore),
		BalanceAfter:  domain.Amount(balanceAfter),
		Idempotent:    false,
	}, nil
}

// coldPath 處理條件 UPDATE 動到 0 列的三種情況。
//
// 順序不可調換，對齊 Java（WalletService.debit 74-84）：
// 冪等命中 → 錢包不存在 → 餘額不足。把「餘額不足」排到冪等命中前面的話，
// 重送一筆餘額已經花光的舊請求會拿到 400 而不是原本的成功結果。
//
// ⚠️ 這三種都是**零副作用**——條件 UPDATE 沒動到任何列，這裡也只有讀。
func (r *Repository) coldPath(tx *gorm.DB, m domain.Movement) (DebitResult, error) {
	existing, found, err := findTxByKey(tx, m.IdempotencyKey)
	if err != nil {
		return DebitResult{}, err
	}
	if found {
		return r.debitResultOf(existing, m)
	}

	var one int
	err = tx.Raw(`SELECT 1 FROM wallets WHERE player_id = ?`, m.PlayerID).Row().Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DebitResult{}, fmt.Errorf("%w: playerID=%d", ErrWalletNotFound, m.PlayerID)
	case err != nil:
		return DebitResult{}, fmt.Errorf("查詢錢包是否存在失敗（playerID=%d）: %w", m.PlayerID, err)
	}
	return DebitResult{}, fmt.Errorf("%w: playerID=%d 需要 %d", ErrInsufficientBalance, m.PlayerID, m.Amount)
}

// compensate 處理併發同鍵競態：對手先寫進了同一把冪等鍵。
//
// ⚠️ **不回傳錯誤讓交易 rollback**，而是就地把錢加回去然後回贏家的結果——
// 對齊 Java（WalletService.debit 93-101）。理由是 debit 可能被包在外層交易裡
// （例如商城兌換），丟例外會把外層整筆拖垮。
func (r *Repository) compensate(tx *gorm.DB, m domain.Movement) (DebitResult, error) {
	res := tx.Exec(restoreBalance, int64(m.Amount), m.PlayerID)
	if res.Error != nil {
		return DebitResult{}, fmt.Errorf("補償回沖失敗（playerID=%d）: %w", m.PlayerID, res.Error)
	}
	if res.RowsAffected != 1 {
		// 回沖沒生效代表錢真的少了一筆。這裡回錯誤讓整筆交易回滾，
		// 淨額同樣歸零，但錯誤會浮上來被看見——比默默少一筆帳好。
		return DebitResult{}, fmt.Errorf("%w: 補償回沖影響了 %d 列", ErrUnexpectedRowsAffected, res.RowsAffected)
	}

	// ⚠️ 這一句能讀到贏家，靠的是交易跑在 READ COMMITTED（見 debitTxOptions）。
	// 在 MySQL 預設的 REPEATABLE READ 下，快照在上面點查餘額那一刻就固定了，
	// 而贏家是在那之後才提交的——普通 SELECT **看不到它**，這裡就會誤判成
	// 「衝突了卻找不到贏家」。地雷 #32，有測試釘住兩個隔離級別的差異。
	winner, found, err := findTxByKey(tx, m.IdempotencyKey)
	if err != nil {
		return DebitResult{}, err
	}
	if !found {
		return DebitResult{}, fmt.Errorf("%w: key=%q", ErrIdempotencyWinnerMissing, m.IdempotencyKey)
	}
	return r.debitResultOf(winner, m)
}

// debitResultOf 把一筆既有流水翻譯成「冪等命中」的回應。
func (r *Repository) debitResultOf(row transactionRow, m domain.Movement) (DebitResult, error) {
	hit, err := r.idempotentHitOf(row, m)
	if err != nil {
		return DebitResult{}, err
	}
	return DebitResult{
		TransactionID: hit.TransactionID,
		PlayerID:      hit.PlayerID,
		Amount:        hit.Amount,
		BalanceBefore: hit.BalanceBefore,
		BalanceAfter:  hit.BalanceAfter,
		Idempotent:    true,
	}, nil
}

// idempotentHit 是 debit 與 credit 的冪等命中共同欄位。
//
// 兩邊的差別只有 `CreditResult.FrozenAfter`，所以共用這一份而不是各寫一次：
// 底下那兩段（跨玩家碰撞的留痕、餘額欄位為 NULL 的判斷）都是**很難重新推導出來**
// 的判斷，複製兩份必定漂移，而漂移的症狀是「credit 的碰撞沒人看得到」。
type idempotentHit struct {
	TransactionID int64
	PlayerID      int64
	Amount        domain.Amount
	BalanceBefore domain.Amount
	BalanceAfter  domain.Amount
}

func (r *Repository) idempotentHitOf(row transactionRow, m domain.Movement) (idempotentHit, error) {
	if row.PlayerID != m.PlayerID {
		// 冪等鍵跨玩家碰撞 = 呼叫端的鍵命名有 bug（正規鍵都以 playerID 當 namespace）。
		// ⚠️ 沿用 Java 語意：**回原交易值而不是拋錯**，但大聲留痕讓它可被監控發現
		// （WalletService.toIdempotentResponse 128-134）。改成拋錯會讓現有呼叫端
		// 從「拿到別人的結果」變成「500」，那是行為漂移，不是修 bug。
		r.logger.Error("冪等鍵跨玩家碰撞",
			"idempotencyKey", row.IdempotencyKey,
			"requestPlayerID", m.PlayerID,
			"txPlayerID", row.PlayerID,
		)
	}
	if row.BalanceBefore == nil || row.BalanceAfter == nil {
		// ⚠️ 這裡與 Java 版**刻意不同**：Java 的 DebitResponse 欄位是 Long，
		// 直接把 null 回出去。Go 這邊有兩個選擇——回 0 或回錯誤。
		// 回 0 是靜默的錯誤數字（本專案最想避免的形狀）；把整條鏈路改成
		// *Amount 則是為了一個「所有 Java 寫入路徑都會設值、只可能來自手動改資料」
		// 的狀態，永久付出指標成本。所以選擇明確報錯。
		return idempotentHit{}, fmt.Errorf("%w: txID=%d", ErrTransactionBalanceMissing, row.ID)
	}
	return idempotentHit{
		TransactionID: row.ID,
		PlayerID:      row.PlayerID,
		Amount:        domain.Amount(row.Amount),
		BalanceBefore: domain.Amount(*row.BalanceBefore),
		BalanceAfter:  domain.Amount(*row.BalanceAfter),
	}, nil
}

// ── credit ─────────────────────────────────────────────────────────────────

// CreditResult 是一次入帳的結果，對齊 Java 的 CreditResponse。
//
// ⚠️ FrozenAfter 是 *Amount 而不是 Amount（AGENTS.md 地雷 #33）：
// Java 在**冪等命中**時明確回 `null`，註解寫「不重算凍結；以當初入帳結果為準」
// （WalletService.java:179、:229）。用 Amount 的話那個 null 會靜靜變成 0，
// 而 0 是一個**合法的凍結金額**——呼叫端分不出「沒有這個資訊」與「凍結金額是 0」。
// 契約測試會直接看到這個差異。
type CreditResult struct {
	TransactionID int64
	PlayerID      int64
	Amount        domain.Amount
	BalanceBefore domain.Amount
	BalanceAfter  domain.Amount
	FrozenAfter   *domain.Amount
	Idempotent    bool
}

// Credit 執行一次入帳，並在**同一筆交易**內把 wallet.credit 事件寫進 outbox。
//
// 流程（對齊 Java WalletService.credit 168-260）：
//
//	往返 1  以冪等鍵查流水 → 命中就零副作用回原結果（FrozenAfter = nil）
//	往返 2  載入錢包（balance / frozen_amount / version）
//	        └ 不存在 → ErrWalletNotFound。credit **不需要餘額守衛**，因為是加錢
//	往返 3  樂觀鎖存檔：UPDATE ... WHERE version = ?
//	        └ ⭐ RowsAffected == 0 = 併發衝突 → ErrConcurrentModification（409）
//	往返 4  INSERT 流水
//	        └ 1062 → 同交易內補償回沖 + 回查贏家（地雷 #35），**不 rollback**
//	往返 5  INSERT outbox（同一交易，地雷 #5）
//
// ⚠️ 比 debit 多兩趟往返。這不是寫壞了，是「Java 版沒對 credit 做 B2 改寫」的
// 直接後果（docs/notes/Java版-wallet-帳務口徑.md §4）。壓測報告裡 credit 就是
// 比較慢的那一邊，**不要為了數字好看而偷偷改成條件 UPDATE**。
//
// ⚠️ 往返 1 的「先查再寫」不是冪等的全部保護，只是快路徑；真正的保護是往返 4 的
// UNIQUE 衝突（地雷 #3）。**兩層都要留**：砍掉往返 1，正常重送會走進昂貴路徑；
// 砍掉往返 4 的處理，那就是一個 race。
func (r *Repository) Credit(ctx context.Context, m domain.Movement) (CreditResult, error) {
	// 與 Debit 對稱的型別守衛：Movement 是可以手動組出來的 struct，
	// 擋在這裡的成本是一個 if，漏掉的成本是一筆 type='DEBIT' 的入帳流水。
	if m.Type != domain.TxTypeCredit {
		return CreditResult{}, fmt.Errorf("%w: Credit 只接受 CREDIT，得到 %q", domain.ErrUnknownTxType, m.Type)
	}
	return withRetry(ctx, r, creditTxOptions, m, r.creditTx)
}

func (r *Repository) creditTx(tx *gorm.DB, m domain.Movement) (CreditResult, error) {
	amount := int64(m.Amount)

	// ── 往返 1：冪等快路徑 ──────────────────────────────────────────────
	existing, found, err := findTxByKey(tx, m.IdempotencyKey)
	if err != nil {
		return CreditResult{}, err
	}
	if found {
		return r.creditResultOf(existing, m)
	}

	// ── 往返 2：載入錢包 ────────────────────────────────────────────────
	var balanceBefore, frozenBefore, version int64
	err = tx.Raw(loadWalletForCredit, m.PlayerID).Row().Scan(&balanceBefore, &frozenBefore, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CreditResult{}, fmt.Errorf("%w: playerID=%d", ErrWalletNotFound, m.PlayerID)
	case err != nil:
		return CreditResult{}, fmt.Errorf("載入錢包失敗（playerID=%d）: %w", m.PlayerID, err)
	}
	balanceAfter := balanceBefore + amount

	// 選填解凍，夾在 [0, frozenBefore]。
	// ⚠️ 超額**不拒絕請求**，只夾住並留痕——對齊 Java（WalletService.java:196-200）。
	// 改成回 400 會讓現有呼叫端從「成功入帳」變成失敗，那是行為漂移。
	unfrozen := int64(m.UnfreezeAmount)
	if unfrozen > frozenBefore {
		r.logger.Warn("解凍金額超過目前凍結金額，夾到 0",
			"playerID", m.PlayerID,
			"requested", unfrozen,
			"frozenAmount", frozenBefore,
			"idempotencyKey", m.IdempotencyKey,
		)
		unfrozen = frozenBefore
	}
	frozenAfter := frozenBefore - unfrozen

	// ── 往返 3：樂觀鎖存檔 ──────────────────────────────────────────────
	res := tx.Exec(applyCredit, balanceAfter, frozenAfter, m.PlayerID, version)
	if res.Error != nil {
		return CreditResult{}, fmt.Errorf("入帳存檔失敗: %w", res.Error)
	}
	switch res.RowsAffected {
	case 0:
		// ⭐ 這一行就是 Java 的 @Version 幫你做掉、Go 必須自己寫的那一步。
		// GORM **不會**為 0 列回傳 error（AGENTS.md 地雷 #3）——少了這個
		// switch，一次被別人蓋掉的更新會靜靜地被當成成功，然後往下寫流水、
		// 發事件。結果是流水說加了錢、餘額說沒有，**而且沒有錯誤訊息**。
		return CreditResult{}, fmt.Errorf("%w: playerID=%d version=%d", ErrConcurrentModification, m.PlayerID, version)
	case 1:
		// 存檔成功，往下走。
	default:
		// 主鍵 + version 條件下不可能，出現代表 WHERE 被改壞了。
		return CreditResult{}, fmt.Errorf("%w: 入帳影響了 %d 列", ErrUnexpectedRowsAffected, res.RowsAffected)
	}

	// ── 往返 4：寫流水。UNIQUE 索引衝突才是冪等的真正保證 ──────────────
	row := transactionRow{
		PlayerID:       m.PlayerID,
		Type:           string(m.Type),
		SubType:        string(m.SubType),
		Amount:         amount,
		BalanceBefore:  &balanceBefore,
		BalanceAfter:   &balanceAfter,
		IdempotencyKey: m.IdempotencyKey,
		ReferenceID:    domain.OptionalString(m.ReferenceID),
	}
	err = tx.Create(&row).Error
	switch {
	case isDupEntry(err):
		// ⭐ 地雷 #35：餘額**已經加上去了**，這裡不補償就是重複入帳。
		return r.compensateCredit(tx, m, unfrozen)
	case err != nil:
		return CreditResult{}, fmt.Errorf("寫入帳務流水失敗: %w", err)
	}

	// ── 往返 5：事件進 outbox（同一交易）──────────────────────────────
	event := domain.CreditEvent{
		TransactionID:  row.ID,
		PlayerID:       m.PlayerID,
		Amount:         m.Amount,
		BalanceBefore:  domain.Amount(balanceBefore),
		BalanceAfter:   domain.Amount(balanceAfter),
		SubType:        m.SubType,
		IdempotencyKey: m.IdempotencyKey,
		ReferenceID:    domain.OptionalString(m.ReferenceID),
	}
	if err := appendOutbox(tx, domain.TopicWalletCredit, m.PlayerID, event); err != nil {
		return CreditResult{}, err
	}

	frozen := domain.Amount(frozenAfter)
	return CreditResult{
		TransactionID: row.ID,
		PlayerID:      m.PlayerID,
		Amount:        m.Amount,
		BalanceBefore: domain.Amount(balanceBefore),
		BalanceAfter:  domain.Amount(balanceAfter),
		FrozenAfter:   &frozen,
		Idempotent:    false,
	}, nil
}

// ⭐ compensateCredit 處理併發同鍵競態：對手先寫進了同一把冪等鍵，
// 而**這筆交易已經把錢加進去了**（AGENTS.md 地雷 #35）。
//
// 可達的交錯是這一個（RC 之下是真的會發生，不是理論值）：
//
//	T2 往返 1（查不到）→ T1 整筆提交 → T2 往返 2（讀到**新的** version）
//	→ T2 往返 3 樂觀鎖過關 → T2 往返 4 撞 1062
//
// ⚠️ 與 debit 的 compensate 一樣**不回傳錯誤讓交易 rollback**，而是就地回沖
// 再回贏家的結果——理由同 docs/ADR-002：credit 可能被包在外層交易裡
// （商城退款、場次結算），丟錯誤會把外層整筆拖垮。
//
// ⚠️ unfrozen 是**實際解凍量**（已被夾過），不是 m.UnfreezeAmount。
func (r *Repository) compensateCredit(tx *gorm.DB, m domain.Movement, unfrozen int64) (CreditResult, error) {
	res := tx.Exec(restoreCredit, int64(m.Amount), unfrozen, m.PlayerID)
	if res.Error != nil {
		return CreditResult{}, fmt.Errorf("補償回沖失敗（playerID=%d）: %w", m.PlayerID, res.Error)
	}
	if res.RowsAffected != 1 {
		// 回沖沒生效代表錢真的多了一筆。回錯誤讓整筆交易回滾，淨額同樣歸零，
		// 但錯誤會浮上來被看見——比默默多一筆帳好。
		return CreditResult{}, fmt.Errorf("%w: 補償回沖影響了 %d 列", ErrUnexpectedRowsAffected, res.RowsAffected)
	}

	// ⚠️ 這一句能讀到贏家，靠的是交易跑在 READ COMMITTED（見 creditTxOptions）。
	// RR 之下快照在往返 1 就固定了，贏家是之後才提交的——普通 SELECT 看不到它。
	winner, found, err := findTxByKey(tx, m.IdempotencyKey)
	if err != nil {
		return CreditResult{}, err
	}
	if !found {
		return CreditResult{}, fmt.Errorf("%w: key=%q", ErrIdempotencyWinnerMissing, m.IdempotencyKey)
	}
	return r.creditResultOf(winner, m)
}

// creditResultOf 把一筆既有流水翻譯成「冪等命中」的回應。
func (r *Repository) creditResultOf(row transactionRow, m domain.Movement) (CreditResult, error) {
	hit, err := r.idempotentHitOf(row, m)
	if err != nil {
		return CreditResult{}, err
	}
	return CreditResult{
		TransactionID: hit.TransactionID,
		PlayerID:      hit.PlayerID,
		Amount:        hit.Amount,
		BalanceBefore: hit.BalanceBefore,
		BalanceAfter:  hit.BalanceAfter,
		// ⚠️ 明確是 nil，對齊 Java 的 `.frozenAfter(null)`（:179、:229）——
		// 「不重算凍結；以當初入帳結果為準」。回目前的凍結金額看起來更有用，
		// 但那會讓同一支 API 在冪等命中與否時回傳**不同時間點**的凍結金額。
		FrozenAfter: nil,
		Idempotent:  true,
	}, nil
}

// ── 資料列 ──────────────────────────────────────────────────────────────────

// transactionRow 對應 wallet_transactions。
//
// ⚠️ 每個欄位都明寫 column tag，不靠 GORM 的命名推導。帳務表的欄位對應
// 不該是「應該會對吧」——推導規則哪天變了，症狀是某個欄位靜靜地沒被寫入。
//
// BalanceBefore / BalanceAfter / ReferenceID 是指標，因為 schema 裡它們可以是 NULL。
// 用非指標會讓 NULL 靜靜地變成 0 或 ""（見 domain.OptionalString 的註解）。
type transactionRow struct {
	ID             int64   `gorm:"column:id;primaryKey"`
	PlayerID       int64   `gorm:"column:player_id"`
	Type           string  `gorm:"column:type"`
	SubType        string  `gorm:"column:sub_type"`
	Amount         int64   `gorm:"column:amount"`
	BalanceBefore  *int64  `gorm:"column:balance_before"`
	BalanceAfter   *int64  `gorm:"column:balance_after"`
	IdempotencyKey string  `gorm:"column:idempotency_key"`
	ReferenceID    *string `gorm:"column:reference_id"`
}

func (transactionRow) TableName() string { return "wallet_transactions" }

// outboxRow 對應 wallet_outbox。
//
// 刻意不含 status / retry_count / created_at：讓 DB 的 DEFAULT 生效，
// 避免「Go 這邊寫 'PENDING'、schema 那邊 DEFAULT 'PENDING'」兩份定義漂移。
type outboxRow struct {
	ID       int64  `gorm:"column:id;primaryKey"`
	Topic    string `gorm:"column:topic"`
	KafkaKey string `gorm:"column:kafka_key"`
	Payload  string `gorm:"column:payload"`
}

func (outboxRow) TableName() string { return "wallet_outbox" }

// ── 共用小工具 ──────────────────────────────────────────────────────────────

func findTxByKey(tx *gorm.DB, key string) (transactionRow, bool, error) {
	var row transactionRow
	err := tx.Where("idempotency_key = ?", key).Take(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return transactionRow{}, false, nil
	case err != nil:
		return transactionRow{}, false, fmt.Errorf("以冪等鍵查詢流水失敗（key=%q）: %w", key, err)
	}
	return row, true, nil
}

// appendOutbox 把事件寫進 outbox。**必須**與帳務異動在同一筆交易裡（地雷 #5）。
//
// ⚠️ 序列化失敗要讓整筆交易回滾，不可以吞掉：序列化失敗代表事件永遠發不出去，
// 此時讓帳務也一起失敗，比留下一個「錢扣了、事件沒了」的無聲缺口好
// （對齊 Java WalletOutboxService 的同一個判斷）。
func appendOutbox(tx *gorm.DB, topic string, playerID int64, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 outbox payload 失敗（topic=%s）: %w", topic, err)
	}
	row := outboxRow{
		Topic: topic,
		// kafka_key 用 playerID：同一個玩家的事件會落在同一個 partition，
		// 於是下游看到的順序與帳務發生順序一致。換成別的 key 會讓
		// 「先扣款後派彩」在下游變成隨機順序，而 Kafka 不會為此報錯。
		KafkaKey: strconv.FormatInt(playerID, 10),
		Payload:  string(encoded),
	}
	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("寫入 outbox 失敗（topic=%s）: %w", topic, err)
	}
	return nil
}

func isDupEntry(err error) bool {
	return mysqlErrNumber(err) == mysqlErrDupEntry
}

// isRetryable 回答「這個錯誤重開一筆交易重做會不會好」。
//
// ⚠️ 只有死鎖與鎖等待逾時算數。把「重試」放寬到其他錯誤是很危險的：
// 例如 1062 重試一萬次也一樣是 1062，而餘額已經被條件 UPDATE 扣掉了。
func isRetryable(err error) bool {
	switch mysqlErrNumber(err) {
	case mysqlErrDeadlock, mysqlErrLockWaitTimeout:
		return true
	default:
		return false
	}
}

func mysqlErrNumber(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}
