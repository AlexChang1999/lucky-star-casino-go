# ADR-002 — wallet 帳務語句在 MySQL 的等價實作

| 項目 | 內容 |
|------|------|
| **狀態** | ✅ 已接受（Accepted） |
| **日期** | 2026-08-01 |
| **決策者** | 單人（作品集專案） |
| **影響範圍** | wallet（Phase A），之後 member 的 outbox 沿用同一套判準 |
| **前提** | `docs/ADR-001`（資料層改用 MySQL）。本檔處理那個決定的**直接後果** |

---

## 背景

`docs/ADR-001` 把帳務寫入主庫從 PostgreSQL 換成 MySQL 8.4。那份 ADR 已經誠實
列出「失去什麼」（沒有 `RETURNING`、沒有 `JSONB`、沒有部分索引），但停在清單層級。

**問題在於 Java 版的 debit 熱路徑正好把 `RETURNING` 用在最關鍵的位置。**
團隊為了效能做過一次改寫（T-090 B2），把原本 4 次 DB 往返壓成 2 次，
靠的就是 PostgreSQL 的兩個語句級原子特性：

```sql
-- 往返 1：條件扣款，一句話完成「冪等預檢 + 餘額守衛 + 扣款 + version+1」
UPDATE wallets
   SET balance = balance - ?, version = version + 1, updated_at = CURRENT_TIMESTAMP
 WHERE player_id = ?
   AND balance - frozen_amount >= ?
   AND NOT EXISTS (SELECT 1 FROM wallet_transactions t WHERE t.idempotency_key = ?)
RETURNING balance;                                    -- ← MySQL 沒有

-- 往返 2：寫流水，冪等鍵衝突時原子略過
INSERT INTO wallet_transactions (...) VALUES (...)
ON CONFLICT (idempotency_key) DO NOTHING              -- ← MySQL 沒有
RETURNING id;                                         -- ← MySQL 沒有
```

（原始碼：`wallet-service/.../postgres/repository/WalletDebitDao.java`）

「MySQL 沒有 `RETURNING`」是 `AGENTS.md` 地雷 #26，但**知道有這個坑不等於
知道怎麼繞過去**。這份 ADR 把三條語句逐一定案，並說明每個取捨。

---

## 決策

### 1. `UPDATE ... RETURNING balance` → 條件 UPDATE + 檢查 `RowsAffected` + 點查餘額

```go
res := tx.Exec(conditionalDebit, amount, playerID, amount, key)
if res.RowsAffected == 0 {
    // 冷路徑：冪等命中 / 錢包不存在 / 餘額不足，由呼叫端補查區分
}
// 熱路徑：再點查一次拿扣款後餘額
tx.Raw(`SELECT balance FROM wallets WHERE player_id = ?`, playerID).Scan(&balanceAfter)
```

**代價**：熱路徑從 2 次往返變成 3 次。誠實記錄，**不美化**。

**為什麼可以接受**：第 3 次是同一筆交易內、主鍵點查、且該列的行鎖已經在手，
必定命中 buffer pool。它不是「多一次查詢」，是「多一次記憶體讀取加一次網路來回」。
真正的成本是 RTT，Phase A 的壓測會量出實際數字（`藍圖` §3.4）。

**為什麼不用 MySQL 的 user variable 技巧**：

```sql
UPDATE wallets SET balance = (@new := balance - ?) WHERE ...;
SELECT @new;                     -- 看起來省掉一次表存取
```

❌ **否決**。MySQL 官方文件明確寫「在同一個語句裡同時讀寫 user variable 的
求值順序是未定義的」，且該用法已被標記為 deprecated。
帳務程式碼不接受「實測看起來是對的」——`CLAUDE.md` §5 的規則是
**冪等鍵、樂觀鎖、金額型別有一點不確定就停下來**。
多一次點查換掉一個未定義行為，這個交易划算得沒有懸念。

> ⚠️ 順帶釐清一個容易混淆的點：`RowsAffected` 在 MySQL driver 預設回的是
> **changed rows** 而不是 **matched rows**（`clientFoundRows` 預設 false）。
> 本專案的條件 UPDATE 恆有 `version = version + 1`，所以「有匹配」必然
> 「有異動」，兩者等價。
> **這才是「樂觀鎖 UPDATE 一定要動 version」的真正理由之一**——不只是為了
> 版本號本身，也是為了讓 `RowsAffected == 0` 只有一種解讀。
> 寫成 `SET balance = ?` 而忘記動 version 的話，「更新成同樣的值」會回 0，
> 被誤判成併發衝突。

### 2. `ON CONFLICT DO NOTHING` → 一般 `INSERT` + 捕捉錯誤碼 1062

```go
if err := tx.Exec(insertTx, ...).Error; isDupEntry(err) {
    // 併發同鍵競態：對手贏了，補償回沖並回查贏家紀錄
}
```

**否決 `INSERT IGNORE`**：它把**所有**錯誤降級成警告——資料截斷、NOT NULL 違反、
外鍵違反全都變成靜默的 no-op。在帳務路徑上，那意味著
**餘額已經扣了、流水卻沒寫進去，而且沒有任何錯誤訊息**。
這正是本專案最想避免的失敗形狀。
捕捉 `ER_DUP_ENTRY (1062)` 只吞重複鍵這一種，其餘照樣往外炸。

**否決 `ON DUPLICATE KEY UPDATE`**：它會改寫贏家那一列（帳務流水不可變），
且 `LastInsertId()` 的語意在該情境下變得模稜兩可。

### 3. `RETURNING id` → `LastInsertId()`

`INSERT` 成功後 `res.LastInsertId()` 就是新流水 id。

> ⚠️ **地雷 #26 的第二半**：批次插入時 `LastInsertId()` 回的是**第一筆**的 id，
> 不是最後一筆。wallet 這裡是單列插入所以不受影響，
> 但 outbox 的批次投遞若哪天改成批次寫入就會踩到。

### 4. ⭐ MySQL 的重複鍵錯誤**不會**中止交易——這一條比原版簡單

PostgreSQL 裡任何錯誤都讓整筆交易進入 aborted 狀態，之後所有語句被拒絕，
所以 Java 版**必須**用 `ON CONFLICT DO NOTHING` 來避免炸掉交易
（或者用 SAVEPOINT，更囉嗦）。

InnoDB 的重複鍵錯誤只是**語句級**失敗，交易仍然可用。
於是 Go 版可以直接捕捉 1062 之後繼續在同一筆交易裡做補償回沖，
不需要 `ON CONFLICT` 的等價物。

⚠️ **但不要過度推廣這條**：死鎖（1213）與鎖等待逾時（1205）**會**回滾整筆交易。
「MySQL 的錯誤都不影響交易」是錯的，認錯誤碼必須精確。

### 5. 帳務表的字串欄位一律 `COLLATE utf8mb4_bin`

見 `AGENTS.md` 地雷 #30。這一條不在原本的預期內，是實作時實測發現的，
因為它是「換資料庫」這件事裡一個**兩邊都不會報錯**的差異。

### 6. ⭐ 扣款交易明寫 `READ COMMITTED` —— 2026-08-01 補充

> **補充於實作 `Repository.Debit` 時**，原始版本沒有這一節。
> 它推翻了一個沒寫下來的隱含假設：「隔離級別用預設的就好」。

決策 1 的條件 UPDATE 在 MySQL 的預設隔離級別（REPEATABLE READ）下**無法運作**。
兩個獨立的失敗模式，都是 PostgreSQL 上不存在的：

**① gap lock 死鎖（地雷 #34）**

`NOT EXISTS (SELECT 1 FROM wallet_transactions WHERE idempotency_key = ?)`
在鍵不存在時，會對唯一索引的 supremum 下 **S 型 gap lock**。
另一個已持有 `wallets` 行 X 鎖的交易要 INSERT 同一個 gap，需要
**insert intention lock**——兩者互斥，於是循環等待。

⚠️ **不同的冪等鍵也會撞**：索引還小的時候，所有不存在的鍵都落在同一個
supremum gap 裡。實測 20 筆同玩家併發下注（每筆不同鍵）**19 筆 1213**。
真因取自 `SHOW ENGINE INNODB STATUS` 的 `LATEST DETECTED DEADLOCK`，
兩邊持有／等待的鎖清清楚楚，不是推測。

**② RR 的快照讓補償路徑回查不到贏家（地雷 #32）**

RR 的快照在交易的**第一次一致性讀**（決策 1 的點查餘額）就固定；
贏家是在那之後才提交的 → 普通 SELECT 看不到它 → 誤判成
「衝突了卻找不到贏家」，而決策 2 的補償路徑正好依賴這次回查。

**決策**：`sql.TxOptions{Isolation: sql.LevelReadCommitted}`，只設在扣款交易上。

**這不是效能調校，是等價**。這份 ADR 的整個前提是「Java 版跑在 PostgreSQL 上，
我們要在 MySQL 上做出等價的東西」——而 **PG 的預設隔離級別就是 READ COMMITTED**，
PG 也沒有 gap lock。沿用 MySQL 的 RR 預設不是「保守」，是憑空引入兩個
原版不存在的失敗模式。

**為什麼不改全域或連線預設**：偷偷改掉全域之後，後來的人完全看不出哪裡變了，
而隔離級別會影響**每一條**查詢的語義。放在交易的選項上，理由就寫在使用它的地方。

**為什麼不靠死鎖重試就好**：19/20 的死鎖率下，重試是在遮蓋病灶而不是治它，
而且每次重試都是一整筆交易白做。重試仍然保留（`maxDeadlockRetries = 3`）
但定位是**縱深防禦**——鎖順序會隨資料分布與執行計畫改變，
「不可能死鎖」在 MySQL 上不是能保證的事。
⚠️ 重試安全的前提是**冪等鍵不變**（地雷 #4），且只重試 1213 / 1205。

**RC 帶來的行為差異（不是錯誤，但要知道）**：同鍵併發時後到者走的是
**補償回沖路徑**而不是冷路徑——RC 的快照是 per-statement 的，後到者在被行鎖
擋住之前就取好了快照。Java 版在 PG 上稱這條路徑為「極窄競態」，
在 MySQL 上它是**常態**。淨額、流水數、事件數都正確，
但 `version` 因此**不是**「餘額變動次數」（每個回沖的 loser 讓它 +2）。

### 7. ⭐ credit 保留「讀改寫 + 樂觀鎖」，並自己補上補償 —— 2026-08-02 補充

> **補充於實作 `Repository.Credit` 時。** 前面六條都在講 debit；這一條處理的是
> 「同一份 ADR 的判準套到一個**形狀不同**的方法上會怎樣」。

團隊只對 debit 做過 T-090 B2 那次「壓成一條語句」的改寫。credit 到現在仍是
JPA 的讀改寫 + `@Version`（`WalletService.java:168-260`）：

```
往返 1  以冪等鍵查流水（快路徑）
往返 2  findById 載入錢包
往返 3  save() → UPDATE ... WHERE version = ?   ← 0 列 = 409
往返 4  INSERT 流水 → 唯一鍵衝突 → 回查贏家
outbox  wallet.credit 進 wallet_outbox（同一交易）
```

**決策 7a：不把 credit 也壓成條件 UPDATE。**

技術上做得到，而且會快。**但那會刪掉一個對外行為**：樂觀鎖衝突（HTTP 409）
從此不再發生。這份 ADR 的整個前提是等價，而等價的對象是**可觀察的行為**，
不是語句數。要改的話得先讓契約測試涵蓋它，再進藍圖 §5 當成刻意的改進。

代價誠實記錄，且**已量測**（`TestCreditConcurrentSamePlayer`）：

| | debit（條件 UPDATE） | credit（讀改寫 + 樂觀鎖） |
|---|---|---|
| 20 筆同玩家不同鍵併發 | 20 筆全成功 | **成功 1、409 十九筆** |
| 熱路徑往返數 | 3（+outbox） | 4（+outbox） |

N 個交易同時讀到 `version = v`，只有一個 UPDATE 得逞。**PostgreSQL 的 EPQ
行為相同**，所以這是 Java 版的既有行為，不是 MySQL 引入的。
→ 已收進 `AGENTS.md` 地雷 #36。

**決策 7b：`RowsAffected == 0` 必須自己檢查。**

Java 的 `@Version` 讓 JPA 自動比對受影響列數並丟
`ObjectOptimisticLockingFailureException`。**Go 沒有這個魔法**——GORM 執行同樣的
UPDATE，但 0 列時**不回傳 error**（地雷 #3）。少了那個 switch，一次被蓋掉的更新
會被當成成功，然後照樣寫流水、發事件：流水說加了錢、餘額說沒有。

決策 1 那條註記在這裡第二次派上用場：`version = version + 1` 讓「有匹配」必然
「有異動」，所以 `RowsAffected == 0` 只有一種解讀。

**決策 7c：⭐ 1062 之後必須補償——這一條推翻了決策 4 的樂觀語氣。**

決策 4 說「MySQL 的重複鍵不中止交易，這一條比原版簡單」。**用在 credit 上，
它反而更危險**：

Java 的 catch（`:220-233`）在 PostgreSQL 上其實**回不了正常值**——PG 的約束違反讓
整筆交易 aborted，catch 裡那句回查自己也會炸，結局是交易回滾、餘額沒多加。
**Java 是被 PG 的語義意外保護的**（實際結局是 500）。
⚠️ `WalletTransaction` 用 `GenerationType.IDENTITY`，`save()` 會立刻送出 INSERT，
所以例外確實落在 try 區塊內——這是判斷結局的依據，不是推測。

MySQL 少了這層保護，逐行照抄的結果是**餘額加了、流水沒寫、正常 commit**：
重複入帳，零錯誤訊息。所以 Go 版必須自己寫 `compensateCredit`，與 debit 對稱。

⚠️ 回沖加回的是**實際解凍量**而不是請求值——Java 的 `max(0, frozen - unfreeze)`
會夾住超額請求，兩者可能不同。加回請求值會讓 `frozen_amount` 憑空長大，
於是可用餘額變小、之後的下注拿到**假的餘額不足**。
→ 已收進 `AGENTS.md` 地雷 #35。

**決策 7d：credit 同樣明寫 READ COMMITTED，但理由只有一半。**

- 地雷 #34（gap lock 死鎖）**不適用**：那個死鎖來自條件 UPDATE 的 `NOT EXISTS`
  子查詢，credit 沒有它，往返 1 是普通的一致性讀、在 RR 下不上任何鎖。
- 地雷 #32（RR 快照）**適用，換了個入口**：往返 1 就是這筆交易的第一次一致性讀，
  快照在那裡固定 → 往返 2 讀到的 version 可能是舊的 → 往返 3 的鎖定讀看最新值
  → `WHERE version = ?` 恆不成立 → **憑空多出一批 Java 版不會有的 409**。

同一個結論、不同的推導，所以 `debitTxOptions` 與 `creditTxOptions` 刻意是
**兩個變數**：隔離級別是 per-transaction 的決定，合併成一個會讓「改一個等於改兩個」。

---

## 這些主張怎麼被證明

不靠文件宣稱，靠測試釘住。`internal/wallet/store/schema_infra_test.go`
（`-tags=infra`，連真的 MySQL）逐條驗證：

| 測試 | 釘住的主張 |
|---|---|
| `TestVerifyWalletSchema` | schema 真的套用了（含定序、CHECK、UNIQUE） |
| `TestIdempotencyKeyIsCaseSensitiveInDB` | 決策 5 的第一個方向 |
| `TestSubTypeCheckIsCaseSensitive` | 決策 5 的第二個方向 |
| `TestConditionalDebit` | 決策 1：足額扣款 / 餘額不足零副作用 / 冪等命中零副作用 |
| `TestDupEntryDoesNotAbortTransaction` | 決策 4 |

`repository_infra_test.go`（2026-08-01 新增）再往上驗一層——決策 1~3 組起來
之後**整條扣款路徑**的行為：

| 測試 | 釘住的主張 |
|---|---|
| `TestDebit`（5 格表格） | 四種結局，含餘額不足與錢包不存在的**零副作用** |
| `TestDebitIsIdempotent` | 重送不再扣款、不再寫流水、**不再發一次事件** |
| `TestDebitWritesOutboxPayload` | 決策 3 的 id 真的填回來了，payload 逐位元組對齊 Java |
| `TestDebitConcurrentSamePlayer` | ⭐ 20 goroutine 搶 1000 元：恰好 10 成功、餘額歸零 |
| `TestDebitConcurrentSameKey` | ⭐ 同一把鍵併發：只扣一次、一筆流水、一則事件 |
| `TestIsolationLevelDecidesWinnerVisibility` | 決策 6 的②，RR / RC 各跑一次 |

`repository_credit_infra_test.go`（2026-08-02 新增）驗決策 7：

| 測試 | 釘住的主張 |
|---|---|
| `TestCredit`（5 格表格） | 正常入帳／餘額 0 也能入（沒有餘額守衛）／解凍／解凍超額夾到 0 並留痕／錢包不存在零副作用 |
| `TestCreditIsIdempotent` | 重送不再加錢、不再發事件，且 `FrozenAfter == nil`（對齊 Java 的 null） |
| `TestCreditWritesOutboxPayload` | topic 是 `wallet.credit`（事件），payload 逐位元組對齊 Java |
| `TestCreditOptimisticLockConflict` | ⭐ 決策 7b：`RowsAffected == 0` → 409 且**零副作用** |
| `TestCreditDupEntryDoesNotDoubleCredit` | ⭐⭐ 決策 7c：拿掉補償就會紅在「balance = 2000, want 1500」 |
| `TestCreditConcurrentSameKey` | 同鍵併發：只加一次、一筆流水、一則事件 |
| `TestCreditConcurrentSamePlayer` | ⭐ 決策 7a 的量測：成功 1／409 十九筆，且 version 恰好等於成功筆數 |

⚠️ `TestCreditDupEntryDoesNotDoubleCredit` **手工重演**往返 2~4 而不是呼叫
`Credit()`：要打中的窗口在往返 1 與往返 2 之間，而兩者都是非鎖定讀，
**沒有辦法用行鎖把執行緒卡在它們中間**。換來的是 100% 決定性，
而被測的補償邏輯（`compensateCredit`）是正式路徑那一份。

⚠️ 這些看起來像「在測資料庫而不是測自己的程式」。**是刻意的**：
它們是這份 ADR 的立論基礎，哪天 MySQL 升版行為變了，
應該是測試先紅，而不是帳先錯。

---

## 後果

**正面**

- 三條語句都有明確的等價物，沒有一條需要「差不多就好」。
- 決策 4 讓 Go 版的錯誤處理比 Java 版**少一層**（不需要 `ON CONFLICT`）。
- 決策 5 產出了地雷 #30，那是一條團隊 repo 不可能有的知識。

**負面（誠實記錄）**

- debit 熱路徑從 2 次往返變 3 次。壓測數字出來之前，**不可以宣稱
  「Go 版比 Java 版快」**——這一條就是反例的來源。
- 帳務表的 schema 多了 `COLLATE` 宣告，看起來囉嗦。
  緩解方式是把它做成開機自檢（`VerifyWalletSchema`）而不是靠人記得。

**待辦**

- ✅ ~~**migration 工具尚未選定**~~ —— **2026-08-01 由 `docs/ADR-003` 結案**：
  選 goose、SQL 用 `//go:embed` 打進 binary、`/docker-entrypoint-initdb.d`
  整個移除、migration 走獨立的 `cmd/migrate`（因為 MySQL 沒有交易式 DDL，
  新增地雷 #31）。`VerifyWalletSchema` 從「唯一的緩解」變成
  `migrate.VerifyVersion` 的**互補**：前者問「schema 長得對嗎」，
  後者問「migration 跑到最新了嗎」。

---

## 參考

- `docs/notes/Java版-wallet-帳務口徑.md` —— 口徑查證紀錄（含 Java 檔名行號）
- `docs/notes/Java版-CQRS-與指令事件分離.md` —— Outbox 的完整資料流
- `AGENTS.md` 地雷 #3（冪等與樂觀鎖）、#5（Outbox）、#17（initdb.d）、
  #26（沒有 RETURNING）、#30（定序）
