// Package redistest 提供 infra 測試共用的 Redis 輔助工具。
//
// 形狀刻意與 internal/platform/mysqltest 一致（那份的 doc comment 解釋了
// 「測試支援程式碼是正式程式碼」的理由），但**隔離策略完全不同**，
// 差別的來源是 AGENTS.md 地雷 #16：**Redis 在本專案是主儲存不是快取**。
//
// mysqltest 的作法是「建一個臨時資料庫、用完 DROP 掉」。Redis 沒有等價物：
// 它的 16 個 DB 是固定的，不能動態建立，而清空的手段（FLUSHDB / FLUSHALL）
// 是**不可逆的破壞性指令**。在這個專案裡按錯一次的代價是砍掉排行榜——
// 而排行榜沒有回填程式（地雷 #16）。
//
// 所以這裡用**兩層防護**，而且刻意不提供任何 flush 函式：
//
//  1. **DB 15**：測試只連最後一個 DB，業務資料一律在 DB 0。連錯的話
//     什麼都找不到，會立刻失敗而不是靜靜地污染資料。
//  2. **唯一玩家 ID**：每個測試拿到不重複的 playerID，所以 key 天生不會撞，
//     不需要「開始前先清空」。清理走 SCAN + 針對性 DEL，只刪自己造出來的東西。
//
// ⚠️ 第 2 層才是真正在做隔離的那一層。第 1 層只是防手滑。
// 少了第 2 層，兩個平行執行的測試套件會互相踩；而**加上第 2 層之後，
// 就沒有任何理由需要 FLUSHDB 了**——這正是「不提供危險工具」的作法：
// 沒有那個函式，就不會有人在半夜複製貼上它。
package redistest

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

// scratchDB 是測試專用的 Redis DB 編號。
//
// Redis 預設有 16 個 DB（0–15），業務一律用 0（go-redis 的預設值）。
// 選最後一個是為了讓「測試連到業務 DB」這種錯誤盡量顯眼。
//
// ⚠️ 多 DB 是**單節點才有的功能**，Redis Cluster 只有 DB 0。
// 哪天真的上 Cluster，這個隔離手段會整個失效，屆時要改用 key 前綴——
// 而 key 前綴需要正式程式碼配合（目前 key 格式是對齊 Java 的硬性契約，
// 不能為了測試加前綴）。記在這裡，免得將來以為改個 DB 編號就能上 Cluster。
const scratchDB = 15

// connectTimeout 是連上 Redis 的預算。測試環境的 Redis 就在本機，
// 給 5 秒是為了讓「容器還沒起來」這件事很快就報錯，而不是卡住整個測試。
const connectTimeout = 5 * time.Second

// NewClient 連上測試用的 Redis DB，並在測試結束時關閉連線。
//
// ⚠️ 一定要 Ping。go-redis 的 NewClient **不會**立刻連線（與
// platformstore.OpenRedis 的註解同一件事），沒有 Ping 的話「Redis 根本沒起來」
// 會變成第一個指令才失敗，而那個錯誤訊息長得像業務邏輯壞掉。
func NewClient(t *testing.T) *redis.Client {
	t.Helper()

	cfg, err := config.LoadRedis()
	if err != nil {
		t.Fatalf("載入 Redis 設定失敗（是不是忘了 set -a; . deploy/.env; set +a）: %v", err)
	}

	client := redis.NewClient(&redis.Options{Addr: cfg.Addr(), DB: scratchDB})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("關閉 Redis 連線失敗: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("連上 Redis %s（DB %d）失敗——基礎設施起來了嗎？: %v", cfg.Addr(), scratchDB, err)
	}
	return client
}

// playerIDSeq 讓同一次測試執行裡的每次呼叫都拿到不同的值。
// 以 nano 時間戳為基底是為了讓**不同次**執行也不會撞
// （上一輪殘留的 key 若還沒過期，撞到就會看到莫名其妙的初始值）。
var playerIDSeq = func() *atomic.Int64 {
	var seq atomic.Int64
	seq.Store(time.Now().UnixNano())
	return &seq
}()

// UniquePlayerID 回傳一個沒有人用過的玩家 ID，並登記在測試結束時
// 清掉這個玩家的所有 key。
//
// ⭐ 這是本套件的隔離主力。用「唯一 ID」而不是「測試前清空」的理由：
//   - 清空是全域操作，兩個平行執行的測試會互相踩，而症狀是**間歇性失敗**
//     ——最難查的那種。
//   - 唯一 ID 讓每個測試的資料天生不相交，平行執行、重複執行都安全。
//   - 而且它逼著測試**用真實的 key 格式**（wallet:gift:sent:{id}:{date}），
//     於是 key 組裝邏輯本身也被測到了。
//
// ⚠️ client 是顯式參數而不是內部自己開一條，這是踩過才改的：
// 原本的版本在每次 t.Cleanup 裡新建一個 client，於是「一個測試用 10 個玩家 ID」
// 就等於 10 條 TCP 連線 + 10 次設定載入。跑 60 輪的併發實驗時光是這件事
// 就佔掉大半時間。**測試的慢也是慢**——慢到一個程度就會有人不跑它。
func UniquePlayerID(t *testing.T, client *redis.Client) int64 {
	t.Helper()

	id := playerIDSeq.Add(1)
	t.Cleanup(func() {
		// ⚠️ 新開 context：測試結束時原本的 ctx 多半已經取消，沿用會讓清理送不出去
		// 而 key 留在 Redis 裡（同 mysqltest 的 DROP DATABASE，地雷 #20 那一類）。
		ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		defer cancel()
		cleanupPlayer(ctx, t, client, id)
	})
	return id
}

// cleanupPlayer 刪掉某個玩家在測試 DB 裡的所有 key。
//
// ⚠️ 用 SCAN 而不是 KEYS。KEYS 是 **O(N) 且會阻塞整個 Redis**——單執行緒的
// Redis 在掃描期間不處理任何其他指令。測試 DB 資料少、看起來沒差，
// 但「在測試裡養成用 KEYS 的習慣」遲早會被複製到正式程式碼裡。
// SCAN 是游標式的，每次只掃一小批，不會阻塞。
func cleanupPlayer(ctx context.Context, t *testing.T, client *redis.Client, playerID int64) {
	t.Helper()

	// key 的形狀一律是 `<前綴>:<playerID>:<日期>`，所以 `*:<id>:*` 精準涵蓋
	// 這個玩家的全部 key，又不會誤傷別人（ID 兩側都有冒號界定，
	// 不會讓 12 去比對到 123）。
	iter := client.Scan(ctx, 0, "*:"+strconv.FormatInt(playerID, 10)+":*", 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Errorf("掃描玩家 %d 的測試 key 失敗（要手動清）: %v", playerID, err)
		return
	}
	if len(keys) == 0 {
		return
	}
	if err := client.Del(ctx, keys...).Err(); err != nil {
		t.Errorf("刪除玩家 %d 的測試 key 失敗（要手動清）: %v", playerID, err)
	}
}
