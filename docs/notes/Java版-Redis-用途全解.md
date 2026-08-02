# 幸運星幣城（Java 版）— Redis 用途全解 筆記

> 日期：2026-07-29
> 來源：**團隊 Java repo** `H:\Lucky_Star_Casino`（develop 分支，程式碼實地查證，唯讀參考）
> 依據：該 repo 的 `docs/adr/ADR-008.md`（捕魚 session Lua CAS）、其 `AGENTS.md` 雷區 16 / 24 / 31
> 搬進本 repo：2026-08-01

> ⚠️ **這份筆記描述的是團隊 Java 版的現況**，但它的**可轉移率遠高於 CQRS 那份**——
> 因為 Redis 的 key 命名、TTL 設計、Lua 腳本**與語言無關**，Go 版要照搬。
>
> | 本文（Java 版） | 本專案（Go 版） |
> |---|---|
> | Redis 6379 | **Redis 7**（6380，埠錯開見地雷 #28） |
> | 「帳務由 PostgreSQL 守」 | 帳務真相在 **MySQL**（`docs/ADR-001`） |
> | `redisTemplate` / Lettuce | `redis/go-redis/v9` |
> | Lua CAS 腳本（`FishingSessionStore:49`） | ⭐ **原樣搬過去**，這是語言無關的資產 |
> | 團隊 `AGENTS.md` 雷區 8 / 16 / 24 / 31 / 32 | 本 repo `AGENTS.md` §2 **#3 / #11 / #6 / #15 / #16** |
>
> ✅ **本文 §6 講的「已修」是指團隊 repo**，但本 repo 的
> `deploy/docker-compose.infra.yml` **從第一天就是修好的狀態**：
> `command: ["redis-server", "--appendonly", "yes"]` + named volume `casino_go_redis_data`。
> 這就是「繼承地雷」的具體樣子——團隊踩了才補的東西，這邊直接當預設。
> ⚠️ 但 `down -v` 照樣會清空，AOF 解決的是「容器重啟」不是「人為刪 volume」（地雷 #16）。

---

## 0. 先修正一個前提：Redis 在本專案不只是「快取」

一般教材講 Redis 都說「快取層，資料庫前面擋一層」。本專案**只有一處是純快取**（風控 RTP），
其餘全都是 **Redis 才做得到的事**：

| 角色 | 為什麼非 Redis 不可 | 本專案案例 |
|---|---|---|
| ① **憑證撤銷狀態** | 需跨 4 個服務共用、毫秒級生效、自動過期 | JWT 黑名單、停用玩家封鎖 |
| ② **短時效 Session** | 30 分鐘後自動消失，不該污染 DB | 對局 session、捕魚場次 |
| ③ **排行榜（主儲存）** | ZSET 的 O(log N) 排序，SQL `ORDER BY` 做不到即時 | 全球/好友/單日/單遊戲排行 |
| ④ **計數器 + 分散式鎖** | 原子遞增、TTL 自動歸零，DB 做會鎖表 | 限流、風控並發閘、當日額度 |
| ⑤ **快取（唯一一處）** | 熱路徑不重算 DB 聚合 | `risk:rtp:{gameType}` |

**關鍵認知**：③ 排行榜是 Redis 當**主儲存**，不是快取 —— 資料只存在 Redis 裡（DB 只有每日快照）。
這直接影響到 §6 的風險，先記住。

---

## 1. 用到的資料結構（只講本專案有的 4 種）

| 型別 | Redis 指令 | 用在哪 |
|---|---|---|
| **String** | `SET` / `GET` / `INCR` / `SETNX` | 標記、票券、計數器、鎖 |
| **Hash** | `HSET` / `HGETALL` / `HINCRBY` | Session（每欄位一個 field）、玩家暱稱表、統計累加 |
| **ZSet**（排序集合） | `ZADD` / `ZINCRBY` / `ZREVRANGE` | 所有排行榜 |
| **Lua Script** | `EVAL` | 需要「多指令原子完成」的地方（CAS、並發閘） |

**Hash vs 一堆 String 的差別**：Session 用 Hash 存，`redis-cli HGETALL` 就能人眼看懂每個欄位，
且能只改動一個欄位（`GameSessionService` 結算時只更新 state/seed，不重寫整筆）。
若序列化成一個 JSON String，每次改都得整包讀出改完寫回。

---

## 2. 全部 key 清單（我逐檔查證過的完整 inventory）

### ① 認證 / 授權（4 個服務共用，key 名稱不可改）

| Key | 型別 | TTL | 寫入方 | 讀取方 | 用途 |
|---|---|---|---|---|---|
| `refresh:{memberId}` | String | = refresh token 壽命 | member `TokenRedisService:24` | member | 存 refresh token；停用時被 admin 刪除 |
| `jwt:blacklist:{jti}` | String | = 該 token 剩餘壽命 | member 登出 | **gateway** `JwtAuthenticationGlobalFilter:43` | 登出後 token 立即失效 |
| `disabled:player:{id}` | String | **無 TTL** | admin `PlayerBanService:44` | gateway + member | 後台停用玩家 → 既有 token 立刻 401、也不能重登 |
| `token:min-iat:{id}` | String | 7 天 | admin `PlayerBanService:45` | gateway | 拒絕 `iat` 早於此值的 token（防「啟用後舊 token 復活」） |
| `oauth:binding-ticket:{uuid}` | String | 短期 | member `SocialAuthService:38` | member | 第三方登入綁定握手 |
| `oauth:login-ticket:{uuid}` | String | 短期 | member `SocialAuthService:39` | member | OAuth 成功 → 前端換 JWT（`getAndDelete`，一次性） |
| `oauth:registration-ticket:{uuid}` | String | 短期 | member `SocialAuthService:40` | member | 第三方首次登入 → 補資料註冊 |

### ② Session（短時效狀態）

| Key | 型別 | TTL | 檔案 | 用途 |
|---|---|---|---|---|
| `game:session:{playerId}:{roundId}` | Hash | 30 分鐘 | `GameSessionService:33` | Provably Fair 對局狀態，結算後保留 30 分鐘供玩家驗證 seed |
| `game:fishing:session:{playerId}` | Hash | 24 小時 | `FishingSessionStore:41` | 捕魚場次（跨批累傷 `fishDamage`、`betPerShot`、`version`） |

### ③ 排行榜（Redis 為主儲存）

| Key | 型別 | TTL | 用途 |
|---|---|---|---|
| `rank:global:coins` | ZSet | 無 | 全球星幣排行（Top 100） |
| `rank:friend:{playerId}` | ZSet | 24 小時 | 好友排行（Top 20） |
| `rank:daily:winnings` | ZSet | 無（排程重置） | 單日贏分榜 |
| `rank:game:{category}:*` | ZSet + Hash | 無 | 各遊戲淨利／局數／總注／總派彩／勝場 |
| `rank:player:usernames` / `nicknames` / `avatars` / `joined-at` | Hash | 無 | 玩家展示資料（避免每次跨服務查 member） |
| `rank:broadcast:lock` | String | 3 秒 | 分散式鎖：Top10 廣播限流（最短間隔 1 秒） |
| `rank:dedup:game:{roundId}` | String | 7 天 | 事件去重（`game.result`） |
| `rank:dedup:daily-win:{txId}` | String | 7 天 | 事件去重（`ZINCRBY` 累加專用） |

### ④ 限流 / 風控 / 額度

| Key | 型別 | TTL | 檔案 | 用途 |
|---|---|---|---|---|
| `rate:player:{userId}` | String | 1 秒 | gateway `PlayerRateLimitGlobalFilter:74` | 一般路徑每秒上限 |
| `rate:game:{userId}` | String | 1 秒 | 同上 | 遊戲路徑（較嚴格） |
| `risk:inflight:{playerId}` | String | 30 秒 | `RiskControlService:115` | 並發閘：同玩家同時兩個請求 → 保守攔截 |
| `risk:rtp:{gameType}` | String | 10 秒 | `RiskControlService:62` | **唯一的純快取**：全局 RTP，排程每 2 秒重算 |
| `risk:player-day:{id}:{yyyyMMdd}:{game}` | Hash | 48 小時 | `RiskControlService:277` | 玩家今日水位（`HINCRBY` 累加） |
| `admin:betcount:{...}` | String | 30 分鐘 | `AlertRuleEngine:35` | 異常偵測：高頻下注 |
| `admin:txncount:{...}` | String | 60 秒 | `AlertRuleEngine:39` | 異常偵測：交易頻率 |
| `wallet:gift:sent:{id}:{date}` | String | 當日 | `GiftService:82` | 贈禮當日額度（送出方） |
| `wallet:gift:recv:{id}:{date}` | String | 當日 | `GiftService:83` | 贈禮當日額度（接收方） |
| `wallet:bankruptcy-aid:{id}:{date}` | String | 到午夜 | `BankruptcyAidService:84` | 破產救助每日一次鎖 |

---

## 3. 五個核心用法逐一拆解

### 3.1 JWT 撤銷三件套 — 為什麼一個「停用玩家」要寫三個 key？

這是全專案最精緻的 Redis 設計，值得逐步看。`PlayerBanService.ban()` 做三件事：

```java
// PlayerBanService:43-50
redisTemplate.opsForValue().set(disabledKey(playerId), "1");           // ① 即時封鎖
redisTemplate.opsForValue().set(minIatKey(playerId), now, 7天);        // ② 時間門檻
redisTemplate.delete(REFRESH_KEY_PREFIX + playerId);                   // ③ 作廢 refresh
```

**為什麼一個不夠？** 因為 JWT 是**無狀態**的 —— 一旦簽發出去，伺服器沒有辦法「收回」它，
它就是有效到過期為止。所以要撤銷必須在外面加一層黑名單。三個 key 各補一個漏洞：

| 沒有它會怎樣 | 對應的 key |
|---|---|
| 停用了，但玩家手上的 token 還沒過期 → 照樣能玩 | ① `disabled:player:` |
| 啟用後刪掉封鎖 key → **停用前簽發的舊 token 復活** | ② `token:min-iat:` |
| 玩家拿停用前的 refresh token 去 `/auth/refresh` 換一張 iat 較新的 access token → **繞過 ②** | ③ 刪 `refresh:` |

`unban()` 刻意**只刪 ①，不刪 ②**：留著時間門檻，讓停用前那些 token 永久失效，
靠 7 天 TTL（refresh token 最長壽命）自然清除。

#### ⚠️ 這裡的雷：key 名稱是三個服務之間的隱含契約

```
admin 寫入  disabled:player:{id}
              ↓ 同一個 Redis
gateway 讀取 disabled:player:{sub}     ← 字串差一個字，撤銷就完全失效
member 讀取  disabled:player:{id}
```

程式碼裡三處都留了註解警告（`TokenRedisService:14-19`、`PlayerBanService` 類別註解）。
**這種靠字串巧合成立的耦合，編譯器完全抓不到** —— 打錯字不會報錯，只會安靜地不生效。

### 3.2 排行榜 ZSET — `ZADD` 與 `ZINCRBY` 的冪等判準

這條直通 `AGENTS.md` 雷區 24，是本專案最容易寫錯的地方。同一個 `RankService` 裡：

```java
// RankService:71 — 餘額排行：寫「絕對值」
redisTemplate.opsForZSet().add(GLOBAL_COINS_KEY, playerId, currentCoins);

// RankService:81 — 單日贏分：做「累加」
redisTemplate.opsForZSet().incrementScore(DAILY_WINNINGS_KEY, playerId, amount);
```

Kafka 是 **at-least-once**（同一則事件會重送），所以兩者面對重送的命運完全不同：

| | `ZADD`（絕對值） | `ZINCRBY`（累加） |
|---|---|---|
| 重送同一則事件 | 寫入同樣的值 → **無害** | 分數被加兩次 → **錯誤** |
| 需要去重嗎 | **不要**（見下） | **需要**（`rank:dedup:daily-win:{txId}`） |

**為什麼冪等操作「加去重反而有害」？** 想像這個時序：

```
事件到達 → ZADD 寫入成功 → 【進程崩潰，還沒 ack】
                              ↓
Kafka 重送 → 去重擋掉（因為 SETNX 已存在）→ 值永久停在錯的中間態
```

去重標記寫進去了、實際工作卻沒完成，這是**去重機制自己製造的資料錯誤**。
冪等操作要的是「可以安全重放」，加鎖只會擋住修復的機會。

> 判準一句話：**問「這個操作重做一次會不會出錯？」不會 → 不要去重；會 → 才去重。**

### 3.3 捕魚 Session 的 Lua CAS（ADR-008）— 讀改寫為什麼一定要版本號

捕魚每批 `shots()` 的流程是 **讀 → 改 → 整包寫回**：

```
① find()  讀出 session（fishDamage、sessionBalance…）
② 在記憶體算傷害、扣款、判定捕獲
③ save()  整包寫回 Redis
```

若同一玩家兩個請求同時進來（開火 + 場中加值），會發生**丟失更新（lost update）**：

```
請求A: 讀(version=5) ──算───────────── 寫回 ← A 的結果覆蓋掉 B
請求B:      讀(version=5) ──算── 寫回      ← B 的加值消失了
```

解法是 Lua script 做 CAS（Compare-And-Swap）：

```lua
-- FishingSessionStore:49
local current = redis.call('HGET', KEYS[1], 'version')
if current ~= ARGV[1] then return 0 end       -- 版本不符 → 拒絕寫入
for i = 3, #ARGV, 2 do
    redis.call('HSET', KEYS[1], ARGV[i], ARGV[i+1])
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
```

**為什麼一定要 Lua，不能用 Java 寫 if？** 因為 Java 版是「先 `HGET` 比對、再 `HSET` 寫入」
的**兩次往返**，兩次之間別人可以插隊 —— 檢查本身就不原子，等於沒檢查。
Redis 執行 Lua script 時是**單執行緒不可中斷**的，所以整段變成一個原子操作。

CAS 失敗（回 0）時，`FishingService` 必須 **重讀 → 重放 → 重存**（上限 3 次）。
**絕不可以沿用舊快照的 session 物件繼續算** —— 那等於繞過 CAS，把問題原封不動搬回來。

### 3.4 限流 — 它其實是「固定視窗計數器」，不是 token bucket

```java
// PlayerRateLimitGlobalFilter:76-86
redis.opsForValue().increment(redisKey)      // INCR
     .flatMap(count -> {
         if (count == 1L) redis.expire(redisKey, Duration.ofSeconds(1));  // 第一次才設 TTL
         if (count > burstCapacity) return reject429(exchange);
         return chain.filter(exchange);
     })
```

**原理**：key 的 TTL 是 1 秒。第一個請求把計數器設為 1 並開啟 1 秒視窗，
之後每個請求 `INCR`，超過上限就 429。1 秒後 key 過期消失，計數自然歸零 ——
**不需要任何清理排程，TTL 就是清理機制**。

> 📌 **命名修正（已回寫專案）**：`AGENTS.md` 雷區 31 原本把這個機制寫成「token bucket」，
> 但實作是**固定視窗計數器（fixed window counter）** —— 2026-07-29 已更正。
> 差別在邊界：固定視窗允許「視窗尾 + 下個視窗頭」瞬間吃到 2 倍流量（經典的 burst 邊界問題），
> token bucket 則是連續補充、不存在這個破口。對本專案影響不大（1 秒視窗很短），但名詞要正確。
>
> 附帶：前端 `useFishingSession.js:62,147-155` 的開火限速**才是真的 token bucket**
> （有 `tokens` 存量與 `SHOTS_PER_SEC` 補充速率），該處敘述無誤。同一份文件裡兩個名詞
> 一對一錯，正好是對照學習的好例子。

### 3.5 故障策略 — fail-open 還是 fail-closed？判準是「放行的代價」

Redis 掛掉時，同樣是「查不到資料」，兩個 filter 的選擇完全相反：

| 元件 | Redis 故障時 | 為什麼 |
|---|---|---|
| **限流**（`PlayerRateLimitGlobalFilter:88`） | **fail-open** 放行 | 限流是保護機制。它壞了就放行，總比「限流元件自己造成全站 503」好 |
| **JWT 撤銷**（`JwtAuthenticationGlobalFilter:132`） | **fail-closed** 拒絕（401） | 放行等於**已撤銷的 token 復活**、被停用的玩家能重新進場 —— 這是安全漏洞 |
| **風控並發閘**（`RiskControlService:118`） | 降級直查 DB | 保守處理，行為退回舊版 |
| **風控日水位**（`recordRoundSettled`） | best-effort，只記 log | 寧可水位少計一局（下次 cache miss 由 DB 回填），**不可因 Redis 故障讓結算失敗** |

> 判準一句話：**問「這個檢查失效會導致什麼？」損失便利性 → fail-open；損失安全性/正確性 → fail-closed。**

### 3.6 分散式鎖 — `SETNX + TTL` 必須是一個指令

破產救助（每日一次）的鎖：

```java
// BankruptcyAidService:84-86
Boolean acquired = redisTemplate.opsForValue()
        .setIfAbsent(claimKey, "1", ttl);   // SET key 1 NX PX(到午夜) —— 單一原子指令
```

註解裡寫得很清楚為什麼不能拆成兩步：

```java
// ❌ 危險寫法
setIfAbsent(key, "1");        // 搶到鎖
// ← 若進程在這一瞬間被硬殺（kill -9 / 容器 OOM）
expire(key, ttl);             // TTL 沒設上 → 鎖永久殘留 → 該玩家「當天再也領不了」
```

**分散式鎖的鐵則：取鎖與設定過期時間必須是同一個原子指令。**
Redis 的 `SET key value NX PX ms` 就是為此而生。

同一個檔案還示範了「取鎖後失敗要回補」：入帳失敗 → `releaseClaim(claimKey)` 讓玩家能重試（`:100`）。
`GiftService` 的當日額度也是同樣模式：`INCRBY` 預扣 → 轉帳失敗 → `releaseDailyQuota` 回補（`:88,95`）。

---

## 4. 貫穿全局的三個設計原則

### 原則一：TTL 是設計的一部分，不是附加選項

看這幾個 key 的設計，每個 TTL 都在解決一個具體問題：

| TTL 設計 | 解決什麼 |
|---|---|
| `rate:*` = 1 秒 | 視窗歸零不需排程，TTL 自己做 |
| `risk:player-day:{id}:{yyyyMMdd}:{game}` = 48 小時 | **日期寫進 key** → 跨日天然歸零，不需「每日清零」排程 |
| `risk:inflight:*` = 30 秒 | 安全網：程式崩潰忘記 `releaseRiskSlot` 也會自動釋放 |
| `game:fishing:session:*` = 24 小時 | 安全網：閒置回收排程掛掉時防 Redis 堆積 |
| `token:min-iat:*` = 7 天 | 剛好等於 refresh token 最長壽命，過後舊 token 必已過期 |

**「日期入 key」這招要學會** —— 它把「定時清零」這個需要排程、可能失敗的工作，
變成「不同的日子用不同的 key」這個不可能出錯的事實。

### 原則二：原子性有三個層級，按需要往上升

```
① 單一 Redis 指令        → 本來就原子（INCR、ZADD、SET NX PX）
② 需要「檢查後才動作」    → 必須用 Lua script（CAS、並發閘的 INCR+首次PEXPIRE）
③ 需要跨 Redis 與 DB     → Redis 只能做預扣 + 失敗回補（gift 額度、破產救助）
```

③ 沒有真正的原子性可言（Redis 與 PostgreSQL 沒有共同交易邊界，同 CQRS 筆記裡的雙寫問題），
所以採「先預扣 Redis → 動 DB → 失敗就回補 Redis」，並讓 DB 的 UNIQUE 約束當最後防線。

### 原則三：Redis 是輔助，帳務正確性永遠靠 DB

`RiskControlService` 的註解說得最直白：

> 全局 RTP 是「統計性水位警報」而非帳務正確性機制 ——
> **帳務由 wallet 冪等鍵＋樂觀鎖守**（雷區 8）

破產救助也是雙保險：Redis 鎖是第一道（快速擋掉），DB 的 `idempotency_key` UNIQUE 是第二道；
Redis 若被清空，`credit.isIdempotent()` 仍會攔住重複領取（`BankruptcyAidService:104`）。

**設計哲學**：Redis 掛掉/被清空，遊戲體驗會退化（排行榜歸零、限流失效），但**帳不會錯**。

---

## 5. 一句話總結

| 概念 | 一句話 |
|---|---|
| **Redis 的四種角色** | 撤銷狀態、短時效 session、排行榜主儲存、計數器與鎖 —— 只有 RTP 一處是純快取 |
| **JWT 撤銷三件套** | JWT 無狀態收不回來，所以外掛黑名單；三個 key 分別補「即時性」「舊 token 復活」「refresh 繞過」三個漏洞 |
| **ZADD vs ZINCRBY** | 操作本身冪等 → 不要去重（加了會卡在錯的中間態）；非冪等累加 → 才用 SETNX 去重 |
| **Lua CAS** | 「檢查後寫入」在 Java 端做是兩次往返、不原子；Lua 單執行緒不可中斷才是真原子 |
| **TTL 是設計** | 日期入 key ＝ 用「不同 key」取代「定時清零排程」，不可能失敗 |
| **fail-open / fail-closed** | 失效損失便利性 → 放行；失效損失安全性 → 拒絕 |
| **分散式鎖鐵則** | 取鎖與設 TTL 必須同一原子指令，否則進程被硬殺會留下永久殘鎖 |

---

## 6. 持久化：原本的缺口與現在的狀態

### ✅ 已修（2026-07-29）：Redis 容器原本沒掛 volume

原本 `docker-compose.yml` 的 redis 服務**是六個有狀態服務中唯一沒掛 volume 的**
（mysql / postgres / kafka / prometheus / grafana 都有）。`redis:7` 預設做 RDB 快照，
但寫在容器內部的 `/data` —— 容器一重建就消失。

現已改為：

```yaml
redis:
  image: redis:7
  command: ["redis-server", "--appendonly", "yes"]   # AOF 持久化
  volumes:
    - lucky_redis_data:/data                          # named volume
```

**AOF（Append Only File）與 RDB 的差別**，順便學：

| | RDB（快照） | AOF（附加日誌） |
|---|---|---|
| 機制 | 定時把整個記憶體 dump 成一個檔 | 每個寫入指令追加到日誌檔 |
| 崩潰損失 | 上次快照後的**全部**寫入 | 預設 `everysec` → 最多 1 秒 |
| 檔案大小 | 小（壓縮過的資料） | 大（會 rewrite 壓縮） |
| 恢復速度 | 快 | 慢（要重放指令） |

本專案選 AOF：排行榜與停用標記寧可慢一點恢復，也不要掉 5 分鐘的資料。

> ⚠️ **但 `docker compose down -v` 仍會清空**（`-v` 就是刪 volume）。AOF 解決的是
> 「容器重啟」，不是「人為刪 volume」。

### 各類資料被清空的影響分級（`down -v` / `FLUSHDB` 時仍適用）

| 資料 | 掉了會怎樣 | 有重建路徑嗎 |
|---|---|---|
| `rank:daily:winnings`、`rank:global:coins` | 排行榜歸零 | ✅ **有** —— `tools/reconciliation/rebuild-rank-redis.mjs`（藍圖 04 P4）從 PostgreSQL 重算，含 `--dry-run` |
| `rank:game:*`、`rank:player:*`、`rank:friend:*` | 各遊戲統計/暱稱/好友榜歸零 | ❌ **沒有** —— 只能靠新事件慢慢補 |
| `disabled:player:*` | 🔴 **安全問題**：被停用的玩家全部自動解封 | 🔶 真相在 `members.status`（T-051），但**沒有開機回填程式** |
| `jwt:blacklist:*` | 已登出的 token 復活（到自然過期為止） | ❌ 不需要（TTL 短） |
| `game:fishing:session:*` | 進行中的捕魚場次消失，局內餘額結算不回去 | ❌ 不需要 |
| `rate:*`、`risk:*` | 🟢 無妨 | ❌ 不需要（本來就短 TTL） |

> 📝 **我在第一版筆記裡把「排行榜沒有重建程式」寫成全面成立，這是錯的** ——
> `rebuild-rank-redis.mjs` 涵蓋了兩個最關鍵的 ZSET。正確的說法是「涵蓋範圍有限」，
> 如上表。**這也是 `AGENTS.md` 開頭那條規則的實例：查進度不要只信文件，要拿程式碼交叉驗證。**

### 順帶修掉的：outbox 表無限成長（CQRS 筆記提到的那個）

`WalletOutboxPoller` 投遞成功只把 status 標 SENT、從不刪除，表單向成長。
已補 `WalletOutboxPurgeJob`（member 為 `OutboxPurgeJob`）每日 04:00 清理保留期外的 SENT 列，
預設保留 7 天。**只刪 SENT** —— PENDING 無論多舊都不能刪，刪掉就是無聲丟失事件。

### 🔶 仍未處理（需要時再做）

1. `disabled:player:` 的開機回填（從 `members.status` 重建）
2. `rank:game:*` / `rank:player:*` 的重建路徑

---

## 相關檔案索引

| 檔案 | 內容 |
|---|---|
| `docs/adr/ADR-008.md` | 捕魚 session Redis 原子化（Lua CAS）決策 |
| `member-service/.../service/TokenRedisService.java` | refresh token / JWT 黑名單 |
| `admin-service/.../service/PlayerBanService.java` | 停用玩家三件套（含最完整的設計註解） |
| `gateway-service/.../filter/JwtAuthenticationGlobalFilter.java` | 撤銷檢查，**fail-closed** |
| `gateway-service/.../filter/PlayerRateLimitGlobalFilter.java` | 固定視窗限流，**fail-open** |
| `rank-service/.../service/RankService.java` | 全部排行榜 ZSET 操作 |
| `rank-service/.../kafka/GameResultConsumer.java` | 事件去重（`rank:dedup:game:`） |
| `game-service/.../fishing/FishingSessionStore.java` | Lua CAS script（`:49`） |
| `game-service/.../service/RiskControlService.java` | 並發閘 Lua、RTP 快取、日水位 HINCRBY |
| `game-service/.../session/GameSessionService.java` | 對局 session Hash，30 分鐘 TTL |
| `wallet-service/.../service/BankruptcyAidService.java` | `SETNX + TTL` 單指令分散式鎖 |
| `wallet-service/.../service/GiftService.java` | 當日額度預扣 / 失敗回補 |
| `AGENTS.md` 雷區 16 / 24 / 31 | Session 序列化、去重判準、限流分層 |
