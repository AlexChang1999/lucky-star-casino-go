//go:build infra

// outbox 投遞側的 infra 測試。需要真的跑起來的 MySQL：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	go test -race -tags=infra ./internal/wallet/store/
//
// ⚠️ 這一檔完全不碰 Kafka：投遞交給 PublishFunc，測試傳一個假的進去。
// Kafka 的部分由 internal/wallet/outbox 的測試負責——分開的好處是這裡可以
// 用「這一批我只成功送出第 1 與第 3 筆」這種在真實 broker 上很難重現的情境。
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// ── 測試資料 ────────────────────────────────────────────────────────────────

type outboxTestRow struct {
	ID         int64      `gorm:"column:id;primaryKey"`
	Topic      string     `gorm:"column:topic"`
	KafkaKey   *string    `gorm:"column:kafka_key"`
	Payload    string     `gorm:"column:payload"`
	Status     string     `gorm:"column:status"`
	RetryCount int        `gorm:"column:retry_count"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	SentAt     *time.Time `gorm:"column:sent_at"`
}

func (outboxTestRow) TableName() string { return "wallet_outbox" }

// seedOutbox 塞一列 outbox 並回傳它的 id。
//
// createdAt 明給而不是靠 DEFAULT：投遞順序是 ORDER BY created_at，
// 靠預設值的話同一次測試塞進去的列可能落在同一微秒，順序就不可預期了。
func seedOutbox(t *testing.T, e *testEnv, row outboxTestRow) int64 {
	t.Helper()
	if row.Topic == "" {
		row.Topic = "wallet.debit"
	}
	if row.Payload == "" {
		row.Payload = `{"transactionId":1}`
	}
	if row.Status == "" {
		row.Status = "PENDING"
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if err := e.db.WithContext(e.ctx).Create(&row).Error; err != nil {
		t.Fatalf("塞入 outbox 測試資料失敗: %v", err)
	}
	return row.ID
}

func readOutbox(t *testing.T, e *testEnv) []outboxTestRow {
	t.Helper()
	var rows []outboxTestRow
	if err := e.db.WithContext(e.ctx).Raw(
		`SELECT id, topic, kafka_key, payload, status, retry_count, created_at, sent_at
		   FROM wallet_outbox ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatalf("讀取 outbox 失敗: %v", err)
	}
	return rows
}

// collectIDs 是「送出全部」的 PublishFunc，順便記下拿到的順序。
func collectIDs(got *[]int64) PublishFunc {
	return func(_ context.Context, events []PendingEvent) ([]int64, error) {
		ids := make([]int64, 0, len(events))
		for _, e := range events {
			ids = append(ids, e.ID)
		}
		*got = append(*got, ids...)
		return ids, nil
	}
}

// ── 投遞的三種結局 ──────────────────────────────────────────────────────────

// ⭐ TestPublishPendingPartialFailure 是這一檔最重要的一條。
//
// 一批事件會依 key 打散到多個 partition，其中一個 partition 出問題時就是
// **部分成功**——這不是例外，是正常情況。三件事都要對：
//   - 成功的標 SENT 並寫 sent_at（沒標的話下一輪會重送，下游每次都多吃一份重複）
//   - 失敗的**維持 PENDING** 並累加 retry_count（標成 SENT 就是無聲丟失事件）
//   - 交易**仍然提交**（回滾的話「已經送進 Kafka」這個事實就沒被記錄下來）
func TestPublishPendingPartialFailure(t *testing.T) {
	env := newTestEnv(t)
	base := time.Now().UTC().Add(-time.Minute)
	id1 := seedOutbox(t, env, outboxTestRow{CreatedAt: base})
	id2 := seedOutbox(t, env, outboxTestRow{CreatedAt: base.Add(time.Second)})
	id3 := seedOutbox(t, env, outboxTestRow{CreatedAt: base.Add(2 * time.Second)})

	publishErr := errors.New("partition 2 的 leader 掛了")
	stats, err := env.repo.PublishPending(env.ctx, 10,
		func(_ context.Context, events []PendingEvent) ([]int64, error) {
			if len(events) != 3 {
				t.Errorf("撈到 %d 筆, want 3", len(events))
			}
			return []int64{id1, id3}, publishErr
		})

	if !errors.Is(err, publishErr) {
		t.Errorf("投遞錯誤要原樣往上傳（呼叫端要記錄它），得到: %v", err)
	}
	want := PublishStats{Fetched: 3, Sent: 2, Failed: 1}
	if stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}

	rows := readOutbox(t, env)
	for _, r := range rows {
		switch r.ID {
		case id1, id3:
			if r.Status != "SENT" {
				t.Errorf("id=%d status = %q, want SENT", r.ID, r.Status)
			}
			if r.SentAt == nil {
				t.Errorf("id=%d 標了 SENT 卻沒有 sent_at——清理排程靠它判斷保留期", r.ID)
			}
			if r.RetryCount != 0 {
				t.Errorf("id=%d retry_count = %d, want 0", r.ID, r.RetryCount)
			}
		case id2:
			if r.Status != "PENDING" {
				t.Errorf("id=%d status = %q, want PENDING——投遞失敗的列標成 SENT 就是無聲丟失事件", r.ID, r.Status)
			}
			if r.SentAt != nil {
				t.Errorf("id=%d 沒送出去卻有 sent_at = %v", r.ID, r.SentAt)
			}
			if r.RetryCount != 1 {
				t.Errorf("id=%d retry_count = %d, want 1", r.ID, r.RetryCount)
			}
		}
	}
}

func TestPublishPendingEmptyTable(t *testing.T) {
	env := newTestEnv(t)

	called := false
	stats, err := env.repo.PublishPending(env.ctx, 10,
		func(context.Context, []PendingEvent) ([]int64, error) {
			called = true
			return nil, nil
		})
	if err != nil {
		t.Fatalf("非預期錯誤: %v", err)
	}
	if stats != (PublishStats{}) {
		t.Errorf("stats = %+v, want 零值", stats)
	}
	// 沒事做時不該去打 Kafka：200ms 一輪、大部分時間是空的，
	// 每輪都建連線／送 metadata 請求是純粹的浪費。
	if called {
		t.Error("沒有待送事件時不應呼叫 publish")
	}
}

// TestPublishPendingRejectsForeignIDs 釘住一條防禦：publish 回報的 id
// 若不屬於這一批，整筆交易必須回滾。
//
// ⚠️ 少了這道檢查，一個回錯 id 的 publish 會把**別的**列標成 SENT——
// 那些列從此不會被投遞，而且沒有任何訊息。寧可整批重來。
func TestPublishPendingRejectsForeignIDs(t *testing.T) {
	env := newTestEnv(t)
	id := seedOutbox(t, env, outboxTestRow{})

	_, err := env.repo.PublishPending(env.ctx, 10,
		func(context.Context, []PendingEvent) ([]int64, error) {
			return []int64{id + 999}, nil
		})
	if err == nil {
		t.Fatal("publish 回了不屬於這一批的 id，應該要報錯")
	}
	if rows := readOutbox(t, env); rows[0].Status != "PENDING" || rows[0].RetryCount != 0 {
		t.Errorf("交易應該整筆回滾，卻留下 status=%s retry=%d", rows[0].Status, rows[0].RetryCount)
	}
}

// ── 撈取的順序與批次 ────────────────────────────────────────────────────────

// TestPublishPendingFetchesOldestFirst 釘住「先進先出」與批次上限。
//
// ⚠️ 順序不是美觀問題：partition 內的順序就是送出順序，撈取順序錯了，
// 同一個玩家的「先扣款、後派彩」在下游就會反過來——而 Kafka 不會報錯。
func TestPublishPendingFetchesOldestFirst(t *testing.T) {
	env := newTestEnv(t)
	base := time.Now().UTC().Add(-time.Hour)

	var ids []int64
	for i := range 5 {
		ids = append(ids, seedOutbox(t, env, outboxTestRow{
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}))
	}

	var got []int64
	stats, err := env.repo.PublishPending(env.ctx, 3, collectIDs(&got))
	if err != nil {
		t.Fatalf("非預期錯誤: %v", err)
	}
	if stats.Fetched != 3 {
		t.Errorf("撈了 %d 筆, want 3（batchSize 沒有生效）", stats.Fetched)
	}
	for i, want := range ids[:3] {
		if got[i] != want {
			t.Errorf("第 %d 筆 id = %d, want %d（應該由舊到新）", i, got[i], want)
		}
	}

	// 第二輪要接著撈剩下的兩筆，不會重撈已經 SENT 的。
	got = nil
	if _, err := env.repo.PublishPending(env.ctx, 3, collectIDs(&got)); err != nil {
		t.Fatalf("第二輪失敗: %v", err)
	}
	if len(got) != 2 || got[0] != ids[3] || got[1] != ids[4] {
		t.Errorf("第二輪撈到 %v, want %v", got, ids[3:])
	}
}

// ⭐ TestClaimPendingUsesIndexWithoutFilesort 釘住撈取語句真的走索引。
//
// ⚠️ 為什麼這件事重要而且非驗不可：`FOR UPDATE` 會鎖住**掃到的每一列**。
// 若優化器選擇「掃出全部 PENDING 再排序（filesort）」，那 LIMIT 500 就只限制
// 回傳筆數，鎖卻蓋在**所有** PENDING 列上——積壓十萬筆時，一輪投遞會鎖十萬列。
//
// 這條之所以成立，靠的是 InnoDB 的一個實作細節：secondary index 的葉節點
// 隱含地帶著主鍵，所以 idx_wallet_outbox_status_created 的實際排序是
// (status, created_at, **id**) —— 剛好等於我們的 ORDER BY created_at, id。
// ⚠️ 這也是為什麼那個 `, id` 不能拿掉也不能換成別的欄位：
// 換掉的瞬間就變成 filesort，而**查詢結果完全正確**，只有鎖的範圍悄悄變成全表。
func TestClaimPendingUsesIndexWithoutFilesort(t *testing.T) {
	env := newTestEnv(t)
	for i := range 3 {
		seedOutbox(t, env, outboxTestRow{CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second)})
	}

	var plan string
	if err := env.db.WithContext(env.ctx).
		Raw("EXPLAIN FORMAT=JSON "+claimPendingOutbox, 500).Row().Scan(&plan); err != nil {
		t.Fatalf("取執行計畫失敗: %v", err)
	}
	t.Logf("執行計畫:\n%s", plan)

	if strings.Contains(plan, `"using_filesort": true`) {
		t.Error("撈取語句走了 filesort：LIMIT 只會限制回傳筆數，FOR UPDATE 卻會鎖住所有 PENDING 列")
	}
	if !strings.Contains(plan, "idx_wallet_outbox_status_created") {
		t.Error("撈取語句沒有走 idx_wallet_outbox_status_created，等於全表掃描加全表上鎖")
	}
}

// ── 併發：多副本安全 ────────────────────────────────────────────────────────

// ⭐ TestPublishPendingSkipsLockedRows 是 SKIP LOCKED 的實證。
//
// Java 版的 poller 沒有鎖，它的 javadoc 自己寫著「多副本同時輪詢會重複送同一筆」
// 並建議改用 FOR UPDATE SKIP LOCKED——這個測試證明 Go 版做到了：
// 第二個副本**不會等鎖**（那會讓兩個副本的投遞變成序列化），
// 也**不會拿到同一批**（那就是重複投遞），而是直接去撈下一批。
func TestPublishPendingSkipsLockedRows(t *testing.T) {
	env := newTestEnv(t)
	base := time.Now().UTC().Add(-time.Minute)
	first := seedOutbox(t, env, outboxTestRow{CreatedAt: base})
	second := seedOutbox(t, env, outboxTestRow{CreatedAt: base.Add(time.Second)})

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	// 副本 A：撈走最舊的一筆，然後卡在「等 Kafka ack」的位置不放鎖。
	go func() {
		_, err := env.repo.PublishPending(env.ctx, 1,
			func(_ context.Context, events []PendingEvent) ([]int64, error) {
				close(entered)
				<-release
				return []int64{events[0].ID}, nil
			})
		done <- err
	}()
	<-entered

	// 副本 B：這一刻 first 還被鎖著。它必須立刻拿到 second，而不是卡住。
	var got []int64
	start := time.Now()
	stats, err := env.repo.PublishPending(env.ctx, 10, collectIDs(&got))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("第二個副本失敗: %v", err)
	}

	if len(got) != 1 || got[0] != second {
		t.Errorf("第二個副本撈到 %v, want [%d]——撈到 %d 就是重複投遞", got, second, first)
	}
	if stats.Fetched != 1 {
		t.Errorf("stats.Fetched = %d, want 1", stats.Fetched)
	}
	// 沒有 SKIP LOCKED 的話這裡會一直等到副本 A 提交（也就是 release 之後）。
	if elapsed > 3*time.Second {
		t.Errorf("第二個副本等了 %s——看起來是在等鎖而不是跳過", elapsed)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("第一個副本失敗: %v", err)
	}
}

// ⭐⭐ TestOutboxClaimIsolationDecidesIfAccountingIsBlocked 是 outboxTxOptions
// 選 READ COMMITTED 的唯一證據（AGENTS.md 地雷 #38）。
//
// 在 MySQL 預設的 REPEATABLE READ 之下，`SELECT ... FOR UPDATE` 對索引範圍下的是
// **next-key lock**（列鎖 + 間隙鎖）。poller 撈到最後一筆（也就是追上進度、
// 掃到範圍尾端）時，那個間隙一路延伸到 supremum —— 而那正是下一筆下注要
// INSERT 新 outbox 列的位置。於是**投遞器把帳務熱路徑卡住了**，
// 卡多久取決於 Kafka 什麼時候 ack。
//
// ⚠️ 症狀是「下注偶爾變慢」，沒有任何錯誤訊息指向 poller。
// READ COMMITTED 不對搜尋下 gap lock，環就斷了——與地雷 #34 同一個藥方，
// 但那條是 debit 自己死鎖，這條是**一個背景排程去卡住帳務**。
//
// 這個測試看起來像在測資料庫而不是測自己的程式，與
// TestIsolationLevelDecidesWinnerVisibility 是同一個理由：MySQL 的鎖行為
// 是這個決定的立論基礎，它哪天變了應該是這裡先紅，而不是下注先變慢。
func TestOutboxClaimIsolationDecidesIfAccountingIsBlocked(t *testing.T) {
	tests := []struct {
		name        string
		txOpts      *sql.TxOptions
		wantBlocked bool
		reason      string
	}{
		{
			name:        "REPEATABLE_READ 會擋住帳務的 outbox INSERT",
			txOpts:      &sql.TxOptions{Isolation: sql.LevelRepeatableRead},
			wantBlocked: true,
			reason:      "MySQL 的預設。next-key lock 蓋住了新列要插進去的間隙",
		},
		{
			name:        "READ_COMMITTED 不會",
			txOpts:      &sql.TxOptions{Isolation: sql.LevelReadCommitted},
			wantBlocked: false,
			reason:      "outboxTxOptions 選的那個：不對搜尋下 gap lock",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			base := time.Now().UTC().Add(-time.Minute)
			for i := range 2 {
				seedOutbox(t, env, outboxTestRow{CreatedAt: base.Add(time.Duration(i) * time.Second)})
			}

			// ① poller 的交易：撈到底（batchSize 遠大於資料量，也就是「追上進度」
			//    的常態），然後像等 Kafka ack 一樣停在那裡不提交。
			claimer := env.db.WithContext(env.ctx).Begin(tt.txOpts)
			if claimer.Error != nil {
				t.Fatalf("開啟投遞交易失敗: %v", claimer.Error)
			}
			t.Cleanup(func() { _ = claimer.Rollback() })

			var claimed []PendingEvent
			if err := claimer.Raw(claimPendingOutbox, 500).Scan(&claimed).Error; err != nil {
				t.Fatalf("撈取待投遞事件失敗: %v", err)
			}
			if len(claimed) != 2 {
				t.Fatalf("撈到 %d 筆, want 2", len(claimed))
			}

			// ② 帳務交易：INSERT 一列新的 outbox（debit/credit 的最後一步）。
			//    把鎖等待壓到 1 秒，讓「被擋住」變成一個快速且明確的錯誤碼，
			//    而不是讓測試掛在預設的 50 秒上。
			accounting := env.db.WithContext(env.ctx).Begin()
			if accounting.Error != nil {
				t.Fatalf("開啟帳務交易失敗: %v", accounting.Error)
			}
			t.Cleanup(func() { _ = accounting.Rollback() })
			if err := accounting.Exec(`SET SESSION innodb_lock_wait_timeout = 1`).Error; err != nil {
				t.Fatalf("設定鎖等待逾時失敗: %v", err)
			}

			err := accounting.Exec(`
				INSERT INTO wallet_outbox (topic, kafka_key, payload)
				VALUES ('wallet.debit', '42', '{"transactionId":999}')`).Error

			blocked := mysqlErrNumber(err) == mysqlErrLockWaitTimeout
			if !blocked && err != nil {
				t.Fatalf("帳務 INSERT 失敗，但不是鎖等待逾時: %v", err)
			}
			if blocked != tt.wantBlocked {
				t.Errorf("帳務 INSERT 被擋住 = %v, want %v（%s）", blocked, tt.wantBlocked, tt.reason)
			}
		})
	}
}

// ── 清理排程 ────────────────────────────────────────────────────────────────

// ⭐ TestPurgeSentOutboxOnlyDeletesSent 釘住地雷 #5 的那一句：**只刪 SENT**。
//
// ⚠️ 「順手」把條件放寬成「刪掉所有超過保留期的列」不會有任何錯誤訊息，
// 它只是把還沒送出去的事件永久刪掉——而 Outbox 這整個模式存在的唯一理由
// 就是防止事件遺失。這條測試是那個規則在程式碼裡的樣子。
func TestPurgeSentOutboxOnlyDeletesSent(t *testing.T) {
	env := newTestEnv(t)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	oldSent := seedOutbox(t, env, outboxTestRow{Status: "SENT", CreatedAt: old, SentAt: &old})
	recentSent := seedOutbox(t, env, outboxTestRow{Status: "SENT", CreatedAt: recent, SentAt: &recent})
	oldPending := seedOutbox(t, env, outboxTestRow{Status: "PENDING", CreatedAt: old})
	// SENT 但 sent_at 是 NULL：理論上不該存在（只有手動改過 status 才會），
	// 但 `sent_at < ?` 對 NULL 是 UNKNOWN 而不是 false，明寫守衛讓意圖看得見。
	sentWithoutTimestamp := seedOutbox(t, env, outboxTestRow{Status: "SENT", CreatedAt: old})

	deleted, err := env.repo.PurgeSentOutbox(env.ctx, cutoff)
	if err != nil {
		t.Fatalf("清理失敗: %v", err)
	}
	if deleted != 1 {
		t.Errorf("刪了 %d 列, want 1", deleted)
	}

	survivors := map[int64]bool{}
	for _, r := range readOutbox(t, env) {
		survivors[r.ID] = true
	}
	if survivors[oldSent] {
		t.Error("保留期外的 SENT 應該被刪掉")
	}
	for _, tc := range []struct {
		id  int64
		why string
	}{
		{recentSent, "保留期內的 SENT 是事故排查的唯一證據"},
		{oldPending, "⭐ PENDING 無論多舊都不能刪——刪掉就是無聲丟失事件"},
		{sentWithoutTimestamp, "sent_at 為 NULL 的列無法判斷保留期，寧可留著"},
	} {
		if !survivors[tc.id] {
			t.Errorf("id=%d 被誤刪：%s", tc.id, tc.why)
		}
	}
}

// TestPurgeSentOutboxDeletesBeyondOneChunk 釘住分塊刪除會**繼續刪**。
//
// ⚠️ 少了迴圈的話，每天只會刪掉一塊（1000 列）。壓測級的量下這等於沒清，
// 而日誌會顯示「清理完成：刪除 1000 筆」——看起來完全正常。
func TestPurgeSentOutboxDeletesBeyondOneChunk(t *testing.T) {
	env := newTestEnv(t)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	const total = purgeChunkSize + 5

	// 一句多列 INSERT：逐列塞 1005 次會讓這個測試變成最慢的一條。
	var values []string
	var args []any
	for range total {
		values = append(values, "('wallet.debit', '42', '{}', 'SENT', ?, ?)")
		args = append(args, old, old)
	}
	if err := env.db.WithContext(env.ctx).Exec(
		`INSERT INTO wallet_outbox (topic, kafka_key, payload, status, created_at, sent_at) VALUES `+
			strings.Join(values, ","), args...).Error; err != nil {
		t.Fatalf("塞入測試資料失敗: %v", err)
	}

	deleted, err := env.repo.PurgeSentOutbox(env.ctx, time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("清理失敗: %v", err)
	}
	if deleted != total {
		t.Errorf("刪了 %d 列, want %d——分塊迴圈是不是只跑了一輪？", deleted, total)
	}
	if n := env.count(t, "wallet_outbox"); n != 0 {
		t.Errorf("清理後還剩 %d 列", n)
	}
}

// TestPurgeSentOutboxStopsOnContextCancel 確認清理吃取消訊號。
//
// 與 poller 的一輪**刻意相反**（見 outbox.Purger.RunOnce 的註解）：
// 清理刪到一半被中斷只是「這次少刪幾塊」，下次排程接著刪；
// 而投遞被中斷會造成重複投遞。同一個專案裡兩個相反的決定，理由各自成立。
func TestPurgeSentOutboxStopsOnContextCancel(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(env.ctx)
	cancel()

	_, err := env.repo.PurgeSentOutbox(ctx, time.Now().UTC())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
