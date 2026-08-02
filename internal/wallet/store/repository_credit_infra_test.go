//go:build infra

// credit 的 infra 測試。跑法與共用輔助工具見 repository_infra_test.go 檔頭。
//
// ⚠️ 為什麼另開一檔而不是接在 repository_infra_test.go 後面：那一檔已經是
// debit 的完整證據鏈，而 credit 與 debit **形狀不同**（讀改寫 + 樂觀鎖 vs
// 條件 UPDATE）。分檔讓「哪些斷言屬於哪一種形狀」一眼看得出來。
package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// ── credit 專用的輔助工具 ───────────────────────────────────────────────────

// setFrozen 把凍結金額設進已建好的錢包。
//
// ⚠️ 不動 version：seedWallet 建出來的錢包 version 是 0，測試的斷言都以此為基準。
// 用 UPDATE 而不是擴充 seedWallet 的參數，是為了不動到既有的 debit 測試。
func setFrozen(t *testing.T, e *testEnv, playerID, frozen int64) {
	t.Helper()
	if err := e.db.WithContext(e.ctx).Exec(
		`UPDATE wallets SET frozen_amount = ? WHERE player_id = ?`, frozen, playerID).Error; err != nil {
		t.Fatalf("設定凍結金額失敗: %v", err)
	}
}

func readWalletFull(t *testing.T, e *testEnv, playerID int64) (balance, frozen, version int64) {
	t.Helper()
	if err := e.db.WithContext(e.ctx).Raw(
		`SELECT balance, frozen_amount, version FROM wallets WHERE player_id = ?`, playerID).
		Row().Scan(&balance, &frozen, &version); err != nil {
		t.Fatalf("讀取錢包失敗: %v", err)
	}
	return balance, frozen, version
}

// requireStillBlocked 確認被測交易在 d 之後**仍未結束**——也就是真的卡在對手的行鎖上。
//
// ⚠️ 為什麼不查 information_schema.innodb_trx 之類的鎖視圖：**本機實測查不到**
// 那筆等待中的交易（goroutine dump 明確顯示它卡在 applyCredit 的 Exec 上，
// 而 innodb_trx 只列得出對手那一筆）。用一個比被測程式還不可靠的判準來守門，
// 只會讓測試在編排明明成立的時候紅。
//
// ⚠️ 那不會讓這個測試變成「睡一下賭它有卡住」——**它沒有靜默通過的路徑**：
// 若編排沒成立（credit 在對手提交前就跑完），它會**成功**，於是底下
// `want ErrConcurrentModification` 那條斷言直接紅。這裡的 select 只是把
// 同一個失敗提早、並換成一句看得懂的訊息。
func requireStillBlocked(t *testing.T, done <-chan error, d time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("編排沒有成立：被測交易在 %v 內就結束了（err=%v），它應該卡在對手的行鎖上", d, err)
	case <-time.After(d):
	}
}

// ── 單次呼叫的結局 ──────────────────────────────────────────────────────────

func TestCredit(t *testing.T) {
	const player = 42

	tests := []struct {
		name string
		// seedBalance 為負代表**不建錢包**，用來測 ErrWalletNotFound。
		seedBalance    int64
		seedFrozen     int64
		amount         domain.Amount
		subType        domain.SubType
		unfreeze       domain.Amount
		referenceID    string
		wantErr        error
		wantBefore     domain.Amount
		wantAfter      domain.Amount
		wantFrozen     int64
		wantBalance    int64
		wantVersion    int64
		wantTxRows     int64
		wantOutboxRows int64
		wantLog        string
	}{
		{
			name:           "派彩入帳並落一筆 outbox",
			seedBalance:    1000,
			amount:         300,
			subType:        domain.SubTypeWin,
			referenceID:    "round-9527",
			wantBefore:     1000,
			wantAfter:      1300,
			wantBalance:    1300,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
		},
		{
			// credit **沒有**餘額守衛（是加錢），所以餘額 0 也照入不誤。
			// 這一格是 debit 的「餘額不足」對照組：同樣的前置條件、相反的結局。
			name:           "餘額為零也能入帳（credit 沒有餘額守衛）",
			seedBalance:    0,
			amount:         500,
			subType:        domain.SubTypeBankruptcyAid,
			wantBefore:     0,
			wantAfter:      500,
			wantBalance:    500,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
		},
		{
			name:           "帶解凍：凍結金額同步釋放",
			seedBalance:    1000,
			seedFrozen:     200,
			amount:         300,
			subType:        domain.SubTypeWin,
			unfreeze:       150,
			wantBefore:     1000,
			wantAfter:      1300,
			wantFrozen:     50,
			wantBalance:    1300,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
		},
		{
			// ⭐ 對齊 Java（WalletService.java:196-200）：超額**夾住並留痕**，
			// 不是拒絕請求。改成回 400 會讓現有呼叫端從成功變失敗。
			// ⚠️ 夾到 0 而不是負數——負的 frozen_amount 會讓可用餘額
			// （balance - frozen_amount）憑空變大，那是**可以超扣**的方向。
			name:           "解凍超過凍結金額則夾到 0 並留痕",
			seedBalance:    1000,
			seedFrozen:     100,
			amount:         300,
			subType:        domain.SubTypeRefund,
			unfreeze:       500,
			wantBefore:     1000,
			wantAfter:      1300,
			wantFrozen:     0,
			wantBalance:    1300,
			wantVersion:    1,
			wantTxRows:     1,
			wantOutboxRows: 1,
			wantLog:        "解凍金額超過目前凍結金額",
		},
		{
			// 零副作用：不可以有流水、不可以有 outbox。
			name:           "錢包不存在",
			seedBalance:    -1,
			amount:         100,
			subType:        domain.SubTypeWin,
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
				if tt.seedFrozen > 0 {
					setFrozen(t, env, player, tt.seedFrozen)
				}
			}

			m, err := domain.NewCredit(player, tt.amount, tt.subType, "key-"+tt.name, tt.referenceID, tt.unfreeze)
			if err != nil {
				t.Fatalf("建立入帳意圖失敗: %v", err)
			}
			got, err := env.repo.Credit(env.ctx, m)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("入帳失敗: %v", err)
				}
				if got.Idempotent {
					t.Error("首次入帳不該是冪等命中")
				}
				if got.TransactionID == 0 {
					t.Error("TransactionID 應由 LastInsertId 填回，得到 0")
				}
				if got.BalanceBefore != tt.wantBefore || got.BalanceAfter != tt.wantAfter {
					t.Errorf("before/after = %d/%d, want %d/%d",
						got.BalanceBefore, got.BalanceAfter, tt.wantBefore, tt.wantAfter)
				}
				// ⚠️ 正常路徑的 FrozenAfter 必須是**非 nil 的值**——nil 只保留給
				// 冪等命中（對齊 Java 的 frozenAfter(null)）。回 nil 會讓呼叫端
				// 分不出「這次沒解凍」與「不知道」。
				if got.FrozenAfter == nil {
					t.Fatal("正常入帳的 FrozenAfter 不該是 nil")
				}
				if int64(*got.FrozenAfter) != tt.wantFrozen {
					t.Errorf("FrozenAfter = %d, want %d", *got.FrozenAfter, tt.wantFrozen)
				}
			}

			if tt.seedBalance >= 0 {
				balance, frozen, version := readWalletFull(t, env, player)
				if balance != tt.wantBalance || frozen != tt.wantFrozen || version != tt.wantVersion {
					t.Errorf("錢包 balance/frozen/version = %d/%d/%d, want %d/%d/%d",
						balance, frozen, version, tt.wantBalance, tt.wantFrozen, tt.wantVersion)
				}
			}
			if n := env.count(t, "wallet_transactions"); n != tt.wantTxRows {
				t.Errorf("流水筆數 = %d, want %d", n, tt.wantTxRows)
			}
			if n := env.count(t, "wallet_outbox"); n != tt.wantOutboxRows {
				t.Errorf("outbox 筆數 = %d, want %d", n, tt.wantOutboxRows)
			}
			if tt.wantLog != "" {
				if logs := env.logs.String(); !strings.Contains(logs, tt.wantLog) {
					t.Errorf("預期日誌含 %q，實際內容:\n%s", tt.wantLog, logs)
				}
			}
		})
	}
}

// TestCreditRejectsNonCreditMovement 是 TestDebitRejectsNonDebitMovement 的對稱面。
func TestCreditRejectsNonCreditMovement(t *testing.T) {
	env := newTestEnv(t)
	seedWallet(t, env.ctx, env.db, 42, 1000)

	debit, err := domain.NewDebit(42, 100, "", "wrong-direction-credit", "")
	if err != nil {
		t.Fatalf("建立扣款意圖失敗: %v", err)
	}
	if _, err := env.repo.Credit(env.ctx, debit); !errors.Is(err, domain.ErrUnknownTxType) {
		t.Fatalf("err = %v, want %v", err, domain.ErrUnknownTxType)
	}
	if n := env.count(t, "wallet_transactions"); n != 0 {
		t.Errorf("方向錯的 Movement 不該留下任何流水，得到 %d 筆", n)
	}
}

// ── 冪等 ────────────────────────────────────────────────────────────────────

// TestCreditIsIdempotent 是 credit 的核心保護。
//
// 重送同一把冪等鍵必須：不再加錢、不再寫流水、**不再發一次事件**。
// ⭐ 外加一條 debit 沒有的斷言：冪等命中的 FrozenAfter 必須是 nil。
// Java 明寫 `.frozenAfter(null)` 並註解「不重算凍結；以當初入帳結果為準」
// （WalletService.java:179）。回 0 會被呼叫端讀成「凍結金額是 0」——
// 那是一個**合法但錯誤**的數字，也就是本專案最想避免的形狀（地雷 #33）。
func TestCreditIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	const player, key = 42, "credit-round-9527"
	seedWallet(t, env.ctx, env.db, player, 1000)
	setFrozen(t, env, player, 200)

	m, err := domain.NewCredit(player, 300, domain.SubTypeWin, key, "round-9527", 50)
	if err != nil {
		t.Fatalf("建立入帳意圖失敗: %v", err)
	}
	first, err := env.repo.Credit(env.ctx, m)
	if err != nil {
		t.Fatalf("首次入帳失敗: %v", err)
	}
	if first.Idempotent {
		t.Fatal("第一次入帳不該是冪等命中")
	}

	second, err := env.repo.Credit(env.ctx, m)
	if err != nil {
		t.Fatalf("重送失敗: %v", err)
	}
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
	if second.FrozenAfter != nil {
		t.Errorf("⭐ 冪等命中的 FrozenAfter 必須是 nil（對齊 Java 的 null），得到 %d", *second.FrozenAfter)
	}

	balance, frozen, version := readWalletFull(t, env, player)
	if balance != 1300 || frozen != 150 || version != 1 {
		t.Errorf("重送不該再動錢包：balance/frozen/version = %d/%d/%d, want 1300/150/1",
			balance, frozen, version)
	}
	if n := env.count(t, "wallet_transactions"); n != 1 {
		t.Errorf("流水筆數 = %d, want 1", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 1 {
		t.Errorf("⭐ outbox 筆數 = %d, want 1——重送多發一則事件會讓下游多算一次（地雷 #6）", n)
	}
}

// ── Outbox ──────────────────────────────────────────────────────────────────

// TestCreditWritesOutboxPayload 驗 wallet.credit 那一列逐欄位長什麼樣。
//
// payload 比對的是**字串**：poller 會原封不動把它搬進 Kafka，
// 跨語言契約測試會直接 diff 它。
//
// ⚠️ topic 必須是 `wallet.credit`（**事件**），不是 `wallet.credit.request`（指令）。
// 搞反會讓 wallet 消費到自己發出的事件而無限迴圈（AGENTS.md 地雷 #2）。
func TestCreditWritesOutboxPayload(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	seedWallet(t, env.ctx, env.db, player, 1000)

	m, err := domain.NewCredit(player, 300, domain.SubTypeWin, "credit-round-9527", "round-9527", 0)
	if err != nil {
		t.Fatalf("建立入帳意圖失敗: %v", err)
	}
	res, err := env.repo.Credit(env.ctx, m)
	if err != nil {
		t.Fatalf("入帳失敗: %v", err)
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

	if got.Topic != "wallet.credit" {
		t.Errorf("topic = %q, want %q（wallet.credit 是**事件**，指令走 wallet.credit.request，地雷 #2）",
			got.Topic, "wallet.credit")
	}
	if got.KafkaKey != "42" {
		t.Errorf("kafka_key = %q, want %q", got.KafkaKey, "42")
	}
	if got.Status != "PENDING" || got.Retry != 0 || got.SentAt != nil {
		t.Errorf("新寫入的列應是 PENDING/0/NULL，得到 %s/%d/%v", got.Status, got.Retry, got.SentAt)
	}

	// ⚠️ 與 WalletDebitEvent 逐欄位同名同序——兩個 Java record 的 component
	// 完全相同，所以 Go 那邊共用一個 MovementEvent（見 domain/event.go）。
	want := fmt.Sprintf(`{"transactionId":%d,"playerId":42,"amount":300,`+
		`"balanceBefore":1000,"balanceAfter":1300,"subType":"WIN",`+
		`"idempotencyKey":"credit-round-9527","referenceId":"round-9527"}`, res.TransactionID)
	if got.Payload != want {
		t.Errorf("payload 與 Java 版的 WalletCreditEvent 不一致\ngot:  %s\nwant: %s", got.Payload, want)
	}
}

// ── ⭐ 樂觀鎖：credit 相對 debit 的結構性差異 ──────────────────────────────

// TestCreditOptimisticLockConflict 釘住 `RowsAffected == 0` 那條路徑。
//
// ⭐ 這是 Java 的 JPA `@Version` 幫你做掉、Go 必須自己寫的那一步：
// GORM 執行 `UPDATE ... WHERE version = ?` 動到 0 列時**不會回傳 error**
// （AGENTS.md 地雷 #3）。少了 creditTx 裡那個 switch，一次被別人蓋掉的更新
// 會被當成成功，然後照樣寫流水、發事件——流水說加了錢、餘額說沒有，
// **而且沒有任何錯誤訊息**。
//
// 編排方式：另一條連線先鎖住錢包那一列並推進 version，逼 credit 的往返 3
// 卡在行鎖上；等它真的卡住之後才提交，於是 credit 手上那個 version 必然是舊的。
func TestCreditOptimisticLockConflict(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	seedWallet(t, env.ctx, env.db, player, 1000)

	// ① 對手先卡住那一列（推進 version），暫不提交。
	blocker := env.db.WithContext(env.ctx).Begin()
	if blocker.Error != nil {
		t.Fatalf("開啟對手交易失敗: %v", blocker.Error)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = blocker.Rollback()
		}
	})
	if err := blocker.Exec(
		`UPDATE wallets SET version = version + 1 WHERE player_id = ?`, player).Error; err != nil {
		t.Fatalf("對手更新失敗: %v", err)
	}

	// ② credit 在背景跑：往返 1、2 讀得到（非鎖定讀），往返 3 卡在行鎖上。
	done := make(chan error, 1)
	go func() {
		m, err := domain.NewCredit(player, 300, domain.SubTypeWin, "stale-version", "", 0)
		if err != nil {
			done <- err
			return
		}
		_, err = env.repo.Credit(env.ctx, m)
		done <- err
	}()

	// ③ 確認它真的卡住了，再提交對手。
	requireStillBlocked(t, done, time.Second)
	if err := blocker.Commit().Error; err != nil {
		t.Fatalf("提交對手交易失敗: %v", err)
	}
	committed = true

	// ④ credit 的 UPDATE 重新求值：version 已經是 1，它手上是 0 → 0 列。
	err := <-done
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("err = %v, want %v（對齊 Java 的 ObjectOptimisticLockingFailureException → 409）",
			err, ErrConcurrentModification)
	}

	// ⚠️ 零副作用是重點：樂觀鎖衝突後不可以留下半筆帳。
	balance, _, version := readWalletFull(t, env, player)
	if balance != 1000 || version != 1 {
		t.Errorf("衝突後錢包只該留下對手的異動：balance/version = %d/%d, want 1000/1", balance, version)
	}
	if n := env.count(t, "wallet_transactions"); n != 0 {
		t.Errorf("流水筆數 = %d, want 0", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 0 {
		t.Errorf("outbox 筆數 = %d, want 0", n)
	}
}

// ⭐⭐ TestCreditDupEntryDoesNotDoubleCredit 是這一輪最重要的一條測試（地雷 #35）。
//
// 它證明的事：**照抄 Java 的 Step 5 catch 在 MySQL 上會重複入帳。**
//
// Java（WalletService.java:220-233）撞到唯一鍵衝突時直接回查贏家並正常返回。
// 那在 PostgreSQL 上活得下來，是因為 PG 的約束違反會讓**整筆交易 aborted**，
// catch 裡那句回查自己也會炸，結局是交易回滾、餘額沒多加。
// MySQL 的 1062 只是**語句級**失敗，交易還活著（docs/ADR-002 決策 4）——
// 逐行照抄就會變成「餘額加了、流水沒寫、正常 commit」。
//
// ⚠️ 這裡手工重演 creditTx 的往返 2~4 而不是呼叫 Credit()：要打中的窗口是
// 「往返 1 查不到 → 對手提交 → 往返 2 讀到新 version」，而往返 1 與往返 2
// 都是非鎖定讀，**沒有辦法用行鎖把執行緒卡在它們中間**。
// 手工編排換來的是 100% 決定性，而被測的補償邏輯（compensateCredit）是真的那一份。
func TestCreditDupEntryDoesNotDoubleCredit(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	const key = "credit-dup-key"
	const amount = int64(500)
	const unfrozen = int64(50)

	seedWallet(t, env.ctx, env.db, player, 1000)
	setFrozen(t, env, player, 200)

	// ① 對手（贏家）完整入帳並提交：balance 1000→1500、frozen 200→150、version 0→1。
	if err := env.db.WithContext(env.ctx).Exec(`
		UPDATE wallets SET balance = balance + ?, frozen_amount = frozen_amount - ?, version = version + 1
		 WHERE player_id = ?`, amount, unfrozen, player).Error; err != nil {
		t.Fatalf("對手入帳失敗: %v", err)
	}
	if err := env.db.WithContext(env.ctx).Exec(`
		INSERT INTO wallet_transactions
		       (player_id, type, sub_type, amount, balance_before, balance_after, idempotency_key)
		VALUES (?, 'CREDIT', 'WIN', ?, 1000, 1500, ?)`, player, amount, key).Error; err != nil {
		t.Fatalf("寫入贏家流水失敗: %v", err)
	}
	var winnerID int64
	if err := env.db.WithContext(env.ctx).Raw(
		`SELECT id FROM wallet_transactions WHERE idempotency_key = ?`, key).Row().Scan(&winnerID); err != nil {
		t.Fatalf("讀取贏家 id 失敗: %v", err)
	}

	m, err := domain.NewCredit(player, domain.Amount(amount), domain.SubTypeWin, key, "", domain.Amount(unfrozen))
	if err != nil {
		t.Fatalf("建立入帳意圖失敗: %v", err)
	}

	// ② 重演落敗者的往返 2~4。它的往返 1 發生在對手提交之前（所以查不到），
	//    往返 2 之後才讀到新的 version —— 於是樂觀鎖**過關**，一路走到 1062。
	var got CreditResult
	err = env.db.WithContext(env.ctx).Transaction(func(tx *gorm.DB) error {
		var balanceBefore, frozenBefore, version int64
		if err := tx.Raw(loadWalletForCredit, player).Row().
			Scan(&balanceBefore, &frozenBefore, &version); err != nil {
			return fmt.Errorf("載入錢包失敗: %w", err)
		}
		if balanceBefore != 1500 || version != 1 {
			t.Fatalf("編排前提不成立：讀到 balance/version = %d/%d, want 1500/1", balanceBefore, version)
		}

		res := tx.Exec(applyCredit, balanceBefore+amount, frozenBefore-unfrozen, player, version)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			t.Fatalf("編排前提不成立：樂觀鎖應該過關，卻動到 %d 列", res.RowsAffected)
		}

		// ⭐ 這一刻餘額是 2000。**沒有補償就會這樣提交出去**——這就是那個沒有
		// 錯誤訊息的重複入帳。把下面的 compensateCredit 拿掉，這個測試會紅在
		// 「balance = 2000, want 1500」。
		var midBalance int64
		if err := tx.Raw(`SELECT balance FROM wallets WHERE player_id = ?`, player).
			Row().Scan(&midBalance); err != nil {
			return err
		}
		if midBalance != 2000 {
			t.Fatalf("編排前提不成立：補償前餘額應是 2000（已重複加過），得到 %d", midBalance)
		}

		row := transactionRow{
			PlayerID:       player,
			Type:           string(m.Type),
			SubType:        string(m.SubType),
			Amount:         amount,
			BalanceBefore:  &balanceBefore,
			BalanceAfter:   &midBalance,
			IdempotencyKey: key,
		}
		if err := tx.Create(&row).Error; !isDupEntry(err) {
			t.Fatalf("編排前提不成立：INSERT 應該撞 1062，得到 %v", err)
		}

		got, err = env.repo.compensateCredit(tx, m, unfrozen)
		return err
	}, creditTxOptions)
	if err != nil {
		t.Fatalf("補償路徑不該讓交易失敗: %v", err)
	}

	// ③ 提交之後的不變量。
	balance, frozen, version := readWalletFull(t, env, player)
	if balance != 1500 {
		t.Errorf("⭐ balance = %d, want 1500——多出來的 500 就是重複入帳", balance)
	}
	if frozen != 150 {
		t.Errorf("⭐ frozen = %d, want 150——回沖加回的必須是**實際解凍量**", frozen)
	}
	// version 因為「扣一次、回沖一次」而是 3，不是 1。
	// ⚠️ 與 debit 同一個結論：version **不是**「餘額變動次數」（地雷 #34）。
	if version != 3 {
		t.Errorf("version = %d, want 3（樂觀鎖 +1、補償回沖再 +1）", version)
	}
	if !got.Idempotent || got.TransactionID != winnerID {
		t.Errorf("應回贏家那筆：idempotent=%v txID=%d, want true/%d",
			got.Idempotent, got.TransactionID, winnerID)
	}
	if got.FrozenAfter != nil {
		t.Errorf("補償路徑同樣是冪等命中，FrozenAfter 應為 nil，得到 %d", *got.FrozenAfter)
	}
	if n := env.count(t, "wallet_transactions"); n != 1 {
		t.Errorf("流水筆數 = %d, want 1", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 0 {
		t.Errorf("⭐ outbox 筆數 = %d, want 0——補償路徑沒有真的入帳，不可以發事件", n)
	}
}

// ── 併發 ────────────────────────────────────────────────────────────────────

// TestCreditConcurrentSameKey 驗「同一把冪等鍵被同時送多次」的不變量。
//
// ⚠️ 這裡刻意**不斷言走了哪一條路徑**，只斷言結果：只能加一次錢、
// 一筆流水、一則事件。
//
// ⭐ 與 debit 的同名測試有一個真實差異：debit 同鍵併發**不會有任何失敗**
// （後到者一律走冪等命中或補償），credit 會產生一批 ErrConcurrentModification。
// 那不是 bug，是「讀改寫 + 樂觀鎖」這個形狀的固有行為，Java 版在
// PostgreSQL 上同樣會回 409。呼叫端帶原鍵重試會拿到冪等命中。
func TestCreditConcurrentSameKey(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	const workers = 10
	const key = "same-credit-key-for-all"
	seedWallet(t, env.ctx, env.db, player, 1000)

	var (
		mu        sync.Mutex
		succeeded int
		conflicts int
		other     []error
		wg        sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := domain.NewCredit(player, 300, domain.SubTypeWin, key, "", 0)
			if err == nil {
				_, err = env.repo.Credit(env.ctx, m)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrConcurrentModification):
				conflicts++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("出現預期外的錯誤: %v", other)
	}
	if succeeded+conflicts != workers {
		t.Fatalf("結局總數 = %d, want %d", succeeded+conflicts, workers)
	}
	balance, _, version := readWalletFull(t, env, player)
	if balance != 1300 {
		t.Errorf("⭐ 只能加一次：balance = %d, want 1300", balance)
	}
	if version < 1 {
		t.Errorf("version = %d, want >= 1", version)
	}
	if n := env.count(t, "wallet_transactions"); n != 1 {
		t.Errorf("流水筆數 = %d, want 1", n)
	}
	if n := env.count(t, "wallet_outbox"); n != 1 {
		t.Errorf("outbox 筆數 = %d, want 1", n)
	}
	t.Logf("同鍵併發 %d 筆：成功 %d、樂觀鎖衝突 %d（衝突是預期行為，不是 bug）",
		workers, succeeded, conflicts)
}

// TestCreditConcurrentSamePlayer 驗「同一個玩家、不同冪等鍵」的併發。
//
// ⭐ 與 debit 的同名測試對照著看最有價值：
//   - debit 走條件 UPDATE，20 筆全部會被 DB 序列化執行，**沒有一筆因衝突失敗**
//   - credit 走讀改寫 + 樂觀鎖，**大部分會 409**，呼叫端必須帶原鍵重試
//
// 這就是「Java 版沒對 credit 做 T-090 B2 改寫」的實際代價。把它寫成測試而不是
// 註解，是因為哪天有人「順手」把 credit 也改成條件 UPDATE，這裡會立刻紅。
//
// ⚠️ 不同鍵所以不會有 1062，也就不會走補償回沖 —— version 因此**恰好**等於
// 成功筆數。這與 TestCreditDupEntryDoesNotDoubleCredit 的 version=3 是同一件事的兩面。
func TestCreditConcurrentSamePlayer(t *testing.T) {
	env := newTestEnv(t)
	const player = 42
	const workers = 20
	const each = domain.Amount(100)
	seedWallet(t, env.ctx, env.db, player, 1000)

	var (
		mu        sync.Mutex
		succeeded int
		conflicts int
		other     []error
		wg        sync.WaitGroup
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := domain.NewCredit(player, each, domain.SubTypeWin, fmt.Sprintf("credit-race-%d", i), "", 0)
			if err == nil {
				_, err = env.repo.Credit(env.ctx, m)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrConcurrentModification):
				conflicts++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("出現預期外的錯誤: %v", other)
	}
	if succeeded < 1 {
		t.Fatal("至少要有一筆成功，否則這個測試什麼都沒驗到")
	}
	if succeeded+conflicts != workers {
		t.Fatalf("結局總數 = %d, want %d", succeeded+conflicts, workers)
	}

	balance, _, version := readWalletFull(t, env, player)
	if want := 1000 + int64(succeeded)*int64(each); balance != want {
		t.Errorf("⭐ balance = %d, want %d（成功筆數 × 金額）", balance, want)
	}
	if version != int64(succeeded) {
		t.Errorf("version = %d, want %d（不同鍵不會走補償回沖，所以恰好等於成功筆數）",
			version, succeeded)
	}
	if n := env.count(t, "wallet_transactions"); n != int64(succeeded) {
		t.Errorf("流水筆數 = %d, want %d", n, succeeded)
	}
	if n := env.count(t, "wallet_outbox"); n != int64(succeeded) {
		t.Errorf("outbox 筆數 = %d, want %d", n, succeeded)
	}
	t.Logf("同玩家不同鍵併發 %d 筆：成功 %d、樂觀鎖衝突 %d", workers, succeeded, conflicts)
}
