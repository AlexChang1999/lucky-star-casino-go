// Package quota 是 wallet 的 Redis 額度層。
//
// # 這一層是什麼
//
// 它**完整覆蓋** Java 版 `wallet-service` 的 Redis 使用面——逐檔查證後
// 剛好三個 key（`docs/notes/Java版-Redis-用途全解.md` §2.4）：
//
//	wallet:bankruptcy-aid:{playerId}:{date}   每日一次的破產救助領取鎖
//	wallet:gift:sent:{senderId}:{date}        當日贈出額度
//	wallet:gift:recv:{receiverId}:{date}      當日收受額度
//
// 歸納起來只有**兩個原語**：
//
//	ClaimBankruptcyAid  一次性宣告（搶得到 / 搶不到），對應 SET NX PX
//	ReserveGiftQuota    可回補的額度預扣，對應 INCRBY + 上限檢查 + DECRBY
//
// # ⭐ 這一層**不是快取**
//
// 這是整個套件最重要的一句話。快取的定義是「資料的真相在別處，這裡只是副本，
// 掉了重算即可」。這裡三個 key 都不是那樣：它們是**額度狀態本身**，
// 沒有任何地方可以重算出「這個玩家今天已經送出多少星幣」。
//
// 帶來的具體差別是**故障策略**（見下一節）：快取掉了應該 fail-open
// （回源頭重算，慢一點而已），額度狀態掉了必須 fail-closed
// （不知道用掉多少，就不能放行）。**把這一層當快取看待，寫出來的錯誤處理會全錯。**
//
// ⚠️ 對照 AGENTS.md 地雷 #16：Redis 在本專案是主儲存。這一層是那條雷的實例。
//
// # 故障策略（fail-open vs fail-closed）
//
// 判準來自 `docs/notes/Java版-Redis-用途全解.md` §3.5：**問「放行的代價是什麼」**。
//
//	操作                   Redis 掛掉時   放行的代價                                   結論
//	ClaimBankruptcyAid    拒絕（500）   免費送出 1,000 星幣，且無法知道是否已送過      fail-closed
//	ReserveGiftQuota      拒絕（500）   玩家可無限贈送，當日上限形同不存在            fail-closed
//	Release*              忽略（WARN）  —— 見下方各自的說明                          不影響請求結果
//
// 兩個 Reserve 都 fail-closed，與 Java 版一致（Java 是讓例外往外拋成 500）。
//
// ⚠️ 但**「例外往外拋」不等於「有策略」**。Java 那邊 fail-closed 是
// 「沒有人寫 catch」的副作用，而不是一個決定——差別在於：如果哪天有人為了
// 「Redis 抖一下不要影響玩家」而加上一個 catch 並放行，那個改動看起來完全合理，
// 也不會有任何測試變紅。這一層把它寫成**明確的、有測試釘住的**行為
// （TestReserveFailsClosedWhenRedisIsDown）。
//
// ⚠️ 破產救助其實還有第二道防線：`credit` 的冪等鍵 `bankruptcy-aid:{id}:{date}`
// 走 UNIQUE 索引，所以就算 Redis 整個被清空，同一天也不會重複入帳（只會多一次
// 撞鍵的往返）。也就是說**這裡改成 fail-open 在帳務上是安全的**。
// 仍然維持 fail-closed 的理由是**等價**：Java 版在 Redis 掛掉時回 500，
// 改成放行是對外行為漂移，要改必須先讓契約測試涵蓋它（藍圖 §5 的規矩）。
//
// # 與 Java 版的三處刻意分歧（都是修 bug，不是品味）
//
//  1. **TTL 與計數在同一個原子操作裡**。Java 的 `GiftService.ensureMidnightExpiry`
//     是 `INCRBY` → `TTL` → `EXPIREAT` 三個往返，進程在中間被硬殺的話，
//     那個 key **永遠不會過期** → 該玩家**從此再也送不出禮物**。
//     諷刺的是同一個 repo 的 `BankruptcyAidService` 註解自己寫著要避免這件事
//     （「用 SETNX + TTL 一次完成」），只是 gift 那邊沒照做。
//
//  2. **回補不會製造出永久的負數計數器**。Java 的 `releaseDailyQuota` 直接
//     `DECRBY`，而 `DECRBY` 打在**已經過期的 key** 上會**重新建立**它、值為負數、
//     **而且沒有 TTL**（release 路徑不呼叫 ensureMidnightExpiry）。
//     後果是那個玩家隔天的額度變成 `5000 + 本次金額`，而且**永久有效**。
//     觸發條件只要「預扣與回補之間跨過午夜」。
//
//  3. **兩個 key 一起成功或一起失敗**。Java 是 sent、recv 分兩次 `INCRBY`，
//     而且 `reserveDailyQuota` 在 `gift()` 的 try 區塊**之外**——所以第二次
//     INCRBY 若失敗，第一次的預扣**不會被回補**，那筆額度要卡到午夜。
//
//  4. **超限回補與計數在同一個原子單位裡**，於是併發時不會誤拒別人。
//     Java 的大額請求在「已 INCRBY、還沒 DECRBY 回補」的窗口裡把 counter
//     撐得虛高，此時進來的小額請求會被誤判成超限。
//     ⭐ **實測**（2026-08-13、Redis 7.4.9、60 輪 × 8 筆併發干擾、跑兩次）：
//     Lua 原子版誤拒 **0/60**，Java 形狀誤拒 **46/60 與 49/60**（約 77~82%）。
//     由 TestReserveDoesNotFalselyRejectWhileOthersRollBack 釘住。
//
// ⚠️ **反過來，有一件事 Java 版是對的，不要跟著改**：非原子版**不會超收**。
// `INCRBY` 本身原子且回傳遞增後的新值，所以 admission 決策早就被 Redis 的
// 單執行緒序列化了。Java 的 javadoc 寫「多計只會讓額度更嚴格、不會讓玩家超額」
// ——那句話是對的。我第一版的註解宣稱「拿掉 Lua 就會超收」，實測後證明是錯的
// （見 TestReserveIsAtomicUnderConcurrency 的說明）。
// **「我用了原子操作」與「這個測試證明了原子性」是兩件事。**
//
// 四條的共同點：**都沒有錯誤訊息，都要等玩家客訴才會發現**，
// 而第 4 條最惡劣——它**重試就會成功**，所以客服重現不出來。
// 四條都由 quota_infra_test.go 的對應測試釘住。
package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

// 每日上限，逐字對齊 Java 的 `GiftService.DAILY_SENT_LIMIT` / `DAILY_RECV_LIMIT`。
//
// ⚠️ 這兩個數字是**業務規則**不是調校參數，不可以「順手」改成環境變數。
// 做成可設定會讓「正式環境的上限到底是多少」變成一個要去翻部署設定才能回答的
// 問題，而額度是會被拿去做風控報表的東西。要改就改這裡，並且進 CHANGELOG。
const (
	DailyGiftSentLimit domain.Amount = 5_000
	DailyGiftRecvLimit domain.Amount = 10_000
)

var (
	// ErrAlreadyClaimedToday 代表當日已經領取過。呼叫端應翻成 422（對齊 Java 的
	// BankruptcyAidNotEligibleException）。
	ErrAlreadyClaimedToday = errors.New("當日已領取過")

	// ErrGiftSentLimitExceeded / ErrGiftRecvLimitExceeded 分成兩個 sentinel，
	// 是因為 Java 對兩者回**不同的訊息文字**，而契約測試會 diff 訊息：
	//   "Daily gift-sent limit exceeded (limit 5000)"
	//   "Receiver daily gift-received limit exceeded (limit 10000)"
	// 合併成一個 ErrLimitExceeded 會讓 HTTP 層分不出該回哪一句。
	ErrGiftSentLimitExceeded = errors.New("贈出方當日贈禮額度已用盡")
	ErrGiftRecvLimitExceeded = errors.New("接收方當日收禮額度已用盡")

	// ErrInvalidAmount 擋非正數金額。
	//
	// ⚠️ domain 已經驗過一次，這裡**再擋一次是刻意的縱深防禦**：
	// `INCRBY key -500` 會讓計數器往下走，也就是**把額度層反過來當成發額度的工具**。
	// 一個負數金額從任何路徑漏進來，這一層就從「上限」變成「後門」。
	// 帳務相關的縱深防禦不算重複程式碼。
	ErrInvalidAmount = errors.New("金額必須為正整數")
)

// Store 是 wallet 的 Redis 額度層。
//
// ⭐ **這裡刻意沒有 interface**。CLAUDE.md §2 的規矩是「interface 放在消費端與
// 外部邊界」，而這一層的消費端（之後的 gift / bankruptcy-aid service）會自己
// 宣告它需要的小介面——就像 httpapi.Store 與 outbox.eventStore 那樣。
// 在**提供者**這邊先定義一個 `Quota` 介面是 Java 的方向（先介面再 Impl），
// 在 Go 是反的。而且測試不需要它：Redis 的行為（Lua 原子性、TTL、
// DECRBY 建 key）**正是這一層唯一值得測的東西**，用假物件測等於什麼都沒測。
type Store struct {
	client *redis.Client
	logger *slog.Logger

	// now 讓測試可以把時間釘在「午夜前 1 毫秒」這種位置。
	// 正式路徑一律是 time.Now——不做成設定，時鐘不是可設定項。
	now func() time.Time
}

// New 組出額度層。
func New(client *redis.Client, logger *slog.Logger) (*Store, error) {
	if client == nil {
		return nil, errors.New("quota.New: client 不可為 nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{client: client, logger: logger, now: time.Now}, nil
}

// ── key 組裝 ────────────────────────────────────────────────────────────────
//
// ⚠️ 三個格式**逐字對齊 Java**（GiftService:82-83、BankruptcyAidService:84）。
// 理由見 daykey.go 的 dayStamp：key 名稱是跨版本、跨服務的隱含契約。

func bankruptcyAidKey(playerID int64, day string) string {
	return fmt.Sprintf("wallet:bankruptcy-aid:%d:%s", playerID, day)
}

func giftSentKey(playerID int64, day string) string {
	return fmt.Sprintf("wallet:gift:sent:%d:%s", playerID, day)
}

func giftRecvKey(playerID int64, day string) string {
	return fmt.Sprintf("wallet:gift:recv:%d:%s", playerID, day)
}

// ── 原語一：一次性宣告 ──────────────────────────────────────────────────────

// ClaimBankruptcyAid 搶下「這個玩家今天的破產救助領取權」。
//
// 回傳 nil 代表搶到了；回傳 ErrAlreadyClaimedToday 代表今天已經領過；
// 其他 error 一律代表 Redis 出問題，呼叫端**必須 fail-closed**（見套件說明）。
//
// ⭐ **為什麼是 `SET key 1 NX PX ttl` 一個指令，而不是 SETNX 之後再 EXPIRE**：
// 拆成兩個指令的話，進程在兩者之間被硬殺（OOMKilled、`docker rm -f`、
// K8s 驅逐）就會留下一個**沒有 TTL 的鎖**——那個玩家**從此再也領不到救助**，
// 而且沒有任何錯誤訊息、沒有任何日誌、要等客訴。
// Java 版在這一支是對的（`setIfAbsent(key, "1", ttl)` 會產生單一 SET 指令），
// 這裡照做；它在 gift 那一支反而沒照做，見套件說明的分歧 #1。
//
// ⚠️ **這不是分散式鎖**，別把它當鎖用：它沒有 owner token，
// 所以任何人都能 DEL 掉它（ReleaseBankruptcyAid 就是這樣做的）。
// 它是「當日資格」——一個**單向、當天不會再變回來**的旗標。
// 真正的分散式鎖需要「解鎖時比對 owner」的 Lua（Java 版的 rank:broadcast:lock
// 是那個形狀），這裡不需要，因為只有領取流程自己會釋放它。
func (s *Store) ClaimBankruptcyAid(ctx context.Context, playerID int64) error {
	now := s.now()
	key := bankruptcyAidKey(playerID, dayStamp(now))

	acquired, err := s.client.SetNX(ctx, key, "1", untilNextMidnight(now)).Result()
	if err != nil {
		return fmt.Errorf("搶救助領取權失敗（key=%s）: %w", key, err)
	}
	if !acquired {
		return ErrAlreadyClaimedToday
	}
	return nil
}

// ReleaseBankruptcyAid 放掉當日領取權，讓玩家可以重試。入帳失敗時呼叫。
//
// ⭐ **它刻意不回傳 error**，這是一個經過考慮的 API 決定：
// 這支只會在「入帳已經失敗、正要把原始錯誤往外拋」的路徑上被呼叫。
// 如果它回傳 error，呼叫端最自然的寫法是 `if err := release(); err != nil { return err }`
// ——而那會**用清理失敗蓋掉真正的失敗原因**，把「餘額不足」變成「Redis 連線逾時」。
// 回傳 error 卻要求呼叫端忽略它，比不回傳更糟：那是一個會被 linter 逼著處理、
// 然後被錯誤地處理的 error。
//
// 清理失敗的實際代價也確實很小：key 帶著午夜 TTL，最壞情況是玩家今天領不到、
// 明天自動恢復。這與 Java 的 `releaseClaim`（catch 住只記 WARN）一致。
//
// ⚠️ 用 context.WithoutCancel：呼叫端多半是在錯誤處理路徑上，
// 而請求的 ctx 可能已經因為逾時或客戶端斷線而取消了。沿用它會讓清理必定失敗
// ——這與 main.go 優雅關機、Kafka commit offset 是同一個坑（地雷 #20）。
func (s *Store) ReleaseBankruptcyAid(ctx context.Context, playerID int64) {
	key := bankruptcyAidKey(playerID, dayStamp(s.now()))

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	if err := s.client.Del(cleanupCtx, key).Err(); err != nil {
		s.logger.Warn("釋放破產救助當日鎖失敗（玩家今天將領不到，明天午夜自動恢復）",
			"key", key, "playerID", playerID, "err", err)
	}
}

// cleanupTimeout 是清理路徑的預算。短是刻意的：清理失敗的代價只是「等到明天」，
// 不值得讓一個已經失敗的請求再多等。
const cleanupTimeout = 2 * time.Second

// ── 原語二：可回補的額度預扣 ────────────────────────────────────────────────

// reserveGiftScript 預扣贈出與收受兩邊的當日額度。
//
// ⭐ **為什麼一定要是 Lua**，而不是幾個 Go 的 Redis 呼叫：
// 這段邏輯有三個地方需要原子性，而三個在 Java 版都各出過一次問題
// （見套件說明的分歧 #1/#2/#3）：
//
//  1. `INCRBY` 與「設 TTL」之間不能有縫。有縫就會留下永不過期的 key。
//  2. 兩個 key 要一起成功。分開做的話，第二個失敗時第一個的預扣會卡到午夜。
//  3. 「超限就回補」必須與 INCRBY 在同一個原子單位裡，否則回補之前的那一瞬間
//     別人讀到的是超限狀態。
//
// Redis 的 Lua 是**單執行緒逐一執行**的，腳本執行期間沒有別的指令能插進來——
// 所以整段是一個原子單位。這是 Redis 提供「多指令原子性」的標準手段，
// 也是 `docs/notes/Java版-Redis-用途全解.md` §4 原則三講的「原子性三個層級」
// 的最上層（單指令 → MULTI → Lua）。
//
// ⚠️ **不在腳本裡呼叫 `redis.call('TIME')`**：TTL 由外面算好後當參數傳進來。
// 理由是可測試性（測試要能把時鐘釘死）與可預測性——腳本只做它被交代的事，
// 不自己看時間。
//
// ⚠️ 這裡用 `PTTL < 0` 判斷「還沒設過 TTL」：`PTTL` 對「存在但無 TTL」回 -1、
// 對「不存在」回 -2。INCRBY 之後 key 必定存在，所以 -1 是唯一會遇到的負值，
// 但寫成 `< 0` 讓它對兩種情況都正確。
//
// ⚠️ 多 key 腳本在 **Redis Cluster** 上要求所有 key 落在同一個 slot，
// 否則會被拒絕（CROSSSLOT）。本專案是單節點（deploy/docker-compose.infra.yml），
// 所以不需要 hash tag。真的要上 Cluster 時，解法是把 key 寫成
// `wallet:gift:sent:{<playerId>}:...` 用大括號指定 slot 依據——
// 但那會讓 sent 與 recv 因為 playerId 不同而落在不同 slot，
// **屆時必須改成兩次獨立呼叫 + 顯式補償**，不是加個大括號就好。記在這裡以免將來誤判。
var reserveGiftScript = redis.NewScript(`
local amount = tonumber(ARGV[1])
local ttl    = tonumber(ARGV[4])

local sent = redis.call('INCRBY', KEYS[1], amount)
if redis.call('PTTL', KEYS[1]) < 0 then
    redis.call('PEXPIRE', KEYS[1], ttl)
end

local recv = redis.call('INCRBY', KEYS[2], amount)
if redis.call('PTTL', KEYS[2]) < 0 then
    redis.call('PEXPIRE', KEYS[2], ttl)
end

if sent > tonumber(ARGV[2]) then
    redis.call('DECRBY', KEYS[1], amount)
    redis.call('DECRBY', KEYS[2], amount)
    return 1
end

if recv > tonumber(ARGV[3]) then
    redis.call('DECRBY', KEYS[1], amount)
    redis.call('DECRBY', KEYS[2], amount)
    return 2
end

return 0
`)

// releaseGiftScript 回補預扣。轉帳失敗時呼叫。
//
// ⭐ **這段修掉 Java 版一個沒有錯誤訊息的資料錯誤**（套件說明的分歧 #2）：
// `DECRBY` 打在**不存在**的 key 上，Redis 會把它當成 0 然後**建立**這個 key、
// 值為負數、**而且沒有 TTL**。Java 的 `releaseDailyQuota` 正是裸的 DECRBY，
// 而它的回補路徑不會補設 TTL。
//
// 觸發條件只要「預扣與回補之間跨過午夜」——轉帳花了幾百毫秒、剛好在 23:59:59.9
// 進來就會踩到。後果是那個玩家的 `wallet:gift:sent` 從此是 `-amount` 且永不過期，
// 於是他每天的實際額度變成 `5000 + amount`，**永久**。
//
// 這裡的處置：
//   - `EXISTS` 檢查：key 不在就什麼都不做（它已經過期＝額度本來就歸零了，
//     不需要回補）。
//   - 夾在 0：即使有其他路徑造成負數，也不讓它留下來。
//     用 `SET ... KEEPTTL`（Redis 6.0+）保住原本的到期時間——
//     普通的 SET 會**清掉 TTL**，那樣就自己製造出分歧 #1 的問題了。
//
// ⚠️ 回補**不是**「把 INCRBY 反做」那麼單純，這是 Redis 計數器最容易錯的地方：
// INCRBY 是無條件的，DECRBY 也是無條件的，但兩者之間 key 可能已經不存在。
// **任何「先加後減」的 Redis 額度都要處理這個窗口。**
var releaseGiftScript = redis.NewScript(`
local amount = tonumber(ARGV[1])

for i = 1, #KEYS do
    if redis.call('EXISTS', KEYS[i]) == 1 then
        if redis.call('DECRBY', KEYS[i], amount) < 0 then
            redis.call('SET', KEYS[i], 0, 'KEEPTTL')
        end
    end
end

return 1
`)

// ReserveGiftQuota 預扣贈出方與收受方的當日額度。
//
// 回傳 nil 代表兩邊都在額度內、且已經扣好；
// ErrGiftSentLimitExceeded / ErrGiftRecvLimitExceeded 代表超限（**已經回補完畢**，
// 呼叫端不需要、也不應該再呼叫 Release）；
// 其他 error 代表 Redis 出問題，呼叫端必須 fail-closed。
//
// ⚠️ 檢查順序是「先 sent 後 recv」，對齊 Java（`sentExceeded` 先判）。
// 兩邊同時超限時回哪一個是對外可觀測的行為，不可以憑直覺換。
//
// ⚠️ sent 與 recv 用的是**不同前綴**，所以即使 senderID == receiverID
// 也不會是同一個 key，Lua 不會重複計數。自己送給自己是 gift service 的業務驗證
// （Java 在 Step 1 擋，回 400），不屬於額度層——這一層保持只做額度。
func (s *Store) ReserveGiftQuota(ctx context.Context, senderID, receiverID int64, amount domain.Amount) error {
	if amount <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidAmount, amount)
	}

	now := s.now()
	day := dayStamp(now)
	keys := []string{giftSentKey(senderID, day), giftRecvKey(receiverID, day)}
	args := []any{
		int64(amount),
		int64(DailyGiftSentLimit),
		int64(DailyGiftRecvLimit),
		untilNextMidnight(now).Milliseconds(),
	}

	outcome, err := reserveGiftScript.Run(ctx, s.client, keys, args...).Int()
	if err != nil {
		return fmt.Errorf("預扣當日贈禮額度失敗（sender=%d receiver=%d）: %w", senderID, receiverID, err)
	}

	switch outcome {
	case 0:
		return nil
	case 1:
		return ErrGiftSentLimitExceeded
	case 2:
		return ErrGiftRecvLimitExceeded
	default:
		// 走到這裡代表 Lua 改過而 Go 沒跟上。回錯誤而不是當成成功——
		// 當成成功等於額度層靜靜失效，那是這個套件最不能有的失敗方向。
		return fmt.Errorf("預扣當日贈禮額度回傳未知結果 %d（Lua 與 Go 不同步？）", outcome)
	}
}

// ReleaseGiftQuota 回補先前的預扣。轉帳失敗時呼叫。
//
// 與 ReleaseBankruptcyAid 同理，**刻意不回傳 error**：它只出現在錯誤處理路徑上，
// 回傳 error 會誘導呼叫端用清理失敗蓋掉真正的失敗原因。
//
// 回補失敗的代價是「這筆額度被多算了，直到午夜」——方向是**更嚴格**，
// 不會讓玩家超額。這與 Java 的判斷一致（GiftService 的 WARN 註解）。
func (s *Store) ReleaseGiftQuota(ctx context.Context, senderID, receiverID int64, amount domain.Amount) {
	if amount <= 0 {
		// 不記 log：這只可能來自呼叫端的程式錯誤，而 Reserve 那邊已經擋掉了。
		return
	}

	day := dayStamp(s.now())
	keys := []string{giftSentKey(senderID, day), giftRecvKey(receiverID, day)}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	if err := releaseGiftScript.Run(cleanupCtx, s.client, keys, int64(amount)).Err(); err != nil {
		s.logger.Warn("回補當日贈禮額度失敗（額度會偏嚴格，午夜自動歸零）",
			"sender", senderID, "receiver", receiverID, "amount", int64(amount), "err", err)
	}
}
