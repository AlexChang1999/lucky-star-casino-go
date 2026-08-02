# 幸運星幣城（Java 版）— CQRS 讀視圖 & 指令／事件分離 筆記

> 日期：2026-07-29
> 來源：**團隊 Java repo** `H:\Lucky_Star_Casino`（唯讀參考）
> 依據：該 repo 的 `docs/adr/ADR-001.md`（雙庫 CQRS）、`docs/adr/ADR-002.md`（指令/事件分離）
> 搬進本 repo：2026-08-01（原檔頭誤寫 `D:\Lucky_Star_Casino\Lucky_Star_Casino`，已更正）

> ⚠️ **這份筆記描述的是「被替換掉的那個系統」，不是本專案的設計。**
> 內文提到的 PostgreSQL、`@KafkaListener`、Java 類別名稱**全部指團隊 Java 版**。
> 讀的時候請照下表換算；本專案的定案一律以 `docs/ADR-001` 與 `docs/藍圖.md` 為準。
>
> | 本文（Java 版） | 本專案（Go 版） | 依據 |
> |---|---|---|
> | PostgreSQL 5433（帳務寫入主庫） | **MySQL 8.4**（3308） | `docs/ADR-001` |
> | MySQL 3307（CQRS 讀端） | **MongoDB 8.0**（27018） | `docs/ADR-001` |
> | `wallet_outbox`（PG 表） | 同名概念，改建在 **MySQL** | 地雷 #5 |
> | `@KafkaListener` / `ack.acknowledge()` | `segmentio/kafka-go` + `CommitMessages` | 地雷 #19 #20 |
> | 團隊 `AGENTS.md` 雷區 6 / 8 / 23 / 24 | 本 repo `AGENTS.md` §2 **#2 / #3 / #5 / #6** | 對照表見 `docs/notes/README.md` |
>
> **不變的部分**（也是留這份筆記的理由）：Outbox 為什麼存在、指令／事件為什麼必須分兩個
> topic、at-least-once 下誰該去重誰不該去重——**這三件事與語言、與資料庫都無關**，
> Go 版要原封不動照做。

---

## 問題一：為什麼不讓 PostgreSQL 寫好庫後直接回傳前端，而要繞 Kafka 非同步？

### 那個「消費者」叫什麼

**`WalletReadSyncListener`**

檔案：`backend/wallet-service/src/main/java/com/luckystar/wallet/kafka/WalletReadSyncListener.java`
任務編號：T-025（帳務流水查詢）

---

### 先修正一個前提：不是所有讀都繞 Kafka

專案分成兩條路：

| API | 走哪個資料庫 | 為什麼 |
|---|---|---|
| `GET /api/v1/wallet/balance` | **直接查 PostgreSQL** | 餘額必須最新，不能容忍延遲 |
| `GET /api/v1/wallet/transactions` | **MySQL 讀庫**（Kafka 同步來的） | 流水明細，晚幾百 ms 沒差 |

ADR-001 第 117 行明文寫死這條規則。

所以「非同步」只用在**流水明細**這種讀多寫少、對即時性要求寬鬆的查詢。

---

### 完整資料流

```
POST /internal/wallet/debit
  │
  ├─ 【單一 PostgreSQL 交易，原子性】
  │    ① UPDATE wallets            ← 條件 UPDATE + 行鎖，防超扣
  │    ② INSERT wallet_transactions ← idempotency_key UNIQUE，防重複扣款
  │    ③ INSERT wallet_outbox       ← 事件先落庫（Transactional Outbox）
  │  COMMIT
  │  ↑ 到這裡就回傳 200 給呼叫方，玩家不用等下面的流程
  │
  └─ WalletOutboxPoller（@Scheduled 排程）
       撈出 status=PENDING 的列 → 發到 Kafka topic `wallet.debit` → 標記 SENT
       │
       ├─► WalletReadSyncListener（wallet-service 內）→ 寫 MySQL wallet_transactions（讀視圖）
       ├─► rank-service  WalletBalanceChangedConsumer  → 更新排行榜 Redis ZSET
       └─► admin-service WalletEventConsumer           → 後台監控 / 報表
```

> 🔁 **本專案（Go 版）的同一條流**：`MySQL 交易（UPDATE wallets + INSERT wallet_transactions
> + INSERT wallet_outbox）` → outbox poller → Kafka → 三個消費端，其中 read-sync 寫的是
> **MongoDB**。⚠️ 兩個 Go 專屬的實作差異：① 樂觀鎖要**自己檢查 `RowsAffected`**，
> GORM 不會為 `== 0` 回傳 error（地雷 #3）；② outbox poller 的 Kafka writer 要設
> `BatchSize: 1`，否則 `kafka-go` 預設會壓滿 1 秒才送（地雷 #21）。

> 📌 **Outbox 是一張普通的 PostgreSQL 表，不是 Kafka 的元件**。`wallet_outbox`
> （`database/postgres/init.sql:175`）只是拿 `topic` / `kafka_key` / `payload` 當欄位的
> 「待寄郵件匣」，Kafka 完全不知道它存在。Outbox Pattern 是通用模式——下游換成
> RabbitMQ 或 HTTP 呼叫也一樣適用。
>
> 📌 **Kafka 的接收者是「其他微服務的 `@KafkaListener` 程式」，不是資料庫**。Kafka
> （compose 服務 `lucky-star-kafka`，9092）是獨立的訊息中介：訊息留在 topic 裡，
> 訂閱者收到後**才各自去寫自己的資料庫**。方向是
> `wallet → PG outbox（DB 寫入）→ Poller 讀（DB 讀取）→ Kafka（網路投遞）→ 其他服務程式`。
>
> 📌 **「PG 已經寫好帳了，為什麼還要發事件？」** 因為那本帳**只屬於 wallet 自己**。
> 微服務的 database-per-service 原則禁止 rank/admin 直連 wallet 的 PG 去 SELECT
> （否則 wallet 改欄位名就弄死三個服務，退化成共用資料庫的分散式單體）。
> 事件是「錢動了」唯一的對外告知管道；少了它 → 排行榜不動、後台報表沒金流、流水頁空白。

---

### 為什麼不「同步雙寫」（寫完 PG 順手寫 MySQL）？四個理由

#### 1. 雙寫沒有交易保證 → 用 Outbox 解決

如果在同一個 method 裡：

```java
// ❌ 錯誤示範
postgresRepo.save(tx);   // commit 成功
mysqlRepo.save(view);    // 這裡爆炸怎麼辦？PG 已經 commit，無法回滾
```

兩個獨立資料庫，**沒有共同的交易邊界**。要做到原子性只能上 XA 分散式交易（兩階段提交），慢又脆弱、任一參與者掛掉整條卡住。

**Outbox Pattern 的解法**：把「要發的事件」寫進 `wallet_outbox` 表，跟帳務**同一個 PostgreSQL 交易**。這樣一致性問題被壓縮回「單一資料庫的交易」，這是資料庫本來就保證的事。

之後由排程器慢慢把 outbox 的列撈出來送 Kafka。送失敗就下次再送 → **at-least-once（至少一次）** 投遞。

> 📌 **outbox 的列不會自己消失**：poller 投遞成功只把 `status` 標成 `SENT`，**從不刪除** ——
> 而每筆下注/派彩/贈禮都寫一列，這張表是單向成長的。已補 `WalletOutboxPurgeJob`
> （member 為 `OutboxPurgeJob`）每日 04:00 刪掉 `sentAt` 超過保留期（預設 7 天）的 SENT 列。
> 兩個設計點：① **只刪 SENT** —— PENDING 代表尚未確認送達，刪掉就是無聲丟失事件，正是 Outbox
> 要防的事；② **保留 7 天而非投遞完就刪** —— 下游漂移時，outbox 是唯一能回答
> 「這則事件到底有沒有發出去、何時發的」的證據，且 7 天與 rank 消費端去重標記的 TTL 對齊
> （都對應「最大重送窗口」）。

（歷史雷：舊寫法是 commit 後直接 `kafkaTemplate.send()` 但沒 `.get()`、沒 callback。broker 失敗發生在背景執行緒，連 log.warn 都不印，事件無聲丟失 → 下游 MySQL 讀視圖／rank 排行／admin 報表三方同時漂移。見 AGENTS.md 雷區 23。）

#### 2. at-least-once 的代價：消費端必須冪等

Kafka 會重送同一則訊息。所以 `WalletReadSyncListener` 開頭就檢查：

```java
// 冪等：讀庫已有同 id 即視為重送，跳過寫入但仍 ack
if (viewRepository.existsById(event.transactionId())) {
    log.warn("Duplicate debit event id={}, skipping", event.transactionId());
    ack.acknowledge();
    return;
}
```

（判準：**操作本身冪等嗎？** 冪等寫入如 `ZADD` 絕對值 → 不需去重，加了反而有害；非冪等累加如 `ZINCRBY` → 才需要 Redis SETNX 去重。見 AGENTS.md 雷區 24。）

#### 3. 讀寫互搶鎖

`wallets` / `wallet_transactions` 是**熱寫表**：行鎖、版本遞增、條件 UPDATE，每一筆下注都在動它。

流水查詢則是**分頁掃描 + 排序 + 走 index**，一次撈幾十列。

兩者在同一台 PG 上就是在搶同一份 buffer pool 跟鎖。分離到 MySQL 後，查詢再重也不會拖慢扣款。→ ADR-001 決策理由第 2 點。

#### 4. 下游不只一個消費者

`wallet.credit` / `wallet.debit` 現在被 **3 個地方**消費：

| 消費者 | 檔案 | 收到後做什麼 |
|---|---|---|
| wallet 自己 | `kafka/WalletReadSyncListener.java:45,80` | 寫 MySQL 讀視圖（流水查詢） |
| rank-service | `kafka/WalletBalanceChangedConsumer.java:42` | 更新排行榜 Redis ZSET |
| admin-service | `kafka/WalletEventConsumer.java:31` | 後台報表 / 流通量監控 |

（notification-service **不**消費 wallet 事件，它聽的是 `notification.push` / `game.result` / `rank.update`。）

同步寫死的話 = 帳務熱路徑耦合 N 個下游，**rank-service 掛掉 → 玩家連錢都扣不了**。

Kafka 解耦後，下游掛了訊息就堆在 topic 裡，帳務照跑，等下游復活再慢慢追。

#### 5.（附帶）熱路徑延遲

T-090 壓測要衝吞吐。同步跨庫寫 MySQL = 每次扣款多一次跨網往返 + 一個 MySQL 交易，直接吃掉 P99。

---

### 代價：最終一致性（Eventual Consistency）

下注完馬上開流水頁，那筆可能還沒到（毫秒～秒級延遲）。

**餘額頁不受影響**（走 PostgreSQL 直查）。這是刻意的取捨：玩家對「餘額不準」零容忍，對「流水晚半秒出現」無感。

---

## 問題二：`wallet.credit` 是事件、`wallet.credit.request` 是指令 — 這是什麼概念？

### 名詞：指令／事件分離（Command–Event Separation）

事件驅動架構（EDA）的基本原則。

### 對照表

| 面向 | 指令 Command | 事件 Event |
|---|---|---|
| **語意** | 祈使句「**請**入帳」 | 過去式「**已**入帳」 |
| **本專案 topic** | `wallet.credit.request` | `wallet.credit` |
| **發送者意圖** | 我要你做某事 | 我告訴你我做了某事 |
| **接收者** | **指定一個**（wallet-service） | **不指定**，誰想聽誰聽 |
| **可否拒絕** | 可以（餘額不足、驗證失敗） | 不行，已成事實 |
| **消費者數量** | 恰好 1 個 | 0～N 個 |
| **命名慣例** | 動詞原形 + `.request` | 動詞過去式 / 名詞 |

### 為什麼一定要分？ADR-002 記錄的真實 bug

原本兩種**相反語意**共用 `wallet.credit` 這一個 topic：

| 來源 | 把它當什麼用 |
|---|---|
| member-service 簽到／新手禮 | **指令**（發出去希望 wallet 加錢） |
| kafka-init 註解、與 `wallet.debit` 對稱 | **事件**（加完錢才發） |
| rank-service（當時未實作） | 預期是**事件** |

炸出兩個問題：

**問題 A：T-017 簽到鏈路斷裂**
member 把 `wallet.credit` 當指令發出去，但 wallet-service 根本沒有 consumer 去接、去真正加餘額。
→ 玩家簽到後餘額不會增加。

**問題 B：直接補 consumer 會無限迴圈**

```
credit() 入帳成功 → 發布 wallet.credit（事件）
                        ↓
             wallet 自己消費 wallet.credit
                        ↓
                  又呼叫 credit() 入帳
                        ↓
                 又發布 wallet.credit
                        ↓
                    ♾️ 重複加錢
```

**根因**：指令（command，請做某事）與事件（event，某事已發生）混用同一個 topic。

### 解法：拆成兩個 topic

```
member 簽到/新手禮 ──發──> wallet.credit.request（指令）
                                  │
                                  │ wallet 的 WalletCreditRequestListener 消費
                                  │ → 呼叫 WalletService.credit() 真正入帳
                                  ▼
                          ──發──> wallet.credit（事件）
                                  │
                                  ├──> rank-service（更新排行）
                                  ├──> admin-service（報表）
                                  └──> notification-service（推播）

game 派彩 ──HTTP──> POST /internal/wallet/credit → credit() → 同樣發 wallet.credit（事件）
```

**為什麼結構上不可能迴圈**：wallet **消費**的是 `wallet.credit.request`、**發出**的是 `wallet.credit`，兩個不同 topic，接不上自己。

### 額外的架構紅利

`credit()` 這個方法完全重用，HTTP 派彩跟 Kafka 指令兩條路徑共用同一份程式碼，一行都不用改。

更明顯的紅利在後來：`GmRewardService`（**admin-service**，不是 member-service）要做 GM 後台發幣時，只要照同一份 payload 格式發 `wallet.credit.request` 就好，**wallet-service 完全不需要知道發布者是誰、一行程式碼都不用改**。

分離指令與事件之後，「誰能觸發入帳」從「要改 wallet-service」變成單純的「誰能發這個 Kafka 訊息」的權限問題。

`wallet.credit.request` 的發布端已從原本 2 個擴充到 4 個：

| 發布端 | 服務 | 用途 |
|---|---|---|
| `CheckinService` | member | 每日簽到（T-017） |
| `NewGiftService` | member | 新手禮（T-018） |
| `MonthlyRewardService` | member | 月度累計簽到獎勵（ADR-005） |
| `GmRewardService` | **admin** | GM 後台手動發幣 |

### 指令的 payload 契約

```json
{
  "playerId": 42,
  "amount": 50,
  "subType": "CHECKIN",
  "idempotencyKey": "checkin-42-2026-05-29",
  "referenceId": null
}
```

- `subType` 須為 DB 允許的 CREDIT 子型：`WIN` / `CHECKIN` / `TASK` / `GIFT` / `GM_REWARD` / `BANKRUPTCY_AID`
- `idempotencyKey` 保證 Kafka 重送不會重複入帳（DB UNIQUE 約束擋住）
- 消費端用 `@JsonIgnoreProperties(ignoreUnknown=true)` 容忍額外欄位（向前相容）

---

## 兩個問題的交會點：那條「唯一安全的例外」

規則說「wallet-service 永不消費 `wallet.credit`」，但 `WalletReadSyncListener` **確實**消費了 `wallet.credit` 跟 `wallet.debit`。矛盾嗎？

不矛盾。看它做了什麼：

- ✅ 只寫 MySQL 讀視圖（`WalletTransactionViewRepository.save()`）
- ✅ 用 `existsById` 做冪等檢查
- ❌ **從不呼叫** `WalletService.credit()` / `debit()`

**迴圈的前提是「同一個服務發布又消費同一個帶動帳務效果的事件」**。read-sync 沒有帳務效果，所以安全。

同理，rank-service / admin-service 消費 `wallet.credit` 也安全 —— 它們是**別的服務**，天然不會迴圈，而且它們本來就該消費，這就是設計目的。

> ⚠️ 新增任何 wallet 事件消費者時，比照這個模式：**絕不能**在消費者內再呼叫入帳／扣款方法。

---

## 一句話總結

| 概念 | 一句話 |
|---|---|
| **CQRS 讀寫分離** | 寫進強一致的 PostgreSQL，讀從查詢優化的 MySQL，中間用 Kafka 事件同步，換取讀寫互不干擾，代價是最終一致<br>→ 本專案：寫進 **MySQL**、讀從 **MongoDB**，其餘完全相同 |
| **Transactional Outbox** | 事件跟帳務寫在同一個 DB 交易，把跨系統一致性問題壓回單庫交易，再由排程器投遞 |
| **指令／事件分離** | 「請做某事」跟「某事已發生」語意相反，必須是不同 topic；混用會同時造成鏈路斷裂與無限迴圈 |

---

## 相關檔案索引

| 檔案 | 內容 |
|---|---|
| `docs/adr/ADR-001.md` | 雙庫 CQRS 決策 |
| `docs/adr/ADR-002.md` | 指令／事件分離決策 |
| `backend/wallet-service/.../kafka/WalletReadSyncListener.java` | 讀視圖同步器 |
| `backend/wallet-service/.../kafka/WalletCreditRequestListener.java` | 指令消費者 → 呼叫 `credit()` |
| `backend/wallet-service/.../service/WalletOutboxService.java` | 事件寫進 outbox |
| `backend/wallet-service/.../service/WalletOutboxPoller.java` | 排程投遞 outbox → Kafka |
| `backend/rank-service/.../kafka/WalletBalanceChangedConsumer.java` | 事件消費（排行） |
| `backend/admin-service/.../kafka/WalletEventConsumer.java` | 事件消費（報表） |
| `database/postgres/init.sql:175` | `wallet_outbox` 表定義（普通 PG 表，非 Kafka 元件） |
| `kafka/kafka-init.sh` | topic 定義（**8 業務 topic + 5 DLT**）
| `AGENTS.md` 雷區 6 / 23 / 24 | 迴圈禁令、Outbox 規則、去重判準 |
