# 跨語言黑箱契約測試

**同一份測試碼，對 Java 版 wallet 與 Go 版 wallet 各跑一次，兩邊都必須綠。**

> **目前狀態（2026-08-02）：11 項測試（含 15 個表格子項）**
> **對 Java 版（:8082）與 Go 版（:8182）兩邊全綠。**

這是整個重構唯一的「等價」證據（藍圖 §2 原則 4、§7 的 Phase DoD）。
在它存在之前，「Go 版與 Java 版行為相同」這句話的依據只是
**「我很仔細地讀過 Java 原始碼」**——那不是證據，那是意圖。

---

## 怎麼跑

### 對 Go 版（:8182）

```bash
# ① 基礎設施 + schema
docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
set -a && . deploy/.env && set +a
go run ./cmd/migrate up

# ② 服務（另開一個終端機）
go run ./cmd/wallet

# ③ 測試
CONTRACT_TARGET=go go test -tags=contract -count=1 -v ./test/contract/
```

### 對 Java 版（:8082）

```bash
# ① 起團隊 repo 的 wallet-service（會一併帶起 postgres / mysql / redis / kafka-init）
cd /h/Lucky_Star_Casino && docker compose up -d wallet-service

# ② 測試（環境變數換成團隊 repo 的 .env）
cd /h/Lucky_Star_Casino_Go
set -a && . /h/Lucky_Star_Casino/.env && set +a
CONTRACT_TARGET=java go test -tags=contract -count=1 -v ./test/contract/
```

⚠️ **`CONTRACT_TARGET` 沒有預設值**，這是刻意的：契約測試的整個意義在於
「同一份測試跑了哪一個實作」。讓它有預設值就等於允許
「我以為我測了 Java，其實測的是 Go」——而那種錯誤跑完是綠的。

---

## ⚠️ 跑 Java 目標之前必看：那顆映像是哪一天的？

2026-08-02 第一次跑 java 目標時踩到的，**兩個都與程式碼無關、都沒有錯誤訊息**：

1. **本機的 `lucky_star_casino-wallet-service` 映像停在 2026-07-18**，
   而 Transactional Outbox 是 07-21 才進去的。也就是說跑起來的那個 Java
   **不是現在的原始碼**，拿它當「正確答案」會得到一個過期的答案。

   ```bash
   docker image inspect lucky_star_casino-wallet-service:latest --format '{{.Created}}'
   cd /h/Lucky_Star_Casino && docker compose build wallet-service   # 對不上就重建
   ```

2. **團隊那顆 PostgreSQL volume 缺 `wallet_outbox` 表**——volume 比那張表老，
   而 `/docker-entrypoint-initdb.d` **只在 volume 全新時執行**（地雷 #17 的活體標本）。
   `JPA_DDL_AUTO=validate` 不會幫你建，Hibernate 也不會抱怨沒被 map 到的表。
   缺表時的症狀是**帳務交易在 commit 時炸掉**，而重建映像之前完全看不出來。

   ```bash
   # 補上缺的那張表（DDL 直接取自團隊 repo 的 database/postgres/init.sql）
   docker exec -e PGPASSWORD=<pw> lucky-star-postgres \
     psql -U lucky_user -d lucky_star_casino -c "$(sed -n '/CREATE TABLE IF NOT EXISTS wallet_outbox/,/^);/p' \
       /h/Lucky_Star_Casino/database/postgres/init.sql)"
   ```

**這兩件事本身就是契約測試的第一個產出**：它們證明「參考實作」不是一個
理所當然存在的東西，而是一個需要被確認版本的東西。

---

## 設計：黑箱是硬性的

1. **不 import 本專案的任何 `internal` 套件。** 共用型別就等於共用 bug——
   兩邊都用同一個 `debitRequest` struct 的話，欄位名打錯會在兩邊以完全相同的
   方式錯掉，而測試是綠的。這裡的請求 JSON 一律**手寫字串**。
2. **只透過 HTTP 觀察行為。** 斷言只能用狀態碼、回應 body、以及事後從 DB
   數出來的列數。
3. **兩個目標的差異只能出現在 `target` 這個結構裡**，測試本體不准出現
   `if target == "java"`。

### 為什麼契約測試可以直接寫資料庫

因為 **Java 版沒有任何 HTTP 端點可以建錢包**——唯一的路徑是
`member.registered` 事件（`MemberEventListener:30`），而那條路徑還沒被重寫。
所以「準備一個有餘額的錢包」只能從 DB 進去。

這不違反黑箱：**被觀察的行為**全部走 HTTP，資料庫只用來擺放前置狀態與事後點算。
而「事後點算」正是最關鍵的一環——回應說 `idempotent: true` 是**實作自己說的**，
**流水只有一筆**才是真的沒有重複入帳。

seed 走 `docker exec`（MySQL 用 `mysql`、PostgreSQL 用 `psql`）而不是 Go 的
資料庫驅動：一種機制對兩個目標，而且 SQL 保持是可以複製去貼進 client 的字串。

---

## 目前涵蓋什麼

| # | 測試 | 釘住的東西 |
|---|---|---|
| 1 | `TestInternalEndpointsRequireSecret` | `/internal/**` 的守門。⚠️ 含「不存在的 internal 路徑也要回 401」——驗證若跑在路由**之後**，沒有 secret 的人可以靠「401 還是 404」探測內部端點 |
| 2 | `TestDebitHappyPath` | 扣款的回應欄位 + **DB 裡的餘額與流水筆數** |
| 3 | **`TestDebitIsIdempotent`** | ⭐ 最重要的一格：同鍵重送不再扣、回當初那一筆、**流水只有一筆** |
| 4 | `TestDebitInsufficientBalance` | **422**（不是 400）+ 餘額與流水都不可以動 |
| 5 | `TestDebitWalletNotFound` | 404 + `Wallet not found for player: {id}` |
| 6 | `TestDebitDefaultsSubTypeToBet` | 不帶 `subType` 要記成 `BET`（只能從 DB 看，回應沒有這個欄位） |
| 7 | `TestCreditHappyPath` | 入帳的餘額計算 |
| 8 | `TestCreditIsIdempotent` | 不重複入帳 + **冪等命中時 `frozenAfter` 必須是 `null` 不是 `0`**（地雷 #33） |
| 9 | `TestValidationRejectsBadRequests` | 11 格 Bean Validation，**連訊息文字都 diff** |
| 10 | `TestMalformedJSON` / `TestZeroPlayerID` | 兩條已知的刻意分歧（見下） |

### 特別說明第 9 格的訊息文字

自訂的 `@Pattern` 訊息本身就以欄位名開頭，串上 handler 的
`"Invalid request: " + field + " "` 之後，欄位名會出現**兩次**：

```
Invalid request: subType subType must be one of BET/SHOP_PURCHASE
```

**看起來像 bug 的地方是對的**——那是 Java 版真實的輸出，Go 版照抄
（CLAUDE.md §5：等價 > 品味）。這一格就是它的證據。

---

## 已知的刻意分歧（兩條，都在藍圖 §5 有案）

⭐ 契約測試的產出不只是「兩邊都綠」，而是**把不等價的地方逼成一份數得出來的清單**。
所以分歧一律是 `target` 結構的**欄位**，不是散在測試裡的 `if`。

| 分歧 | Java | Go | 理由 |
|---|---|---|---|
| 壞掉的 JSON | **500** | **400** | Java 的 `GlobalExceptionHandler` 註冊了 `@ExceptionHandler(Exception.class)`，搶在會回 400 的 `DefaultHandlerExceptionResolver` 之前接住 `HttpMessageNotReadableException`。「呼叫端送了壞 JSON，伺服器不該說自己壞了」 |
| `playerId: 0` | **404** | **400** | Java 的 `playerId` 只有 `@NotNull` 沒有 `@Positive`，於是 0 一路走到「查不到錢包」。非正整數的玩家 ID 不可能有錢包 |

---

## 還沒涵蓋（誠實清單）

- **409 樂觀鎖衝突**：credit 是讀改寫 + `@Version`，同玩家併發下成功率 1/N
  （地雷 #36）。黑箱難以穩定重現，目前由 store 層的
  `TestCreditConcurrentSamePlayer` 釘住。
- **事件（outbox → Kafka）完全沒驗**：目前一格都沒有斷言 outbox 列或 Kafka 訊息。
  要補的話得先決定「同一個 topic 在兩個獨立叢集上怎麼比對」，
  而那個決定與 consumer 切片綁在一起。⚠️ 也就是說：**現在兩邊綠，
  只證明 HTTP 契約等價，不證明事件等價。**
- **玩家端點 `/api/v1/wallet/**`**：Go 版還沒實作。
- **`member.registered` → `createWallet`**：兩邊都還沒接（Go 版是下一個切片）。
  接上之後，seed 就能改走事件而不是直接寫 DB——那才是完整的黑箱。

---

## CI

**go 目標已經在 CI 裡跑**（`.github/workflows/ci.yml` 的 `infra` job 最後一步）。
掛在那個 job 裡而不是另開一個：契約測試要的 MySQL + Kafka + 已跑完的 migration
在那裡全都有了，另開一個 job 等於再起一次全部容器換來一個更好看的名字。

⚠️ **java 目標不在 CI 裡**，因為它需要團隊 repo 的 wallet-service 與 PostgreSQL。
所以：

> **CI 綠只證明 Go 版沒有回歸，不證明兩版仍然等價。**
> 「兩版等價」目前是**手動、按需**驗證的——每次改動 HTTP 契約、
> 或準備把某個切片當成「完成」時，都要在本機對 java 目標跑一次。
