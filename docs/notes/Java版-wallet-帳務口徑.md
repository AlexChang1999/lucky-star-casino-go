# 幸運星幣城（Java 版）— wallet 帳務口徑查證紀錄

> 日期：2026-08-01
> 來源：**團隊 Java repo** `H:\Lucky_Star_Casino`（唯讀參考，逐檔讀過）
> 目的：Phase A 重寫 wallet 之前，把「已經定死的正確答案」抄下來。
> **這裡的每一條都標了檔名與行號，可以隨時回原始碼複驗。**

> ⚠️ 本文描述 **Java 版現況**。本專案的定案見 `docs/ADR-001`（資料層）與
> `docs/ADR-002`（帳務語句在 MySQL 的等價實作）。
> PostgreSQL → MySQL 的換算表在 `docs/notes/README.md`。

---

## 1. 金額型別：`BIGINT` / Go 的 `int64`，**不是** DECIMAL

`database/postgres/init.sql:12-21`、`postgres/entity/Wallet.java`：

```
balance        BIGINT NOT NULL DEFAULT 0   -- 可用餘額（單位：星幣，整數，無小數）
frozen_amount  BIGINT NOT NULL DEFAULT 0
version        BIGINT NOT NULL DEFAULT 0
```

星幣**沒有小數**，所以走「整數最小單位」而不是 `DECIMAL`——
`CLAUDE.md` §5 的兩個合法選項裡的後者。

⚠️ **不要「順手」改成 DECIMAL**：整條鏈路（DTO、Kafka payload、前端顯示）
都是整數，改型別會讓契約測試對不起來，而且 JSON 數字型別一變，
下游的反序列化會出現「看起來一樣但不相等」。

---

## 2. 三張表的欄位與約束

| 表 | 用途 | 關鍵約束 |
|---|---|---|
| `wallets` | 錢包主表（唯一真相） | PK `player_id`、`CHECK balance >= 0`、`CHECK frozen_amount >= 0`、`version` 樂觀鎖 |
| `wallet_transactions` | 帳務流水 | `idempotency_key` **UNIQUE**、`CHECK amount > 0`、`type` / `sub_type` 兩個 CHECK 白名單 |
| `wallet_outbox` | Transactional Outbox | `CHECK status IN ('PENDING','SENT')`、索引 `(status, created_at)` |

### `type` 與 `sub_type` 的白名單（`init.sql:38-44`）

- `type`：`DEBIT` / `CREDIT` / `BONUS`
  ⚠️ **`BONUS` 在 CHECK 裡存在，但 `WalletService` 沒有任何路徑會寫入它。**
  保留但不使用——移除等於偷偷改了 schema 契約。
- `sub_type`（13 個）：`BET`、`SHOP_PURCHASE`、`WIN`、`CHECKIN`、`TASK`、`GIFT`、
  `GM_REWARD`、`BANKRUPTCY_AID`、`DIAMOND_EXCHANGE`、`TOPUP`、`CASHBACK`、
  `REFUND`、`MONTHLY_REWARD`

### ⭐ 方向配對只存在於應用層，DB 擋不住

DB 的兩個 CHECK 是**各查各的白名單**，沒有任何約束擋得住
`type='CREDIT', sub_type='BET'` 這種組合。方向配對的真正來源是兩條 DTO regex：

| 檔案 | 允許的 `sub_type` |
|---|---|
| `dto/DebitRequest.java` `@Pattern` | `BET` \| `SHOP_PURCHASE`（2 種） |
| `dto/CreditRequest.java` `@Pattern` | `WIN` \| `CHECKIN` \| `TASK` \| `GIFT` \| `GM_REWARD` \| `BANKRUPTCY_AID` \| `DIAMOND_EXCHANGE` \| `TOPUP` \| `CASHBACK` \| `REFUND` \| `MONTHLY_REWARD`（11 種） |

2 + 11 = 13，剛好蓋滿 DB 白名單，沒有重疊。
→ Go 版收斂成 `internal/wallet/domain` 的 `subTypeDirection` 一張表，
並有測試釘住它與 schema 一致。

### 兩個容易誤判的子類型

- **`MONTHLY_REWARD` 刻意不算 `WIN`**：算成 WIN 會**污染 rank 的今日贏幣榜**。
  這種「為了下游正確而拆出來的子類型」不可自行合併。
- **`REFUND` 是退款／本金返還**（捕魚 buy-in 退款、場次結算返還剩餘局內餘額），
  不是「中獎」。RTP 口徑會用到這個區分（地雷 #10）。

---

## 3. `debit` 的流程（`service/WalletService.java:66-126`）

團隊做過一次效能改寫（T-090 B2），把 4 次 DB 往返壓成 2 次。
**冪等與防超扣語意與舊版完全等價，差別只在原子性搬進了 SQL 語句。**

```
往返 1  條件 UPDATE：冪等預檢 + 可用餘額守衛 + 扣款 + version+1，RETURNING balance
        └ 0 列 = 冷路徑（冪等命中 / 錢包不存在 / 餘額不足），補查區分，皆零副作用
往返 2  INSERT 流水，ON CONFLICT (idempotency_key) DO NOTHING RETURNING id
        └ 空 = 極窄的併發同鍵競態 → 同交易內原地補償回沖 + 回查贏家紀錄
        （不 rollback：debit 可能 join 外層交易如商城兌換，丟例外會拖垮外層）
outbox  把 wallet.debit 事件寫進 wallet_outbox（同一交易）
```

SQL 原文在 `postgres/repository/WalletDebitDao.java:35-76`。
→ **MySQL 沒有 `RETURNING` 也沒有 `ON CONFLICT`，等價實作見 `docs/ADR-002`。**

### 三個非顯而易見的細節

1. **餘額守衛用的是 `balance - frozen_amount >= amount`**，不是 `balance >= amount`。
   凍結金額目前恆為 0（debit 尚未實作凍結流程），但**條件不可簡化**——
   簡化掉之後，哪天真的啟用凍結就會超扣。
2. **`subType` 未帶時預設 `BET`**（`WalletService.java:70`）。
   game-service 送的 JSON 不帶這個欄位。⚠️ 改成必填會讓現有呼叫全變 400。
3. **冪等鍵跨玩家碰撞不會拒絕，只會 `log.error`**（`WalletService.java:128-134`）。
   語意是「回原交易值但大聲留痕」。正規鍵都以 playerId 當 namespace，
   會碰撞代表呼叫端的鍵命名有 bug。→ Go 版沿用，**不要改成拋錯**。

---

## 4. `credit` 的流程（`service/WalletService.java:168-260`）

與 debit 對稱，但**沒有做 B2 那次改寫**，仍是讀改寫 + JPA `@Version`：

```
Step 1  冪等檢查：用 idempotencyKey 查流水，已存在直接回原結果（零副作用）
Step 2  載入錢包（找不到 → 404）
Step 3  balance += amount
Step 3b 選填解凍：frozen_amount -= unfreezeAmount，以 max(0, ...) 守衛
Step 4  樂觀鎖存檔（衝突 → ObjectOptimisticLockingFailureException → 409）
Step 5  寫流水（UNIQUE 衝突 → 回查贏家紀錄並回傳，idempotent=true）
Step 6  wallet.credit 事件寫進 wallet_outbox（同一交易）
```

⚠️ **Step 1 的「先查再寫」不是冪等的全部保護**——它只是快路徑。
真正的保護是 Step 5 的 UNIQUE 衝突（`AGENTS.md` 地雷 #3：
冪等鍵走 UNIQUE 索引衝突，不是先 SELECT 再 INSERT，那有 race）。
**Go 版兩層都要保留**，砍掉 Step 1 會讓正常的重送走進昂貴路徑，
砍掉 Step 5 則直接是 race。

⚠️ **冪等命中時 `frozenAfter` 回 `null`**（`WalletService.java:179`），
註解寫「不重算凍結；以當初入帳結果為準」。這是刻意的，契約測試會看到。
→ Go 版因此用 `*Amount`（地雷 #33）：回 0 會被讀成「凍結金額是 0」，
那是一個**合法但錯誤**的數字。

### ⭐ Step 5 的 catch 在 PostgreSQL 上其實回不了正常值（2026-08-02 複驗）

`:220-233` 看起來是「撞唯一鍵 → 回查贏家 → 正常返回」，但 **PG 的約束違反會讓
整筆交易 aborted**，catch 裡那句 `findByIdempotencyKey` 自己也會炸 →
交易回滾 → 餘額沒多加 → 對外是 500。
**Java 是被 PG 的語義意外保護的，不是它自己處理對了。**

複驗依據（不是推測）：`postgres/entity/WalletTransaction.java:28` 是
`@GeneratedValue(strategy = GenerationType.IDENTITY)`，所以 `save()` 必須**立刻**
送出 INSERT 才拿得到主鍵，例外確實落在 try 區塊內、而不是延到 commit。

⚠️ **MySQL 沒有這層保護**（1062 只是語句級失敗），逐行照抄就是**重複入帳**。
Go 版必須自己補償回沖 —— 見 `AGENTS.md` 地雷 #35 與 `docs/ADR-002` 決策 7c。

### ⭐ 讀改寫 + 樂觀鎖的併發代價（2026-08-02 實測）

Go 版實測：**20 筆同玩家、不同冪等鍵的併發入帳 → 成功 1、409 十九筆。**
N 個交易同時讀到 `version = v`，只有一個 UPDATE 得逞。
PostgreSQL 的 EPQ 行為相同，所以**這是 Java 版的既有行為**，不是 MySQL 引入的。
對照組：debit 走條件 UPDATE，同樣條件下 20 筆全成功。
→ 這就是 T-090 B2 那次改寫的價值，而 credit 沒做。見 `AGENTS.md` 地雷 #36。

---

## 5. 樂觀鎖在 Go 的對應

Java 靠 JPA 的 `@Version` 自動處理：`walletRepository.save(wallet)` 會產生
`UPDATE ... WHERE version = ?`，0 列時丟 `ObjectOptimisticLockingFailureException`。

**Go 沒有這個魔法。** GORM 執行同樣的 UPDATE，但
⚠️ **`RowsAffected == 0` 時不會回傳 error**（`AGENTS.md` 地雷 #3）。
不自己檢查就等於默默丟棄一次更新——**而且沒有錯誤訊息**。

```go
res := tx.Exec(`UPDATE wallets SET balance = ?, version = version + 1
                 WHERE player_id = ? AND version = ?`, newBalance, playerID, oldVersion)
if res.Error != nil { return res.Error }
if res.RowsAffected == 0 { return ErrConcurrentModification }  // ← 少這行就是 bug
```

---

## 6. 事件契約

| topic | 語意 | 誰發 | 誰收 |
|---|---|---|---|
| `wallet.debit` | 事件（已扣款） | wallet 的 outbox poller | wallet read-sync / rank / admin |
| `wallet.credit` | 事件（已入帳） | 同上 | 同上（+ notification） |
| `wallet.credit.request` | **指令**（請入帳） | member ×3、admin ×1 | **只有** wallet |

payload 欄位（`kafka/WalletDebitEvent.java` / `WalletCreditEvent.java`）：
`transactionId`、`playerId`、`amount`、`balanceBefore`、`balanceAfter`、
`subType`、`idempotencyKey`、`referenceId`。

`wallet.credit.request` 的指令 payload：
`playerId`、`amount`、`subType`、`idempotencyKey`、`referenceId`。
消費端用 `@JsonIgnoreProperties(ignoreUnknown=true)` 容忍額外欄位（向前相容）。

⚠️ 詳細的事故經過與「唯一安全的例外」見
`docs/notes/Java版-CQRS-與指令事件分離.md`。

---

## 7. 開工前的待確認清單

以下在 Phase A 實作到對應部分時**必須回頭讀原始碼**，本文尚未查證：

- [ ] `GiftService` 的當日額度預扣／回補（Redis + DB 跨儲存，無交易保護）
- [ ] `BankruptcyAidService` 的 `SETNX + TTL` 鎖與 DB 冪等鍵雙保險
- [ ] `pending_wallet_credits` 補償單（ADR-009，地雷 #4：冪等鍵絕不可換）
- [ ] 禮品商城（在 wallet 之內，地雷 #9）的兌換流程
- [ ] `WalletOutboxPoller` 的撈取批量、重試與退避策略
- [ ] 讀端投影 `WalletReadSyncListener` 的欄位對應（→ MongoDB 文件形狀）
