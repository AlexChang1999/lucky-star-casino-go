//go:build infra

// 這個檔需要真的跑起來的 Redis，所以放在 infra build tag 後面：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d redis --wait
//	go test -race -tags=infra ./internal/wallet/quota/
//
// ⭐ **為什麼這一層的測試一定要打真的 Redis**：這個套件的正確性幾乎全部來自
// Redis 自己的語義——Lua 的原子性、`PEXPIRE 0` 會刪 key、`DECRBY` 打在不存在的
// key 上會建立負數且無 TTL、`SET ... KEEPTTL` 保留到期時間。
// 這些**沒有一項**是我們自己的程式碼在做的事，用假物件測等於在測假物件。
// （miniredis 之類的記憶體替身對 Lua 的支援是部分的，用它會得到「測試綠、
// 正式紅」——那比沒有測試更糟。）
package quota

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/redistest"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(redistest.NewClient(t), discardLogger())
	if err != nil {
		t.Fatalf("建立額度層失敗: %v", err)
	}
	return s
}

// ── 原語一：一次性宣告 ──────────────────────────────────────────────────────

func TestClaimBankruptcyAidIsOncePerDay(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)

	if err := s.ClaimBankruptcyAid(ctx, player); err != nil {
		t.Fatalf("第一次領取應該成功: %v", err)
	}

	err := s.ClaimBankruptcyAid(ctx, player)
	if !errors.Is(err, ErrAlreadyClaimedToday) {
		t.Fatalf("第二次領取 err = %v, want ErrAlreadyClaimedToday", err)
	}
}

// TestClaimSetsTTLInTheSameCommand 釘住「鎖與 TTL 是同一個指令」。
//
// ⭐ 這是 AGENTS.md 那類「沒有錯誤訊息」的坑裡最典型的一個：
// 如果實作寫成 `SETNX` 之後再 `EXPIRE`，功能測試（領一次、再領一次被拒）
// **完全會過**，因為那兩步在正常情況下都會成功。
// 只有「進程剛好死在兩步之間」才會留下一個永不過期的鎖，
// 而那個玩家**從此再也領不到救助**。
//
// 所以這個測試斷言的不是行為，是**狀態**：key 一定要帶著 TTL。
func TestClaimSetsTTLInTheSameCommand(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)

	now := time.Now()
	if err := s.ClaimBankruptcyAid(ctx, player); err != nil {
		t.Fatalf("領取失敗: %v", err)
	}

	ttl := s.client.PTTL(ctx, bankruptcyAidKey(player, dayStamp(now))).Val()
	switch {
	case ttl == -1:
		t.Fatal("領取鎖沒有 TTL——玩家會永遠領不到（SETNX 與 EXPIRE 是不是拆成兩步了？）")
	case ttl == -2:
		t.Fatal("領取鎖不存在")
	case ttl <= 0 || ttl > 24*time.Hour:
		t.Fatalf("領取鎖 TTL = %s，應落在 (0, 24h]", ttl)
	}

	// 再確認它真的對齊台北午夜，而不是隨手設的固定值（例如 24h）。
	want := untilNextMidnight(now)
	if diff := want - ttl; diff < 0 || diff > 5*time.Second {
		t.Errorf("TTL = %s，與到台北午夜的時距 %s 差太多", ttl, want)
	}
}

// TestClaimIsAtomicUnderConcurrency 是「同一個玩家同時打 N 個請求」的併發證明。
//
// ⚠️ 必須跑在 -race 下才算數（AGENTS.md §4）。
//
// Java 版的 javadoc 說「兩個同時請求都通過資格檢查時，Redis SETNX 只有一個
// 會成功」——這個測試就是那句話的可執行版本。
func TestClaimIsAtomicUnderConcurrency(t *testing.T) {
	const goroutines = 20

	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)

	var (
		mu       sync.Mutex
		won      int
		rejected int
		other    []error
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start // 一起出發，把競態窗口開到最大
			err := s.ClaimBankruptcyAid(ctx, player)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrAlreadyClaimedToday):
				rejected++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("出現預期外的錯誤: %v", other)
	}
	if won != 1 {
		t.Errorf("搶到的數量 = %d, want 1——%d 個併發請求只能有一個領到救助", won, goroutines)
	}
	if rejected != goroutines-1 {
		t.Errorf("被拒絕的數量 = %d, want %d", rejected, goroutines-1)
	}
}

func TestReleaseBankruptcyAidAllowsRetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)

	if err := s.ClaimBankruptcyAid(ctx, player); err != nil {
		t.Fatalf("領取失敗: %v", err)
	}
	// 模擬「入帳失敗，把資格還回去」。
	s.ReleaseBankruptcyAid(ctx, player)

	if err := s.ClaimBankruptcyAid(ctx, player); err != nil {
		t.Fatalf("釋放之後應該可以重新領取: %v", err)
	}
}

// TestReleaseBankruptcyAidSurvivesCancelledContext 釘住 context.WithoutCancel。
//
// ⭐ 清理路徑一律不吃取消訊號（AGENTS.md 地雷 #20 的同一類）。
// 這支只會在錯誤處理路徑上被呼叫，而那時請求的 ctx 很可能已經因為
// 客戶端斷線或逾時而取消了。沿用那個 ctx 的話**清理必定失敗**，
// 玩家的資格就要卡到午夜——而日誌上只會有一行看起來很合理的
// 「context canceled」。
func TestReleaseBankruptcyAidSurvivesCancelledContext(t *testing.T) {
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)

	if err := s.ClaimBankruptcyAid(context.Background(), player); err != nil {
		t.Fatalf("領取失敗: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // 已經取消了
	s.ReleaseBankruptcyAid(cancelled, player)

	if err := s.ClaimBankruptcyAid(context.Background(), player); err != nil {
		t.Fatalf("即使傳進已取消的 ctx，釋放仍應生效（是不是漏了 context.WithoutCancel？）: %v", err)
	}
}

// ── 原語二：可回補的額度預扣 ────────────────────────────────────────────────

func TestReserveGiftQuotaEnforcesSentLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sender := redistest.UniquePlayerID(t, s.client)
	receiver := redistest.UniquePlayerID(t, s.client)

	// 剛好用到上限：應該放行（Java 是 `total > LIMIT` 才拒絕，等於不含上限本身）。
	if err := s.ReserveGiftQuota(ctx, sender, receiver, DailyGiftSentLimit); err != nil {
		t.Fatalf("剛好等於上限應該放行: %v", err)
	}
	// 再多一枚星幣就超限。
	err := s.ReserveGiftQuota(ctx, sender, receiver, 1)
	if !errors.Is(err, ErrGiftSentLimitExceeded) {
		t.Fatalf("超出贈出上限 err = %v, want ErrGiftSentLimitExceeded", err)
	}
}

func TestReserveGiftQuotaEnforcesRecvLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	receiver := redistest.UniquePlayerID(t, s.client)

	// 收受上限（10,000）是贈出上限（5,000）的兩倍，所以要用**兩個不同的贈送方**
	// 才碰得到它——單一贈送方會先撞上自己的贈出上限。
	// ⚠️ 這也是為什麼兩個上限必須是兩個 sentinel：呼叫端要回不同的訊息。
	for i := range 2 {
		sender := redistest.UniquePlayerID(t, s.client)
		if err := s.ReserveGiftQuota(ctx, sender, receiver, DailyGiftSentLimit); err != nil {
			t.Fatalf("第 %d 個贈送方應該放行: %v", i+1, err)
		}
	}

	third := redistest.UniquePlayerID(t, s.client)
	err := s.ReserveGiftQuota(ctx, third, receiver, 1)
	if !errors.Is(err, ErrGiftRecvLimitExceeded) {
		t.Fatalf("超出收受上限 err = %v, want ErrGiftRecvLimitExceeded", err)
	}
}

// TestReserveRollsBackBothKeysWhenRejected 釘住「超限時兩個計數器都要復原」。
//
// ⭐ 這是 Java 版分歧 #3 的直接對照。Java 是 sent、recv 分兩次 INCRBY，
// 中間任何失敗都會讓其中一邊的預扣卡到午夜；而且它的回補是在 Lua 之外做的，
// 所以「已經加上去、還沒減回來」這個中間狀態**別的請求讀得到**。
//
// 這裡斷言的是：被拒絕的那一次呼叫，對兩個 key 的淨影響都是 0。
func TestReserveRollsBackBothKeysWhenRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sender := redistest.UniquePlayerID(t, s.client)
	receiver := redistest.UniquePlayerID(t, s.client)
	day := dayStamp(time.Now())

	const accepted = domain.Amount(4_000)
	if err := s.ReserveGiftQuota(ctx, sender, receiver, accepted); err != nil {
		t.Fatalf("第一次預扣應該成功: %v", err)
	}

	// 這一筆會讓 sent 變成 6,000 > 5,000，必定被拒。
	if err := s.ReserveGiftQuota(ctx, sender, receiver, 2_000); !errors.Is(err, ErrGiftSentLimitExceeded) {
		t.Fatalf("err = %v, want ErrGiftSentLimitExceeded", err)
	}

	sent := s.client.Get(ctx, giftSentKey(sender, day)).Val()
	recv := s.client.Get(ctx, giftRecvKey(receiver, day)).Val()
	if sent != "4000" {
		t.Errorf("贈出計數 = %s, want 4000——被拒絕的預扣沒有完全回補", sent)
	}
	if recv != "4000" {
		t.Errorf("收受計數 = %s, want 4000——被拒絕時只回補了 sent，recv 漏了", recv)
	}
}

// TestReserveSetsTTLAtomically 釘住 Java 版分歧 #1。
//
// ⭐ Java 的 `ensureMidnightExpiry` 是 `INCRBY` → `TTL` → `EXPIREAT` 三個往返。
// 進程在 INCRBY 之後、EXPIREAT 之前被硬殺（OOMKilled、`docker rm -f`、
// K8s 驅逐），那個 key 就**永遠不會過期**——該玩家從此送不出禮物，
// 而且沒有任何錯誤訊息。
//
// 諷刺的是同一個 repo 的 BankruptcyAidService 註解自己寫著要避免這件事。
// 這裡把 INCRBY 與 PEXPIRE 包進同一段 Lua，那個窗口在結構上就不存在。
func TestReserveSetsTTLAtomically(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sender := redistest.UniquePlayerID(t, s.client)
	receiver := redistest.UniquePlayerID(t, s.client)

	now := time.Now()
	if err := s.ReserveGiftQuota(ctx, sender, receiver, 100); err != nil {
		t.Fatalf("預扣失敗: %v", err)
	}

	day := dayStamp(now)
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"贈出計數", giftSentKey(sender, day)},
		{"收受計數", giftRecvKey(receiver, day)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ttl := s.client.PTTL(ctx, tc.key).Val()
			if ttl == -1 {
				t.Fatal("計數器沒有 TTL——這個玩家的當日額度永遠不會歸零")
			}
			if ttl <= 0 || ttl > 24*time.Hour {
				t.Fatalf("TTL = %s，應落在 (0, 24h]", ttl)
			}
		})
	}
}

// TestNaiveDecrByResurrectsKeyWithoutTTL **證明 Java 版分歧 #2 的雷是真的**。
//
// ⭐ 這個測試不測我們的程式碼，它測的是 **Redis 的行為**——因為那正是
// Java 版踩到的東西，而「DECRBY 一個不存在的 key 會怎樣」多數人答不出來。
//
// 答案：Redis 把不存在當成 0，於是 **DECRBY 會建立這個 key**、值為負數、
// **而且沒有 TTL**。Java 的 releaseDailyQuota 是裸的 DECRBY，
// 且回補路徑不呼叫 ensureMidnightExpiry。
//
// 觸發條件只要「預扣與回補之間跨過午夜」——轉帳花幾百毫秒，
// 23:59:59.9 進來就會踩到。後果是那個玩家的當日贈出計數從此是負數且永不過期，
// 於是他每天的實際額度變成 `5000 + 那筆金額`，**永久**。
//
// 這一格若哪天紅了，代表 Redis 改了語義，那時 releaseGiftScript 的
// EXISTS 檢查就可以拿掉。
func TestNaiveDecrByResurrectsKeyWithoutTTL(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)
	key := giftSentKey(player, dayStamp(time.Now()))

	// 前提：key 不存在（模擬「已經跨過午夜、key 過期了」）。
	if n := s.client.Exists(ctx, key).Val(); n != 0 {
		t.Fatalf("前提不成立，key 已存在: %s", key)
	}

	got := s.client.DecrBy(ctx, key, 500).Val()
	if got != -500 {
		t.Fatalf("DECRBY 不存在的 key = %d, want -500", got)
	}
	if ttl := s.client.PTTL(ctx, key).Val(); ttl != -1 {
		t.Fatalf("PTTL = %s, want -1（無 TTL）——Redis 的語義變了？", ttl)
	}
	t.Logf("已確認：DECRBY 打在不存在的 key 上會建立 %d 且無 TTL——這正是 releaseGiftScript 要防的", got)
}

// TestReleaseDoesNotResurrectExpiredKey 是上一個測試的鏡像：同樣的情境，
// 走我們的腳本就不會留下那個永久負數。
func TestReleaseDoesNotResurrectExpiredKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sender := redistest.UniquePlayerID(t, s.client)
	receiver := redistest.UniquePlayerID(t, s.client)
	day := dayStamp(time.Now())
	sentKey, recvKey := giftSentKey(sender, day), giftRecvKey(receiver, day)

	if err := s.ReserveGiftQuota(ctx, sender, receiver, 500); err != nil {
		t.Fatalf("預扣失敗: %v", err)
	}
	// 模擬「回補之前跨過了午夜」——直接刪掉，效果與 TTL 到期相同。
	s.client.Del(ctx, sentKey, recvKey)

	s.ReleaseGiftQuota(ctx, sender, receiver, 500)

	for _, key := range []string{sentKey, recvKey} {
		if n := s.client.Exists(ctx, key).Val(); n != 0 {
			v := s.client.Get(ctx, key).Val()
			ttl := s.client.PTTL(ctx, key).Val()
			t.Errorf("回補把過期的 key 復活了: %s = %s (ttl=%s)——玩家隔天額度會變多", key, v, ttl)
		}
	}
}

// TestReleaseNeverGoesNegativeAndKeepsTTL 釘住兩件事：夾在 0、且不清掉 TTL。
//
// ⚠️ 第二件事很容易寫錯：用普通的 `SET key 0` 會把 TTL 清掉，
// 於是為了修分歧 #2 反而自己製造出分歧 #1（永不過期的 key）。
// 正解是 `SET key 0 KEEPTTL`（Redis 6.0+）。
func TestReleaseNeverGoesNegativeAndKeepsTTL(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sender := redistest.UniquePlayerID(t, s.client)
	receiver := redistest.UniquePlayerID(t, s.client)
	day := dayStamp(time.Now())

	if err := s.ReserveGiftQuota(ctx, sender, receiver, 100); err != nil {
		t.Fatalf("預扣失敗: %v", err)
	}
	// 回補的量刻意大於預扣量（呼叫端傳錯、或重複回補）。
	s.ReleaseGiftQuota(ctx, sender, receiver, 500)

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"贈出計數", giftSentKey(sender, day)},
		{"收受計數", giftRecvKey(receiver, day)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := s.client.Get(ctx, tc.key).Val(); v != "0" {
				t.Errorf("計數 = %s, want 0——負數會變成隔天的額外額度", v)
			}
			if ttl := s.client.PTTL(ctx, tc.key).Val(); ttl == -1 {
				t.Error("夾成 0 的時候把 TTL 清掉了（SET 是不是漏了 KEEPTTL？）")
			}
		})
	}
}

// TestReserveIsAtomicUnderConcurrency 是這個套件的併發主證明。
//
// ⚠️ 必須跑在 -race 下（AGENTS.md §4）。
//
// ⭐ 50 個 goroutine 同時對**同一個贈送方**各預扣 200，上限 5,000。
// 正確的結果只有一種：**剛好 25 筆成功、25 筆被拒、最終計數剛好等於 5,000**。
//
// ⚠️ **這一格不是 Lua 的功勞，要講清楚**（2026-08-13 實測後更正）：
// 把 Lua 換成「INCRBY → 在 Go 判斷 → 超限才 DECRBY」，這個測試**照樣會過**。
// 原因是 `INCRBY` 本身就是原子的、而且**回傳遞增後的新值**，
// 於是每個請求都是拿「我自己那一次遞增之後的總額」做判斷——
// Redis 的單執行緒已經把 admission 決策序列化了，Go 這邊怎麼判斷都不影響。
// Java 的 javadoc 說「多計只會讓額度更嚴格、不會讓玩家超額」，**它是對的**。
//
// 所以這一格真正的價值是**證明沒有超收**（額度層最基本的義務），
// 不是證明 Lua 的必要性。Lua 的必要性由下一個測試證明。
//
// 這個更正本身值得記住：**「我用了原子操作」與「這個測試證明了原子性」是兩件事。**
// 沒有做過對照實驗就寫下「拿掉它就會紅」，那是一句沒有根據的聲稱。
func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	const (
		goroutines = 50
		each       = domain.Amount(200)
	)
	wantWinners := int(DailyGiftSentLimit / each) // 25

	ctx := context.Background()
	s := newTestStore(t)
	sender := redistest.UniquePlayerID(t, s.client)
	receiver := redistest.UniquePlayerID(t, s.client)

	var (
		mu       sync.Mutex
		accepted int
		rejected int
		other    []error
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			err := s.ReserveGiftQuota(ctx, sender, receiver, each)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrGiftSentLimitExceeded):
				rejected++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("出現預期外的錯誤: %v", other)
	}
	if accepted != wantWinners {
		t.Errorf("放行數 = %d, want %d", accepted, wantWinners)
	}
	if rejected != goroutines-wantWinners {
		t.Errorf("拒絕數 = %d, want %d", rejected, goroutines-wantWinners)
	}

	// ⭐ 最重要的一行：計數器**絕對不可以**超過上限。
	// 超過就代表有請求在「已經滿了」的狀態下還是被放行了。
	final := s.client.Get(ctx, giftSentKey(sender, dayStamp(time.Now()))).Val()
	if want := "5000"; final != want {
		t.Errorf("最終贈出計數 = %s, want %s——原子性破了", final, want)
	}
}

// TestReserveDoesNotFalselyRejectWhileOthersRollBack 是 Lua 真正買到的東西。
//
// ⭐ 情境：贈出計數已經墊到「上限 -1」，只剩 1 枚星幣的空間。此時同時進來
// 8 筆**必定超限**的大額請求，與 1 筆只要 1 枚星幣的小額請求。
// 大額的全都會被拒絕並回補，所以小額那筆**必須**被放行——它要的空間一直都在。
//
// 非原子版做不到這件事：大額請求在「已經 INCRBY、還沒 DECRBY 回補」的窗口裡，
// counter 是**虛高的**，小額請求剛好落在那裡就會被誤判成超限。
// Lua 讓 INCRBY 與回補位於同一個原子單位，那個窗口在結構上不存在。
//
// ⭐ **實測對照**（2026-08-13，本機 Redis 7.4.9，60 輪 × 8 筆干擾，跑兩次）：
//
//	Lua 原子版          誤拒 0 / 60、0 / 60
//	Java 形狀（非原子）  誤拒 46 / 60、49 / 60   （約 77~82%）
//
// ⚠️ 症狀是這條雷最惡劣的地方：玩家收到 422「你今天送太多了」，
// 但他其實還有額度；而且**重試就會成功**，所以客服重現不出來，
// 最後會被歸類成「玩家記錯了」。沒有任何錯誤訊息指向真因。
//
// ⚠️ 這個測試是**確定性**的，不是機率性的：Lua 版的小額請求應該永遠被放行。
// 它若偶爾紅，代表原子性真的破了，不是測試不穩。
func TestReserveDoesNotFalselyRejectWhileOthersRollBack(t *testing.T) {
	const (
		rounds   = 10
		blockers = 8
	)

	ctx := context.Background()
	s := newTestStore(t)

	for round := range rounds {
		sender := redistest.UniquePlayerID(t, s.client)

		// 墊到只剩 1 枚的空間。用專屬收受方，讓拒絕只可能來自贈出上限。
		if err := s.ReserveGiftQuota(ctx, sender, redistest.UniquePlayerID(t, s.client), DailyGiftSentLimit-1); err != nil {
			t.Fatalf("第 %d 輪墊底失敗: %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})

		wg.Add(blockers)
		for range blockers {
			receiver := redistest.UniquePlayerID(t, s.client)
			go func() {
				defer wg.Done()
				<-start
				// 必定超限（上限-1 + 上限 > 上限），結果不重要，重點是它會回補。
				_ = s.ReserveGiftQuota(ctx, sender, receiver, DailyGiftSentLimit)
			}()
		}

		var smallErr error
		smallReceiver := redistest.UniquePlayerID(t, s.client)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			smallErr = s.ReserveGiftQuota(ctx, sender, smallReceiver, 1)
		}()

		close(start)
		wg.Wait()

		if smallErr != nil {
			t.Fatalf("第 %d 輪：只要 1 枚星幣的請求被誤拒了（額度明明還有）: %v", round, smallErr)
		}
	}
}

func TestReserveRejectsNonPositiveAmount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, tt := range []struct {
		name   string
		amount domain.Amount
	}{
		{"零", 0},
		{"負一", -1},
		// ⚠️ 這一格是重點：負數金額會讓 INCRBY 變成「發額度」，
		// 也就是把額度層反過來當後門用。
		{"大額負數", -DailyGiftSentLimit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sender := redistest.UniquePlayerID(t, s.client)
			receiver := redistest.UniquePlayerID(t, s.client)

			err := s.ReserveGiftQuota(ctx, sender, receiver, tt.amount)
			if !errors.Is(err, ErrInvalidAmount) {
				t.Fatalf("err = %v, want ErrInvalidAmount", err)
			}
			// 而且不可以留下任何痕跡——建了 key 就等於偷偷改了額度基準。
			key := giftSentKey(sender, dayStamp(time.Now()))
			if n := s.client.Exists(ctx, key).Val(); n != 0 {
				t.Errorf("被拒絕的請求不該建立 key: %s = %s", key, s.client.Get(ctx, key).Val())
			}
		})
	}
}

// ── 故障策略 ────────────────────────────────────────────────────────────────

// TestFailsClosedWhenRedisIsDown 把 fail-closed 從「沒人寫 catch 的副作用」
// 變成「有測試釘住的行為」。
//
// ⭐ 這正是套件說明講的那件事：Java 版也是 fail-closed，但它是
// **因為沒有人寫 catch**。差別在於，哪天有人為了「Redis 抖一下不要影響玩家」
// 而加上 catch 並放行，那個改動看起來完全合理、也不會讓任何測試變紅。
// 有了這一格，那個改動會立刻紅。
//
// ⚠️ 斷言刻意不只是「有 error」，還要「**不是**額度用盡的 sentinel」：
// 如果哪天有人把 Redis 故障誤映射成 ErrGiftSentLimitExceeded，
// 對外就會變成 422「你今天送太多了」——玩家收到一個關於自己行為的錯誤訊息，
// 而真因是我們的基礎設施掛了。那種錯誤訊息會讓查修往完全錯誤的方向走。
func TestFailsClosedWhenRedisIsDown(t *testing.T) {
	// 127.0.0.1:1 是保留埠，連不上。MaxRetries 設 -1 關掉重試讓測試跑得快。
	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = dead.Close() })

	s, err := New(dead, discardLogger())
	if err != nil {
		t.Fatalf("建立額度層失敗: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("預扣", func(t *testing.T) {
		err := s.ReserveGiftQuota(ctx, 1, 2, 100)
		if err == nil {
			t.Fatal("Redis 掛掉時預扣回 nil——額度層靜靜失效了")
		}
		if errors.Is(err, ErrGiftSentLimitExceeded) || errors.Is(err, ErrGiftRecvLimitExceeded) {
			t.Fatalf("基礎設施故障被誤映射成額度用盡: %v", err)
		}
	})

	t.Run("領取", func(t *testing.T) {
		err := s.ClaimBankruptcyAid(ctx, 1)
		if err == nil {
			t.Fatal("Redis 掛掉時領取回 nil——會免費送出救助金")
		}
		if errors.Is(err, ErrAlreadyClaimedToday) {
			t.Fatalf("基礎設施故障被誤映射成已領取: %v", err)
		}
	})
}

// ── Redis 語義的直接驗證 ────────────────────────────────────────────────────

// TestPExpireZeroDeletesKey **證明 daykey.go 那個夾值防的是真的東西**。
//
// ⭐ 多數人以為 `PEXPIRE key 0` 是「立刻過期」或「不設過期」，
// 實際上 Redis 會**當場刪掉這個 key**（等同 DEL）。
//
// 所以「午夜前不到 1 毫秒進來的請求」如果沒有夾值，會發生：
// INCRBY 建立 key → PEXPIRE 0 把它刪掉 → 玩家的當日額度歸零重來。
// 這個測試讓那個推論變成可執行的事實，而不是註解裡的一句聲稱。
func TestPExpireZeroDeletesKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)
	key := giftSentKey(player, dayStamp(time.Now()))

	if err := s.client.Set(ctx, key, "123", time.Hour).Err(); err != nil {
		t.Fatalf("寫入測試 key 失敗: %v", err)
	}
	if err := s.client.PExpire(ctx, key, 0).Err(); err != nil {
		t.Fatalf("PEXPIRE 失敗: %v", err)
	}

	if n := s.client.Exists(ctx, key).Val(); n != 0 {
		t.Fatalf("PEXPIRE 0 之後 key 還在（Redis 的語義變了？）——daykey.go 的夾值可以重新評估")
	}
	t.Log("已確認：PEXPIRE key 0 會直接刪掉 key，所以 TTL 必須夾在 1 毫秒以上")
}

// TestTTLTracksTaipeiMidnight 用注入的時鐘驗證 TTL 真的貼著台北午夜走。
//
// 前面的測試都在「現在」跑，而現在距離午夜可能還有好幾個小時，
// 看不出「日界到底是不是台北」。這裡把時鐘釘在台北 23:00，
// 期望 TTL 是 1 小時——如果有人把時區改成 UTC，這裡會變成 9 小時。
func TestTTLTracksTaipeiMidnight(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	player := redistest.UniquePlayerID(t, s.client)

	fixed := time.Date(2026, 8, 13, 23, 0, 0, 0, dayZone)
	s.now = func() time.Time { return fixed }

	if err := s.ClaimBankruptcyAid(ctx, player); err != nil {
		t.Fatalf("領取失敗: %v", err)
	}

	ttl := s.client.PTTL(ctx, bankruptcyAidKey(player, "2026-08-13")).Val()
	if diff := time.Hour - ttl; diff < 0 || diff > 5*time.Second {
		t.Fatalf("TTL = %s, want ≈1h（台北 23:00 到午夜）——日界是不是被改成 UTC 了？", ttl)
	}
}
