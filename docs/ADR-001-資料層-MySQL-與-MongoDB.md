# ADR-001 — 資料層：MySQL（寫入主庫）+ MongoDB（CQRS 讀端）

| 項目 | 內容 |
|------|------|
| **狀態** | ✅ 已接受（Accepted） |
| **日期** | 2026-08-01 |
| **決策者** | 單人（作品集專案） |
| **影響範圍** | wallet / member / game / rank / admin |
| **Supersedes** | `Lucky_Star_Casino/docs/adr/ADR-001.md`「PostgreSQL（寫入主庫）+ MySQL（查詢讀庫）」（2026-05-26） |

---

## 決策

| 角色 | 選型 | 團隊原本 |
|---|---|---|
| 帳務寫入主庫 | **MySQL 8.4** | PostgreSQL 16 |
| CQRS 讀端 | **MongoDB 8.0** | MySQL 8.4 |
| 分散式鎖 / ZSET / session | Redis 7（不變） | Redis 7 |

**CQRS 的讀寫分離結構完全保留**，換掉的是兩端各自的實作。

---

## 為什麼推翻原決策

### 1. 原決策否決「單一 MySQL」的技術理由，今天已經不成立

團隊 ADR-001 的選項 B（單一 MySQL）被否決，理由原文是：

> MySQL 的樂觀鎖支援（FOR UPDATE SKIP LOCKED）相較 PostgreSQL 弱；
> ACID 保證在高併發情況下較不穩定

**這兩句在 MySQL 8.0 之後都不準確：**

- `SELECT ... FOR UPDATE SKIP LOCKED` 與 `NOWAIT` 在 **MySQL 8.0.1 就已加入**
  （2017 年）。決策日期是 2026-05，當時 MySQL 8.4 已是 LTS。
- InnoDB 的 row-level lock 是**索引記錄鎖**：`UPDATE ... WHERE version = ?`
  只要 WHERE 走主鍵或唯一索引，鎖的就是那一列，與 PostgreSQL 沒有實質差別。
  「精準度較差」的情況發生在**沒有走索引時退化成掃描範圍鎖**——
  但那是索引設計問題，不是資料庫選型問題。
- 「ACID 保證較不穩定」原文沒有給出具體場景或測試依據。
  InnoDB 是完整 ACID 的，預設隔離級別 REPEATABLE READ 甚至比 PG 的
  READ COMMITTED 更嚴格。

⚠️ **最有力的反證是團隊自己**：他們的讀庫跑的就是 MySQL 8.4，
而讀庫裡放著 `members`（玩家帳號）、`outbox_events`（Outbox 落地表）——
`outbox_events` 正是**要求交易原子性**的表。
**如果 MySQL 的 ACID 真的不可靠，Outbox 就不該放在那裡。**

這一條屬於「**當初就判斷錯了**」，不是「情境改變」。

### 2. 原本的「讀庫」其實不是讀庫

團隊 ADR-001 的分配表自己註明：MySQL 的 `members` 是
**「唯一寫入端，不是讀庫同步」**。`friendships`、`daily_checkins`、
`player_tasks`、`outbox_events` 同樣是直接寫入的。

也就是說那不是 CQRS 讀端，是**第二個主庫**——而且是**沒有交易保護
跨在兩個主庫上的業務**。他們的現況校驗記錄的 `@Transactional` 自我呼叫失效坑，
正是這個結構的併發症。

**本決策把界線劃乾淨：**

- **MySQL 是唯一真相**。所有寫入、所有需要一致性的讀取都在這裡。
- **MongoDB 只放衍生資料**：由 Kafka 事件投影而成，壞掉就重放重建，
  因此沒有備份需求，也**絕不可以**成為任何欄位的唯一來源。

⚠️ 這條界線一旦模糊（「這個欄位只有 Mongo 有」），CQRS 就退化回
「兩個都是主庫」——也就是原本那個結構。**這是本 ADR 最重要的一條。**

### 3. 讀模型天生是文件，不是關聯

CQRS 讀端存的是**反正規化的查詢結果**：一筆玩家儀表板資料在關聯式讀庫要
join 五張表，在文件庫就是一份文件一次讀出。

這是文件庫在 CQRS 讀端的標準理由——**不是「因為想用 NoSQL」**。

⚠️ 附帶好處：這讓 MongoDB 有了真實的架構角色。
`docs/藍圖.md` §3.2 原本把 MongoDB 定位成「為學習而保留，
技術上 PG 的 JSONB 就夠」，並打算照團隊 ADR-010 的模式誠實寫明。
**現在不需要那個免責聲明了**——面試問「為什麼用 MongoDB」，
答案從「為了學」變成「讀模型是反正規化文件」。

---

## 誠實的代價

**不寫下來的取捨，日後會被別人當成疏忽。**

| 失去 | 影響 | 對策 |
|---|---|---|
| **`RETURNING` 子句** | PG 可以 `INSERT ... RETURNING id` 一次拿到主鍵，MySQL 要 `LastInsertId()` | 見 `AGENTS.md` 地雷 #26。⚠️ **批次插入時 `LastInsertId()` 回的是第一筆**，照 PG 直覺寫會拿到錯的關聯 |
| **`JSONB` 與其索引** | PG 的半結構化查詢能力較強 | 半結構化資料本來就歸 MongoDB，這項損失是零 |
| **部分索引 / 運算式索引** | PG 的 `WHERE` 條件索引在稀疏資料上更省 | 本專案資料量到不了會在意這個的規模 |
| **PostgreSQL 這個關鍵字** | 9 個目標職缺中有 1 個（川圖）明列 PostgreSQL | 換到 MySQL——**同一份清單裡出現次數最多的 DB**，Junior 門檻直接寫 `Go + Gin + MySQL + Redis` |

⚠️ **最後一列要說白**：職缺關鍵字**是**這個決策的考量之一。
把它藏起來假裝純技術決策是不誠實的；但它也不是唯一理由——
前面三條技術理由各自都站得住。**能分清「這條是技術理由、那條是求職理由」，
本身就是判斷力。**

---

## 後果與注意事項

1. **金額一律 `DECIMAL(19,4)` 或整數最小單位，嚴禁 `float64`。**
   這條與資料庫選型無關，換到哪個庫都成立。

2. **帳務關鍵路徑要「看得見 SQL」。** 全專案用 GORM，但帳務服務開
   `db.Debug()` 或自訂 logger 把 SQL 印出來。
   面試時這是好答案：不是「我不敢用 ORM」，而是**「我知道 ORM 在哪裡會騙我」**。

3. **`RowsAffected == 0` 是併發衝突，不是「沒事發生」。**
   GORM 不會為此回傳 error。不檢查等於默默丟棄一次更新——
   而餘額會少一筆，沒有任何錯誤訊息。

4. **跨儲存不可能有交易。** MySQL 與 MongoDB 之間只能靠 Transactional Outbox：
   業務寫入與事件寫入在**同一個 MySQL 交易**內，之後才由發布器送進 Kafka。
   團隊 ADR-002 的指令/事件分離完全沿用。

5. **schema migration 沿用團隊的版號慣例**，但要修掉他們記錄的問題：
   PG 那邊有**兩個 `V15__` 同版號並存**，靠檔名排序重放時順序不確定。
   本專案的 migration **版號不得重複**。

6. **Redis 的角色完全不變**，而且仍然是**主儲存不是快取**
   （`AGENTS.md` 地雷 #16）。

---

## 相關文件

- `Lucky_Star_Casino/docs/adr/ADR-001.md` — 被推翻的原決策
- `Lucky_Star_Casino/docs/adr/ADR-002.md` — 指令/事件分離（沿用）
- `Lucky_Star_Casino/docs/adr/ADR-010.md` — 「明知規模不需要仍保留」的先例
- `AGENTS.md` §2 地雷 #1、#26、#27
- `docs/藍圖.md` §3.2 資料層取捨表（本 ADR 取代其中的 MongoDB 定位段落）
