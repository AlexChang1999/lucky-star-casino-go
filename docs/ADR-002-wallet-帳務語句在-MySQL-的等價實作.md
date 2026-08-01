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
**也是這份 ADR 最有價值的一段**——因為它是「換資料庫」這件事裡
唯一一個**兩邊都不會報錯**的差異。

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

- 🔶 **migration 工具尚未選定**。目前只有一次性建表 SQL 掛在
  `/docker-entrypoint-initdb.d`，而它**只在 volume 全新時執行**（地雷 #17）。
  **Phase A 結束前必須補上**，否則第二次改 schema 就會重現團隊那個坑。
  `VerifyWalletSchema` 是現階段的緩解——它讓「忘了套用」在**開機時**就失敗，
  而不是等到第一筆下注。

---

## 參考

- `docs/notes/Java版-wallet-帳務口徑.md` —— 口徑查證紀錄（含 Java 檔名行號）
- `docs/notes/Java版-CQRS-與指令事件分離.md` —— Outbox 的完整資料流
- `AGENTS.md` 地雷 #3（冪等與樂觀鎖）、#5（Outbox）、#17（initdb.d）、
  #26（沒有 RETURNING）、#30（定序）
