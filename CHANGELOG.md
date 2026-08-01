# CHANGELOG

本專案所有「會影響行為」的變更都記在這裡（規則見 `AGENTS.md` §3）。
最新的在最上面。

---

## [feat] — 2026-08-01 — wallet debit 熱路徑：條件扣款 + 冪等 + Outbox 同交易

Phase A 的第二個切片，也是**第一段真的會動到餘額的程式碼**。
`docs/ADR-002` 的三條語句從文件變成實作，並由 infra 測試逐條釘住。
credit 尚未實作，HTTP 層與 outbox poller 也還沒有。

**Added**

- **`internal/wallet/store.Repository.Debit`——扣款熱路徑**。
  逐步對齊 Java 的 `WalletService.debit`（66-126，已逐行讀過原始碼）：
  條件 UPDATE → 點查餘額 → 寫流水 → 寫 outbox，全部在**同一筆交易**裡。
  冷路徑的判斷順序（冪等命中 → 錢包不存在 → 餘額不足）不可調換：
  把「餘額不足」排前面的話，重送一筆錢已經花光的舊請求會拿到 400 而不是原結果。
- **`internal/wallet/domain.DebitEvent`——`wallet.debit` 的事件契約**。
  逐欄位、**逐順序**對齊 Java 的 record，因為 outbox 存的是原始 JSON 字串，
  契約測試會直接 diff 它。有單元測試釘住輸出的位元組。
- **`domain.OptionalString`**——`"" → nil` 的共用轉換（地雷 #33）。
- **地雷 #32 / #33 / #34**（見下）。

**Changed**

- **`isDupEntry` 與 1062 從測試檔搬進 `repository.go`**：正式路徑同樣要辨識
  重複鍵，留在測試檔會變成兩份定義。順帶補上 1213 / 1205 兩個**會回滾整筆交易**
  的錯誤碼——`docs/ADR-002` 決策 4 說「認錯誤碼必須精確」，這是它的實作面。
- **`TestConditionalDebit` 改用 `repository.go` 裡的 SQL 常數**，不再自己抄一份。
  測試抄一份 SQL 的話，改壞了實作它照樣綠。

**⭐ 地雷 #34：MySQL 的 gap lock 讓熱路徑在併發下穩定死鎖**

本輪**最重要**的發現，而且完全不在預期內——`docs/ADR-002` 寫完的時候沒有人想到它。

debit 的條件 UPDATE 帶 `NOT EXISTS (... idempotency_key = ?)` 做冪等預檢。
鍵不存在時 InnoDB 對唯一索引的 supremum 下 **S 型 gap lock**；另一個已持有
`wallets` 行 X 鎖的交易要 INSERT 同一個 gap，需要 **insert intention lock**，
兩者互斥 → 循環等待。⚠️ **不同的冪等鍵也會撞**，因為索引還小的時候所有不存在的
鍵都落在同一個 supremum gap。

**實測：20 筆同玩家併發下注（每筆不同鍵），19 筆 `Error 1213`。**
不是邊角案例，是「兩個人同時下注就掛」。真因由 `SHOW ENGINE INNODB STATUS` 的
`LATEST DETECTED DEADLOCK` 確認，不是推測——報告裡兩邊持有／等待的鎖清清楚楚。

解法是把扣款交易明寫成 **READ COMMITTED**。這不是效能調校，是**等價**：
Java 版跑在 PostgreSQL 上，而 PG 的預設就是 RC，PG 也沒有 gap lock。
沿用 MySQL 的 RR 預設等於憑空引入兩個原版不存在的失敗模式。
⚠️ 只設在該筆交易上，不動全域也不動連線預設。

**⭐ 地雷 #32：RR 的快照讓補償路徑回查不到剛提交的贏家**

同一個隔離級別決策的第二個理由。RR 的快照在**第一次一致性讀**（點查餘額）
就固定，而贏家是在那之後才提交的 → 普通 SELECT 看不到它 → 誤判成
「衝突了卻找不到贏家」。PG 的 RC 每條語句重取快照，所以 Java 版沒這問題。
`TestIsolationLevelDecidesWinnerVisibility` 用**兩個隔離級別各跑一次**釘住這件事，
它就是選 RC 的證據。

**⭐ 地雷 #33：Go 的 `""` 與 Java 的 `null` 在 JSON 與 SQL 都不是同一個值**

Jackson 預設會輸出 `"referenceId": null`（已確認本專案的 Java 版沒設 `NON_NULL`）；
Go 的 string 零值會輸出 `""`，寫進 DB 也是空字串而不是 NULL。
後果是對帳查詢 `WHERE reference_id IS NULL` 一筆都找不到、讀端投影出兩種空值，
而**沒有任何一層會報錯**。解法是選填欄位一律用指標 + 共用一份轉換函式。
⚠️ 這條適用於**每一個服務**的每一個選填欄位，不只 wallet。

**如何驗證**

```
gofmt -l .                                # 無輸出
go vet -tags=infra ./...                  # OK
go build ./...                            # OK
golangci-lint run                         # 0 issues
golangci-lint run --build-tags=infra      # 0 issues
go test -race -count=1 ./...              # 全綠
go test -race -tags=infra -count=1 ./...  # 6 個套件全綠（wallet/store 19.4s）
```

`-tags=infra` 那組實際連上 MySQL 8.4.10，**每個測試在自己的臨時資料庫上跑**
（併發測試要斷言「總共只有 N 筆流水」，共用的庫裡有別人的資料就驗不了）：

| 測試 | 釘住的主張 |
|---|---|
| `TestDebit`（5 格表格） | 成功 / 剛好扣到零 / 商城子類型 / 餘額不足**零副作用** / 錢包不存在 |
| `TestDebitIsIdempotent` | 重送不再扣款、不再寫流水、**不再發一次事件** |
| `TestDebitIdempotencyKeyIsCaseSensitive` | 地雷 #30 在應用層的表現 |
| `TestDebitCrossPlayerIdempotencyCollision` | 回原交易值 + ERROR 級留痕（刻意保留的怪行為） |
| `TestDebitWritesOutboxPayload` | topic / kafka_key / PENDING / payload **逐位元組** |
| `TestDebitEmptyReferenceIDBecomesNull` | 地雷 #33 |
| `TestDebitConcurrentSamePlayer` | ⭐ 20 goroutine 搶 1000 元：**恰好** 10 成功、餘額歸零、流水與事件各 10 |
| `TestDebitConcurrentSameKey` | ⭐ 10 goroutine 同一把鍵：只扣一次、一筆流水、一則事件 |
| `TestIsolationLevelDecidesWinnerVisibility` | 地雷 #32，RR/RC 各跑一次 |

**誠實記錄的負面後果**

- **`version` 不是「餘額變動次數」**。RC 之下同鍵併發走的是補償回沖路徑，
  每個 loser 讓 version +2。淨額、流水數、事件數都正確，但拿 version 當計數器
  會得到錯的答案。Java 版的 `restoreBalance` 同樣 +1，差別只在頻率——
  在 PG 上是「極窄競態」，在 MySQL 上是常態。
- **`db.Debug()` 尚未接上**：藍圖 §3.2 要求帳務熱路徑看得見 SQL，
  但目前還沒有 `cmd/wallet`，沒有地方接。`NewRepository` 的文件已寫明
  「傳進來的 db 應該已掛好 SQL logger」，實際接線留到 main 出現時。
- **`ErrTransactionBalanceMissing` 與 Java 不同**：Java 的 `DebitResponse`
  欄位是 `Long`，冪等命中時會把 null 原樣回出去；Go 這邊改成明確報錯。
  理由是回 0 是靜默的錯誤數字，而把整條鏈路改成指標，是為一個
  「所有 Java 寫入路徑都會設值」的狀態永久付出成本。**這是刻意的分歧**，
  等契約測試建起來要再確認一次。
- 🔶 **credit 尚未實作**，`domain.NewCredit` 目前只有驗證、沒有落庫路徑。
- 🔶 **outbox poller 與清理排程仍未實作**（地雷 #5），掛在 Phase A 待辦。

---

## [chore] — 2026-08-01 — schema migration 改由 goose 管理，並清掉既有 lint

結掉 `docs/ADR-002` 的待辦（migration 工具未選定），順手把既有的 lint 問題清乾淨。
**沒有業務行為變更**，但**開發流程變了**：`compose up` 之後多一步 `migrate up`。

**Added**

- **`docs/ADR-003`——schema migration 以 goose 管理，且不在服務啟動時自動執行**。
- **`internal/platform/migrate`——migration 本體**。SQL 用 `//go:embed` 打進 binary，
  因為 migration 與程式碼**必須是同一個版本**——執行時讀目錄的話，
  「binary 是新的、SQL 目錄是舊的」是一個跑得起來的狀態，而症狀是「欄位不存在」。
- **`internal/platform/migrate.VerifyVersion`——開機時的版本守門**。
  這是地雷 #17 的**通用**解法：任何未來的 migration 忘了跑都會被抓到，
  不需要有人記得回頭維護一份硬編碼的檢查清單。
- **`cmd/migrate`——CLI**（`up` / `down` / `status` / `version`）。
  ⚠️ `down` 強制明寫 `-yes`，因為 00001 的 Down 會 `DROP` 掉全部帳務表。
- **`internal/platform/mysqltest`——infra 測試共用的臨時資料庫**。
  形狀對齊標準庫的 `net/http/httptest`：**測試支援程式碼是正式程式碼，
  只是沒有人在正式路徑上 import 它**。⚠️ 它在非 `_test.go` 檔裡 import
  `testing`，所以**只准測試檔 import**。
- **`config.LoadMySQL`**——只載入 MySQL 的設定。`cmd/migrate` 不碰 Mongo，
  不該因為 `MONGO_ROOT_PASSWORD` 沒設就拒絕啟動：那個錯誤訊息與它要做的事
  完全無關，正是本專案最想避免的形狀。
- **地雷 #31**（見下）。

**Changed**

- **`deploy/mysql/init/` 整個移除**，`docker-compose.infra.yml` 不再掛
  `/docker-entrypoint-initdb.d`。schema 的唯一真相搬到
  `internal/platform/migrate/migrations/00001_wallet_schema.sql`。
  **為什麼不保留當「全新 volume 的快速路徑」**：那等於同一份 schema 有兩個來源，
  而兩份定義一定會漂移——漂移的症狀是「新舊環境 schema 不一樣」且不報錯。
- **`VerifyWalletSchema` 的定位**：從「migration 缺席時的唯一緩解」變成
  `VerifyVersion` 的**互補**。前者問「schema 長得對嗎」（定序 / CHECK / UNIQUE），
  後者問「migration 跑到最新了嗎」。兩個都要，都只在開機時跑一次。
- **`AGENTS.md`**：新增地雷 #31、#17 補上 goose 的流程、§3 目錄表更新、
  §4 驗證指令加入 `migrate up` 與「改 schema 一律是加新檔」的規則。
- **lint 清零**（`golangci-lint run` 與 `--build-tags=infra` 都是 0 issues）：
  - `store.go` 的 **ST1005** 是**誤判**，用 `//nolint` 標記並寫明理由：
    那條規則本意是「除非專有名詞或縮寫」，但 staticcheck 的啟發式只認得
    **含兩個以上大寫字母**的字——所以 `MySQL` / `MongoDB` 不被標，只有 `Redis` 被標。
    訊息形狀刻意與另外兩個保持一致，不為了討好 linter 而改成別的樣子。
  - `store_infra_test.go` 的 **errcheck ×3**：`Close` / `Drop` 失敗改成 `t.Errorf`
    而不是丟掉——連線沒關乾淨會讓**後面**的測試莫名其妙拿不到連線。
  - `store_infra_test.go` 的 **SA1019**：`ZRevRange` → `ZRangeArgs{Rev: true}`。
    Redis 6.2 起 `ZREVRANGE` 已被取代，而排行榜是本專案的主儲存之一，
    熱路徑上的指令一開始就用對，比之後全域搜尋替換便宜。

**⭐ 地雷 #31：MySQL 沒有交易式 DDL，migration 失敗會留下半套 schema**

與地雷 #26（沒有 `RETURNING`）同源——**團隊的 PostgreSQL 經驗在這裡不適用**。
PostgreSQL 的 DDL 可以放進交易裡回滾；MySQL 的 DDL 會**隱式 commit**，
把它包在 `BEGIN` 裡不會報錯，只是那個交易在第一條 DDL 就自己 commit 掉了。

1. 一個 migration 裡兩條 `CREATE TABLE`，第二條失敗 → 第一條**已經在了**、
   版本表**沒有記錄** → 下次重跑撞 duplicate，而工具認為「從沒跑過」。
2. 多副本服務同時啟動一起下 DDL → 併發 DDL 撞在一起留下半套 schema。
   goose 對 PostgreSQL 有 advisory lock 可擋，**對 MySQL 沒有內建的**。

所以 migration 是**獨立的一步**（`cmd/migrate`），服務端只做 `VerifyVersion`
——**檢查**版本，不**修改** schema。DDL migration 一律明寫
`-- +goose NO TRANSACTION`：反正沒有原子性，不如讓它在檔案裡看得見。
⚠️ 純 DML 的 migration **要保留交易**。
這條也是否決 golang-migrate 的主因：它失敗會把版本表標成 dirty 並拒絕後續執行，
而在 MySQL 上 dirty 是**常態**不是意外。

**如何驗證**

```
gofmt -l .                                     # 無輸出
go vet ./...                                   # OK
go build ./...                                 # OK
golangci-lint run                              # 0 issues
golangci-lint run --build-tags=infra           # 0 issues（之前是 5 個）
go test -race ./...                            # 全綠
go test -race -tags=infra ./...                # 6 個套件全綠
```

`-tags=infra` 那組**實際連上 MySQL 8.4.10**，每個測試在自己的臨時資料庫上跑：

| 測試 | 釘住的主張 |
|---|---|
| `TestMigrationFilesAreWellFormed` | 檔名格式、**版號不得重複**（ADR-001 決策 5：團隊有兩個 `V15__` 並存）、Up/Down 都在、只有 baseline 可用 `IF NOT EXISTS` |
| `TestMigrationsApplyFromEmptyDatabase` | 空庫被 `VerifyVersion` 擋下 → `up` → 表出現 → 重跑是 no-op → `down` 後又被擋下 |
| `TestMigrationProducesVerifiableSchema` | ⭐ **migration 的產出通過帳務自檢**——在**空的**臨時庫上跑，因為在既有的庫上 `CREATE TABLE IF NOT EXISTS` 整段跳過，測試會綠得毫無意義 |

CLI 也在**既有的開發庫**（表是 initdb.d 時代建的）上實走過一次轉移：
`status` 顯示 `pending` → `up` 8ms 完成且**不動既有的表** → `status` 顯示 `applied`
→ 再 `up` 一次回「沒有待套用的 migration」。
這正是 baseline 用 `IF NOT EXISTS` 要換到的東西。

**誠實記錄的負面後果**

- **多一個部署步驟**：全新環境不再是 `compose up` 就能跑。這是「schema 只有
  一個地方定義」的代價，接受。
- **多一個依賴**（goose + 3 個間接）。這是本專案第一個「為工程流程而非業務功能」
  引入的套件。
- **baseline 的 `IF NOT EXISTS` 看不出定義漂移**（表存在但欄位不同時靜靜跳過）。
  緩解是 `TestMigrationProducesVerifiableSchema` 在空庫上驗，
  以及單元測試禁止 00001 以後的 migration 再用它。
- 🔶 **多副本的 migration 併發鎖尚未處理**，目前靠「migration 是獨立的一步」迴避。
  等真的跑到 K8s（Phase H）再評估。
- 🔶 **outbox 清理排程仍未實作**（地雷 #5），同樣掛在 Phase A 待辦。

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
