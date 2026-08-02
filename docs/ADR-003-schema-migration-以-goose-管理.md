# ADR-003 — schema migration 以 goose 管理，且不在服務啟動時自動執行

| 項目 | 內容 |
|------|------|
| **狀態** | ✅ 已接受（Accepted） |
| **日期** | 2026-08-01 |
| **決策者** | 單人（作品集專案） |
| **影響範圍** | 全專案的 MySQL 寫入主庫。wallet 是第一個使用者，之後每個服務的 schema 都走這條路 |
| **前提** | `docs/ADR-001`（資料層改用 MySQL）、`docs/ADR-002`（帳務語句等價實作）的**待辦結案** |

---

## 背景

在這份 ADR 之前，schema 是一份 `deploy/mysql/init/01-wallet-schema.sql`，
掛在 MySQL 官方映像的 `/docker-entrypoint-initdb.d/`。

**它只在 volume 全新時執行**（`AGENTS.md` 地雷 #17）。也就是說它能做的事只有
「從零建一次」，而**改 schema 這件事它完全做不到**：在既有的 volume 上改那個
`.sql` 檔沒有任何效果，也沒有任何提示。

這在只有一次建表時撐得住，但第二次改欄位就會變成：

- 新環境（新 volume）：有新欄位
- 舊環境（既有 volume）：沒有新欄位
- **兩邊都不報錯**，直到某個查詢用到那個欄位

團隊 Java 版踩過這個坑，所以它是 A 類地雷（語言無關、必須帶走）。
`ADR-002` 的「待辦」把補上 migration 工具列為 **Phase A 結束前必做**，
本檔就是那件事。

`internal/wallet/store.VerifyWalletSchema` 是當時的緩解措施——它讓「忘了套用」
在**開機時**失敗而不是等到第一筆下注。但它是**緩解不是解法**：
它只認得一份硬編碼的檢查清單，未來每加一個欄位都要有人記得回來改它。

---

## 決策

### 1. 工具選 goose（`github.com/pressly/goose/v3`）

| 候選 | 否決／採用理由 |
|---|---|
| **goose** ✅ | Up/Down 寫在**同一個檔**裡，改欄位時兩個方向一眼對照得到；可用 `//go:embed` 打進 binary；版本表就兩欄；整合程式碼約 20 行 |
| golang-migrate | 生態最大，但 up/down 拆成兩個檔（`000001_x.up.sql` / `.down.sql`），而且**失敗會把版本表標成 dirty**、之後每次執行都拒絕跑，要人工 `force` 掉。⚠️ MySQL 沒有交易式 DDL（見下），dirty 會是**常態**而不是意外，那個工作流在這裡特別痛 |
| Atlas | 宣告式（寫目標 schema、工具算 diff），功能最強，但要引入外部工具鏈與 HCL。本專案目前只有一個服務有 schema，**答不出「為什麼需要它」**（`CLAUDE.md` §5） |
| 自幹 | 約 60 行寫得出來，也符合本專案「自幹 STOMP」的調性。但 **migration 不是差異化賣點**，而鎖、部分失敗、版本表併發這些細節自己寫容易有洞。自幹要花在 STOMP 與 gateway 那種地方 |

### 2. migration 檔用 `//go:embed` 打進 binary

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS
```

理由：**migration 與程式碼必須是同一個版本**。若改成執行時讀目錄，
「binary 是新的、掛進去的 SQL 目錄是舊的」就是一個跑得起來的狀態，
而它的症狀是「欄位不存在」——指不到真因。embed 讓這件事在編譯期綁死。

### 3. migration **不在服務啟動時自動執行**，走獨立的 `cmd/migrate`

```bash
set -a && . deploy/.env && set +a
go run ./cmd/migrate up
```

⭐ 這是本檔最重要的一個決定，理由是 **MySQL 沒有交易式 DDL**（新增地雷 #31）：

- PostgreSQL 的 DDL 可以放在交易裡回滾，所以團隊 Java 版從來不必想這件事。
- MySQL 的 DDL 會**隱式 commit**。一個 migration 裡兩條 `CREATE TABLE`，
  第二條失敗時第一條**已經在了**，而版本表沒有記錄 → 下次重跑直接撞
  duplicate，而且工具認為「這個版本從沒跑過」。
- 於是多副本服務同時啟動一起下 DDL，就不只是「重複做一次白工」，
  而是**併發 DDL 撞在一起留下半套 schema**。
- goose 對 PostgreSQL 有 advisory lock 可以擋（`WithSessionLocker`），
  **對 MySQL 沒有內建的**。

所以部署時 migration 是明確的一步（K8s 用 Job / initContainer），
服務端負責的是**檢查**而不是**修改**。

### 4. 服務啟動時檢查版本：`migrate.VerifyVersion`

```go
if err := migrate.VerifyVersion(ctx, sqlDB); err != nil {
    return err   // 開不起來，而不是帶著舊 schema 跑
}
```

它與 `VerifyWalletSchema` 是**互補**的，兩個都要：

| | 問的問題 | 涵蓋範圍 | 抓不到什麼 |
|---|---|---|---|
| `migrate.VerifyVersion` | migration 跑到最新了嗎 | **通用**：任何未來的 migration 忘了跑都會被抓到 | 版本號對、但 schema 被人手動 ALTER 過 |
| `VerifyWalletSchema` | 跑出來的東西長得對嗎 | **具體**：定序、CHECK、UNIQUE 索引 | 只認得硬編碼的那份清單 |

### 5. `deploy/mysql/init/` 整個移除，migration 成為唯一真相

保留它當「全新 volume 的快速路徑」很誘人（`compose up` 之後就能用），
但那等於**同一份 schema 有兩個來源**。兩份定義一定會漂移，而漂移的症狀是
「新環境和舊環境的 schema 不一樣」——沒有錯誤訊息的那種。

代價：全新環境多一步 `go run ./cmd/migrate up`。這個代價買到的是
「schema 只有一個地方定義」，划算。

### 6. baseline（00001）是**唯一**准用 `IF NOT EXISTS` 的 migration

它必須能在「initdb.d 時代就已經建好表」的既有 volume 上安全地跑一次，
把版本表補記成 1 而不是撞 duplicate。實測結果（2026-08-01，MySQL 8.4.10）：

```
--- status (轉移前) --- 00001  pending
--- up ---              已套用 00001 00001_wallet_schema.sql（8ms）
--- status (轉移後) --- 00001  applied  2026-08-01T05:41:37Z
```

⚠️ 代價是它**看不出定義漂移**：表存在但欄位不同時它靜靜跳過。
這正是決策 4 裡 `VerifyWalletSchema` 存在的理由——而且為了讓那個保護真的
有效，`internal/wallet/store` 有一個測試在**空的臨時資料庫**上跑完 migration
之後才做自檢（`TestMigrationProducesVerifiableSchema`）。
在已經有表的開發庫上驗，`IF NOT EXISTS` 會讓測試綠得毫無意義。

**之後的 migration 一律不准用 `IF NOT EXISTS`**，由
`TestMigrationFilesAreWellFormed` 在單元測試層級擋下來——用它等於把
「這個變更沒生效」靜靜吞掉，而 migration 工具存在的意義就是不讓這件事發生。

### 7. 三條 migration 撰寫規則（由測試釘住）

1. **檔名是 `NNNNN_描述.sql`**，版號補到五位——讓字典序與數值序永遠一致。
2. **版號不得重複**。這條直接來自 `ADR-001` 決策 5：團隊的 PostgreSQL 那邊有
   **兩個 `V15__` 同版號並存**，重放順序取決於檔名排序，也就是不確定的。
3. **DDL migration 明寫 `-- +goose NO TRANSACTION`**。MySQL 的 DDL 反正會隱式
   commit，包在交易裡只會得到假的安全感；明寫出來讓「這裡沒有原子性」變成
   程式碼裡看得到的事實。⚠️ 純 DML 的 migration（補資料、改值）**要保留交易**。

---

## 後果

**正面**

- `ADR-002` 的待辦結案，Phase A 少一個未爆彈。
- 「忘了跑 migration」從**第一筆下注才炸**變成**開機就失敗**，而且是通用的——
  不需要有人記得回頭維護一份硬編碼的檢查清單。
- migration 的產出第一次有了自動化的正確性證據：空資料庫 → `up` →
  `VerifyWalletSchema` 綠。這條路徑在 initdb.d 時代根本不存在。
- 新增了一條 PostgreSQL → MySQL 的差異知識（地雷 #31），
  而且它與地雷 #26（沒有 `RETURNING`）是同一個來源：**團隊的經驗在這裡不適用**。

**負面（誠實記錄）**

- **多一個部署步驟**。全新環境不再是 `compose up` 就能跑，要接著 `migrate up`。
  這是決策 5 的直接代價，接受。
- **多一個依賴**（goose + 3 個間接依賴）。這是本專案第一個「為了工程流程
  而不是為了業務功能」引入的套件。判準是決策 1 的表格：自幹省不下什麼，
  Atlas 又答不出「為什麼需要」。
- **`down` 是一把危險的刀**：00001 的 Down 會 `DROP` 掉全部帳務表。
  緩解是 CLI 強制明寫 `-yes`，但正式環境的回退手段是**備份還原，不是 `goose down`**。
- **`internal/platform/mysqltest` 是一個在非測試檔裡 import `testing` 的套件**。
  這是標準庫 `net/http/httptest` 的形狀，但它意味著任何 import 它的套件都會把
  `testing` 連進去——**只准測試檔 import**。

**尚未處理**

- 🔶 **多副本的 migration 併發鎖**。目前靠「migration 是獨立的一步」迴避，
  沒有分散式鎖。等真的跑到 K8s（Phase H）且用 Job 執行時再評估是否需要
  `GET_LOCK` 之類的 MySQL session lock。現在加是為了一個還不存在的問題。
- 🔶 **outbox 清理排程**仍未實作（`AGENTS.md` 地雷 #5）。它與本檔無關，
  但同樣掛在 Phase A 的待辦上。

---

## 參考

- `docs/ADR-001` 決策 5 —— migration 版號不得重複的來源
- `docs/ADR-002` 待辦 —— 本檔結案的那一條
- `AGENTS.md` 地雷 #17（initdb.d 只跑一次）、#26（PG → MySQL 的差異）、
  #30（定序）、#31（MySQL 沒有交易式 DDL）
- `internal/platform/migrate/` —— 實作與測試
- `cmd/migrate/` —— CLI
