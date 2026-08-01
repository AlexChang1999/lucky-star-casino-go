# CHANGELOG

本專案所有「會影響行為」的變更都記在這裡（規則見 `AGENTS.md` §3）。
最新的在最上面。

---

## [feat] — 2026-08-01 — Phase A 起步：wallet 帳務 schema 與 domain 契約

Phase A（wallet 重構）的第一個切片。**還沒有任何業務端點**——
這一輪做的是「把已經定死的正確答案抄下來，並讓測試釘住它」。

**Added**

- **`docs/notes/`——團隊 Java 版的實地查證筆記**（3 份）。
  兩份是既有筆記搬入（CQRS／指令事件分離、Redis 用途全解），
  一份是本輪新查的（wallet 帳務口徑，含檔名行號）。
  ⚠️ 它們用的是**團隊的雷區編號**，換算表在 `docs/notes/README.md`——
  直接把數字抄進本 repo 的文件會指到完全不同的一條。
- **`deploy/mysql/init/01-wallet-schema.sql`——wallet 三張表**
  （`wallets` / `wallet_transactions` / `wallet_outbox`），
  由團隊 `database/postgres/init.sql` 逐欄位翻譯。
- **`internal/wallet/domain`——帳務型別與規則**（純函式，不碰 DB）。
  `Amount` 是具名型別而不是 `int64`：wallet 的簽章裡到處是 int64
  （playerID／amount／balance／version／txID），具名型別讓「參數順序寫反」
  **編譯不過**。Java 要做到同樣的事得包 value object，成本高到大家都不做。
- **`internal/wallet/store.VerifyWalletSchema`——開機自檢**。
  MySQL 官方映像的 `initdb.d` **只在 volume 全新時執行**（地雷 #17），
  在既有 volume 上改 `.sql` 完全沒有效果也沒有提示。自檢讓這件事
  在**開機時**失敗，而不是等到第一筆下注。
- **`docs/ADR-002`——帳務語句在 MySQL 的等價實作**。
- **地雷 #30**（見下）。

**Changed**

- **`AGENTS.md`**：新增地雷 #30、擴充 #26（`INSERT IGNORE` 的否決理由）、
  §1 必讀清單加入 `docs/notes/`、#2 / #5 / #6 / #16 補上筆記交叉引用。
- **`docs/藍圖.md`**：§6 與 §8 加入 `docs/notes/` 的指路。
- **`deploy/docker-compose.infra.yml`**：MySQL 掛載 `./mysql/init`。

**⭐ 地雷 #30：MySQL 預設定序不分大小寫，會同時弱化冪等鍵與列舉約束**

這是本輪最有價值的發現，**不在原本的預期內**，是實作時實測撞出來的。
MySQL 8.4 預設 `utf8mb4_0900_ai_ci`（不分音標、不分大小寫），
PostgreSQL 的預設區分大小寫，於是換庫之後多了兩個**兩邊都不報錯**的破口：

1. `checkin-42` 與 `CHECKIN-42` 在 UNIQUE 索引裡是同一把冪等鍵
   → 第二筆入帳被當成「冪等命中」跳過 → **少入一筆帳**
2. `CHECK (type IN ('DEBIT',...))` **放行小寫 `'debit'`** 並原樣存入
   → Go 端字串比對失敗、事件 payload 帶著小寫進 Kafka、下游 switch 落到 default

解法是帳務表的相關字串欄位一律 `COLLATE utf8mb4_bin`。
⚠️ **不改全域預設**——暱稱、商品名稱這類欄位**應該**是 ci 的。
⚠️ DSN 裡的 `collation=` 是連線定序，比對時欄位定序優先，**管不到這件事**。

**如何驗證**

```
gofmt -l .                                    # 無輸出
go vet ./...                                  # OK
go build ./...                                # OK
go test -race ./...                           # ok domain 1.464s / config
go test -race -tags=infra ./internal/wallet/  # 5 個測試、3 個子測試全 PASS
```

`-tags=infra` 那組**實際連上 MySQL 8.4.10** 跑過，逐條釘住 `docs/ADR-002` 的主張：

| 測試 | 釘住的主張 |
|---|---|
| `TestVerifyWalletSchema` | schema 真的套用了（定序 / CHECK / UNIQUE） |
| `TestIdempotencyKeyIsCaseSensitiveInDB` | 地雷 #30 方向一 |
| `TestSubTypeCheckIsCaseSensitive` | 地雷 #30 方向二 |
| `TestConditionalDebit` | 條件扣款：足額成功、餘額不足零副作用、冪等命中零副作用 |
| `TestDupEntryDoesNotAbortTransaction` | InnoDB 的重複鍵是語句級失敗，交易仍可用 |

⚠️ 後面幾項看起來像「在測資料庫而不是測自己的程式」。**是刻意的**：
它們是 ADR-002 的立論基礎，MySQL 哪天升版行為變了，
應該是測試先紅，而不是帳先錯。

**誠實記錄的負面後果**

- debit 熱路徑從 Java 版的 **2 次往返變成 3 次**（少了 `RETURNING`，
  要多一次點查拿扣款後餘額）。壓測數字出來之前，
  **不可以宣稱「Go 版比 Java 版快」**——這一條就是反例的來源。
- 🔶 **migration 工具尚未選定**，目前只有一次性建表 SQL。
  **Phase A 結束前必須補上**，否則第二次改 schema 就會重現團隊那個坑。
  `VerifyWalletSchema` 是現階段的緩解，不是解法。

---

## [chore] — 2026-08-01 — 專案骨架、治理層與資料層定案

新 repo 的第一批內容：能跑的基礎設施、連線層、以及**兩份推翻團隊既有決策的 ADR**。
業務服務尚未開工。

**Added**

- **`deploy/docker-compose.infra.yml`——四個基礎設施一鍵起**：
  MySQL 8.4（帳務寫入主庫）、MongoDB 8.0（CQRS 讀端）、Redis 7、Kafka 4.1（KRaft）。
  埠全部與團隊 repo 及 `lucky-star-notify-go` 錯開（3308 / 27018 / 6380 / 9095），
  **三套環境要能同時跑才做得成 A/B 對照**。
  Kafka 的雙 listener 設定直接沿用 `notify-go` 已實戰驗證的那份。
- **`internal/platform/config`——設定載入與驗證**。缺必填一律拒絕啟動，
  且**一次回報所有問題**（`errors.Join`）；有測試釘住這個行為。
  「打錯字」與「沒設定」刻意分開處理——`MYSQL_PORT=3308a` 是錯誤，不是靜默走預設。
- **`internal/platform/store`——三個儲存的連線與連線池設定**。
  連線池三個參數顯式設定並註明理由（`ConnMaxLifetime` **必須小於** MySQL 的
  `wait_timeout`，否則池裡會留著伺服器已關閉的連線，隨機噴 invalid connection）。
  ⚠️ 這個套件刻意**沒有定義 interface**——需要測試替身的是 repository，不是連線本身。
- **`docs/ADR-000`——為什麼現在改用 Go**。逐條回應團隊 ADR-000「用 Java 而非 Go」：
  理由 1（Spring 一站式）重量下降；理由 2（新手團隊）**前提消失，非判斷錯誤**；
  理由 3（效能差距可由架構彌補）**尚未被實測支持**——引用團隊自己的 T-090 壓測
  （1,000 併發 P99 5,055ms、5xx 89.3%、效能 gate 持續 FAIL）。
  ⭐ 團隊 ADR-000 的後果第 4 點本來就寫了「若出現即時推播閘道可局部評估 Go，
  但需另開 ADR」——`notify-go` 正是那個服務，本檔就是它要求的那份說明。
- **`docs/ADR-001`——資料層 MySQL + MongoDB**（Supersedes 團隊 ADR-001）。
- **`AGENTS.md`——29 條地雷**。A 類 17 條從團隊 repo 原封不動帶走（業務與架構本質，
  換語言一樣會踩）、B 類 8 條是 `notify-go` 實際踩過的、C 類 4 條本專案新增。
- **`CLAUDE.md`**。§2 相對 `notify-go` 升級：那邊的「沒有兩個實作就不要有 interface」
  是為單體 8,500 行的服務定的，本專案是 7 服務規模，**interface 放在消費端與
  外部邊界是對的**。⚠️ 但 DI 仍然是 `main.go` 明確傳參，**不引入容器**。
- **`.claude/agents/`——三個 subagent**：`go-reviewer`（唯讀）、`go-tester`（只碰測試檔）、
  **`java-reference`（新增）**。第三個是本專案才需要的：業務規則散在 555 個
  `.java` 檔裡，找答案是「大範圍搜尋、讀很多、結論很短」的工作——
  那正是 subagent 唯一真正划算的形狀。

**Changed**

- **資料層推翻團隊 ADR-001**：從「PostgreSQL 寫 + MySQL 讀」改為
  **「MySQL 寫 + MongoDB 讀」**。
  - **為什麼**：① 團隊否決「單一 MySQL」的技術理由是「`FOR UPDATE SKIP LOCKED`
    支援較弱」，而該語法在 **MySQL 8.0.1（2017）就已加入**，決策當時 8.4 已是 LTS
    ——這屬於「當初就判斷錯了」。最有力的反證是團隊自己把要求交易原子性的
    `outbox_events` 放在 MySQL 讀庫裡。
    ② 團隊的「讀庫」其實不是讀庫——`members`、`friendships` 等表在那裡是**唯一寫入端**，
    等於第二個主庫，而且是**沒有交易保護跨在兩個主庫上的業務**。
    ③ 讀模型天生是反正規化文件，join 五張表 vs 一份文件一次讀出。
  - **失去什麼**（誠實記錄）：MySQL 沒有 `RETURNING`（要用 `LastInsertId()`，
    ⚠️ 批次插入時回的是**第一筆**的 id）；沒有 `JSONB`（但半結構化資料歸 Mongo，
    損失為零）；沒有部分索引；以及 9 個目標職缺中有 1 個明列 PostgreSQL。
  - **本 repo 的界線**：MongoDB 只放**由 Kafka 事件投影、可重建的衍生資料**，
    **絕不可**成為任何欄位的唯一真相。這條界線一旦模糊，CQRS 就退化回
    「兩個都是主庫」——也就是原本那個結構。
- **`docs/藍圖.md` 從 `notify-go` 搬進本 repo** 並就地標註：§3.2 資料層已被 ADR-001
  取代，原表格**保留不刪**（「當初怎麼想、後來為什麼改」本身就是敘事）；
  MongoDB 的定位從「為學習而保留」升級為 **CQRS 讀端**，
  原本照團隊 ADR-010 模式寫的誠實免責聲明因此不再需要。

**如何驗證**

```
gofmt -l .                                  # 無輸出
go vet ./...                                # OK
go test -race ./...                         # ok internal/platform/config 1.394s
go test -race -tags=infra ./...             # 4 個測試、7 個子測試全 PASS
```

`-tags=infra` 那組**實際連上四個容器**跑過：MySQL 連線池參數生效、
MongoDB 可寫可讀、Redis ZSET 排序正確。
⭐ 其中 `SKIP LOCKED` 子測試是刻意寫的——**與其在 ADR 裡宣稱 MySQL 8 支援它，
不如讓測試釘住**。它若哪天紅了，ADR-001 的主張就需要修正。

⚠️ 用 build tag 而不是「偵測不到就 `t.Skip`」：skip 會讓「基礎設施沒起來」與
「測試通過」在輸出上長得幾乎一樣，於是 CI 綠燈其實什麼都沒驗到。
