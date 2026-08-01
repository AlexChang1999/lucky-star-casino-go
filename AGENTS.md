# AGENTS.md — AI 開發前必讀（幸運星幣城 Go 全面重構）

> 任何 AI / 自動化代理在本專案開發前，**先讀完本檔**。
> 目的：快速掌握專案、遵守既有約定、避開已知地雷。
>
> ⚠️ **本檔不是從空白開始的。** 前身專案 `lucky-star-notify-go` 的
> `AGENTS.md` §6 做過一次「逐條核對團隊 31 條地雷」的工作，結論是**只有 3 條
> 可轉移**——因為它只拿了一個服務，而且是最不涉及帳務與遊戲的那個。
> **全面重構的可轉移率高得多**：光是下面 A 類就有 17 條要原封不動帶走。

---

## 0. 專案一句話

把 `Lucky_Star_Casino`（Java 21 / Spring Boot 3.3.5、7 個微服務、555 個 `.java`）
**逐服務**重構為 Go，用**黑箱契約測試**證明每一步都等價。
module path `github.com/AlexChang1999/lucky-star-casino-go`，**Go 1.25+**。

**這不是「用 Go 寫一個娛樂城」，是「把一個在跑的系統換掉引擎而不停機」。**
差別在於：每個行為都有一個**已存在的正確答案**，你的工作是對齊它，不是發明它。

`notification-service` 已完成，成果在獨立 repo
[`lucky-star-notify-go`](https://github.com/AlexChang1999/Lucky_Star_Notify_Go)——
自幹 STOMP 1.2 伺服器子集、契約測試 14/14 兩邊全綠、映像 18.6 MB。
**那是本重構的方法論驗證**，證明「契約測試 + 逐服務替換」這條路走得通。

---

## 1. 必讀文件（照順序）

| 順序 | 檔案 | 重點 |
|---|---|---|
| 1 | `docs/藍圖.md` | **單一真相來源**：取捨原則、技術選型、重寫順序、Phase DoD |
| 2 | 本檔 §2 已知地雷 | 不讀會浪費時間、而且多數**沒有錯誤訊息**的坑 |
| 3 | `docs/ADR-*.md` | 已拍板的架構決策（含推翻團隊 ADR 的理由） |
| 4 | `docs/notes/` | **動 wallet / gateway / rank 之前必讀**：團隊 Java 版的實地查證筆記（Outbox 資料流、Redis key inventory）。⚠️ 它們用**團隊的**雷區編號，對照表在 `docs/notes/README.md` |
| 5 | `README.md` | 對外門面 |

**參考來源（唯讀）**：
- **團隊 Java repo**：`H:\Lucky_Star_Casino\`（單層，`backend/` 直接在底下）
  ⚠️ **這個路徑會隨機器改變，開工前先 `ls` 確認**。前身專案的同一行被改錯過兩次。
  它是**唯讀參考**：可以在本機跑起來、可以改本機 `.env`，
  但**不提交任何 commit、不開 PR**。所有產出留在本 repo。
  💡 **先查 `docs/notes/`**——常見的帳務與 Redis 問題已經查證過並標了行號，
  不必每次都重掃 555 個 `.java` 檔。
- **前身 Go repo**：`H:\Lucky_Star_Notify_Go\`（notification 的完成品 + 28 條地雷）

---

## 2. ⚠️ 已知地雷

> **A 類（#1–#17）是從團隊 repo 原封不動帶走的**——它們看起來像「Java 專案的事」，
> 其實是業務與架構本質，換語言一樣會踩。**這一類最容易漏。**
> **B 類（#18–#25）是前身 Go 專案實際踩過的**，已驗證適用於 Go。
> **C 類（#26–#36）是本專案新增的**，其中 #30、#32、#34、#35 全部是
> **PostgreSQL → MySQL** 的落差，而且全部**沒有錯誤訊息**——那正是 `docs/ADR-001`
> 這個決定的真實代價，也是它最有價值的產出。
> 之後真的踩到新雷，**當場往下加**（§5）。

### A 類：業務與架構本質（語言無關，必須全部帶走）

1. **wallet 是跨儲存的，而 Go 沒有 `@Transactional` 魔法**：寫入主庫是 MySQL、
   讀端是 MongoDB。Spring 那邊靠註解就把交易邊界處理掉了，Go **必須顯式拆開**——
   而且**跨儲存不可能有交易**。正解是 Transactional Outbox（見 #16）：
   業務寫入與事件寫入在**同一個 MySQL 交易**裡，之後才由發布器送進 Kafka。
   ⚠️ 這反而是好事：它讓「為什麼不能有跨庫交易」變得一目瞭然，是面試的好材料。

2. **`wallet.credit` 是「事件」、`wallet.credit.request` 才是「指令」**：
   搞反會造成**無限迴圈**——listener 收到自己發出的事件又觸發自己。
   （團隊 ADR-002 明訂這個分離，語義與語言無關。）
   📖 事故經過與「唯一安全的例外」（read-sync 為什麼可以消費自己的事件）：
   `docs/notes/Java版-CQRS-與指令事件分離.md`。

3. **帳務＝冪等 + 防超扣，這是核心中的核心**：
   - 冪等鍵走 **UNIQUE 索引衝突**，不是先 SELECT 再 INSERT（那有 race）
   - 樂觀鎖是 `UPDATE ... WHERE version = ?` 之後**檢查受影響列數**
   - ⚠️ **`RowsAffected == 0` 代表併發衝突，不是「沒事發生」**。
     GORM 不會為此回傳 error，你不檢查就等於默默丟棄一次更新。

4. **`game→wallet` credit 失敗必須落補償單，且冪等鍵絕不可換**：
   換了冪等鍵就會**重複入帳**。重試時必須帶原本那把鍵。
   （團隊 ADR-009 的最小 Saga 補償設計，沿用。）

5. **wallet 事件走 Transactional Outbox，勿改回直接 send**：
   重構時很容易「順手簡化」掉它——但直接 send 意味著「DB 交易成功、
   送 Kafka 失敗」時事件永遠遺失，而餘額已經變了。
   ⚠️ 附帶一個容易漏的維運面：**outbox 的列投遞成功只標 SENT、從不刪除**，
   這張表單向成長，要配一支清理排程。**只刪 SENT，PENDING 無論多舊都不能刪**
   （刪掉就是無聲丟失事件，正是 Outbox 要防的事）。
   📖 完整資料流與「為什麼不同步雙寫」的四個理由：
   `docs/notes/Java版-CQRS-與指令事件分離.md`。

6. **消費端「非冪等累加」必須去重，「冪等寫入」則不可去重**：
   兩者搞反都會錯。累加型（例如統計）重複消費會多算；
   冪等寫入型（例如以主鍵 upsert）加了去重反而讓正常重放失效。
   **判準一句話**：問「這個操作重做一次會不會出錯？」不會 → 不要去重；會 → 才去重。
   ⚠️ 冪等操作加去重的具體災難：`ZADD` 寫入成功但進程在 ack 前崩潰 → 重送被去重標記
   擋掉 → **值永久停在錯的中間態**，是去重機制自己製造的資料錯誤。
   📖 `ZADD` vs `ZINCRBY` 的實例：`docs/notes/Java版-Redis-用途全解.md` §3.2。

7. **`friend.relationship.updated` 是「完整清單」事件，不是增量**：
   當成增量處理會讓好友列表越積越多。事件語義看錯就是資料錯。

8. **wallet `sub_type` 新增要「四同步」**：後端列舉、DB 約束、前端顯示、
   後台報表四處都要改。只改一處不會報錯，只會有一類交易在報表裡消失。

9. **禮品商城在 wallet 之內，不是獨立服務**（團隊 ADR-006）：
   重構時最容易「順手拆出來」。**別拆**——拆了就要處理跨服務的帳務一致性，
   而那個複雜度換不到任何東西。

10. **風控 RTP 門檻是 per-game 且含本金口徑**：
    口徑搞錯會造成**假的風控告警**，而告警看起來完全合理，很難懷疑到口徑上。

11. **捕魚血量/傷害模型與經濟再平衡不可自行更動**（團隊 ADR-003 / ADR-004）：
    這是遊戲數值，改動會直接影響 RTP。要改必須重算 RTP band 測試。

12. **老虎機權重與前端 mock 必須與後端同步**：前端 mock 是鏡像後端的，
    只要前端還在就適用。兩邊不同步時，開發環境看到的結果與正式不同。

13. **gateway 路由：具體路徑必須排在 catch-all 之前**：
    Go 的 router 同樣有順序語義。排錯的症狀是「某條 API 永遠打到別的服務」。
    ⚠️ 且 `/api/v1/auth/` **必須在 JWT 白名單內**，否則 OAuth callback 整條死掉。

14. **後台 JWT 與玩家 JWT 是兩套 secret**：混用會讓 gateway 驗不了 admin token。
    ⚠️ 反過來更危險：如果誤用同一把，玩家 token 就能打後台 API。

15. **Gateway 有兩套獨立限流 + 熔斷，調參有固定順序**：
    Go 版自寫 gateway 時要重現這個分層，不要簡化成一層。

16. **Redis 不只是快取，是主儲存**：
    - `rank:*` 排行榜 ZSET —— DB 只有每日快照且**無回填程式**
    - 捕魚 session —— Lua CAS 樂觀鎖（團隊 ADR-008，**腳本可原樣搬過來**）
    - `disabled:player:*` 後台停用標記 —— 無 TTL，清空等於所有停用玩家自動解封
    ⚠️ 所以 `docker compose down -v` 會**砍掉業務資料**。
   📖 全部 key 的 inventory（型別 / TTL / 讀寫方）、JWT 撤銷三件套、
   fail-open vs fail-closed 的判準：`docs/notes/Java版-Redis-用途全解.md`。

17. **舊 DB volume 缺 migration 會讓服務開機即死**：
    與語言無關的維運坑。換 schema 時要嘛跑 migration，要嘛砍 volume 重建。
    ⚠️ 本專案 2026-08-01 起 schema 一律由 **goose** 管（`docs/ADR-003`），
    `/docker-entrypoint-initdb.d` **已經拿掉**——它只在 volume 全新時執行，
    天生做不到「改 schema」。開發流程多一步：
    `set -a && . deploy/.env && set +a; go run ./cmd/migrate up`。
    忘了跑會被 `migrate.VerifyVersion` 在**開機時**擋下，不會拖到第一筆下注。

### B 類：前身 Go 專案已驗證的坑

18. **JWT secret 是「原始 bytes」不是 base64**：Java 版用
    `Keys.hmacShaKeyFor(secret.getBytes(UTF_8))`——**直接取字串的 UTF-8 位元組**。
    Go 必須 `[]byte(os.Getenv("JWT_SECRET"))`，**不可以 base64 decode**
    （網路上多數 Go JWT 教學都示範 base64，照抄就驗章永遠失敗）。
    失敗訊息只說 `signature is invalid`，**完全指不到真因**。
    另：`jwt.Parse` 必須帶 `jwt.WithValidMethods([]string{"HS256"})`——
    不帶等於接受 `alg: none` 與 RS256 混淆攻擊，這是 JWT 最經典的漏洞。

19. **⭐ consumer 比 topic 早啟動 → 它會永遠收不到訊息**：
    `kafka-go` 的 `WatchPartitionChanges` **預設是 false**。加入 group 那一刻
    topic 還不存在，這個 member 就被分配到 **0 個 partition**，
    而且之後**沒有任何事件會觸發 rebalance**。
    症狀最惡劣的地方是**每一項檢查都顯示正常**：healthcheck 過、日誌無錯誤、
    `--list` 看得到 group——但 `--describe` 是空的。
    解法：`WatchPartitionChanges: true` + `PartitionWatchInterval: 5s`。
    （Spring Kafka 沒這問題，它靠 `metadata.max.age.ms` 每 5 分鐘自己好。）

20. **消費端不論成功失敗都要 commit offset**：對齊 Java 的
    `finally { ack.acknowledge(); }`。不 commit 會讓 offset 卡在壞訊息上，
    **一則壞訊息就能讓整個 topic 停擺**，而外部看起來只是「功能突然沒了」。
    ⚠️ 這條與 #6 不衝突：commit offset 是「我處理過了」，
    要不要**重試**是另一回事（走 DLT，見團隊既有設計）。
    另外 `CommitMessages` 要用 `context.WithoutCancel(ctx)` 再加逾時：
    關機時 root ctx 已被取消，直接傳下去會讓最後一則提交失敗、下次重播。

21. **`kafka-go` 的 `Writer.BatchTimeout` 預設是 1 秒**（`BatchSize` 預設 100）：
    一次只寫一則訊息時，producer 端會**整整壓一秒**才送出。
    **任何低頻、單則寫入的 writer 都要設 `BatchSize: 1`**。
    ⚠️ 這一秒**日誌與指標都看不出來**，因為延遲指標量的是訊息時間戳之後的事。

22. **Go 版與 Java 版併存時，consumer group id 必須不同**：
    用同一個 group → Kafka 把 partition 分給兩邊，**每則事件只有其中一版收到**，
    看起來像「Go 版隨機漏訊息」的假 bug。本專案一律加 `-go` 後綴。

23. **Prometheus 的 gauge 回呼在每次 scrape 時「同步執行」**：
    回呼裡下 SQL＝每 5 秒打一次 DB（團隊用 Micrometer 踩過）。
    Go 的 collector 同理：用 `atomic` + 背景刷新，gauge 只讀記憶體。

24. **`GOMEMLIMIT` 是軟上限：貼上去不會死，會變慢**：
    RSS 逼近上限時 GC **持續**運行，延遲被吃掉但不 OOM
    （同情境 Java 是直接 OOMKilled）。
    `GOMEMLIMIT` 要與容器 `mem_limit` **成對設定**且略低於它——
    這是 Java「`mem_limit` 與 `-Xmx` 成對」在 Go 的對應物。

25. **`docker rm -f` 殺掉 consumer 容器會留下幽靈 member**：
    SIGKILL 不會送 `LeaveGroup`，coordinator 要等 `session.timeout.ms`
    （預設 45 秒）才踢掉它。這段期間新容器**可能一個 partition 都分不到**，
    症狀是「健康檢查過、日誌無錯、但一則訊息都收不到」。
    **換容器一律 `docker stop`（優雅）而不是 `docker rm -f`。**

### C 類：本專案新增

26. **MySQL 沒有 `RETURNING`**：這是從團隊的 PostgreSQL 換過來之後最常踩的差異
    （見 `docs/ADR-001`）。PG 可以 `INSERT ... RETURNING id` 一次拿到主鍵，
    MySQL 要靠 `LastInsertId()`。
    ⚠️ **批次插入時 `LastInsertId()` 回的是第一筆的 id**，不是最後一筆——
    照 PG 的直覺寫會拿到錯的關聯。
    📖 wallet 的 debit 熱路徑正好把 `RETURNING` 用在最關鍵的位置，
    三條語句的等價實作與取捨見 `docs/ADR-002`。
    ⚠️ 也**不要**用 `INSERT IGNORE` 當 `ON CONFLICT DO NOTHING` 的替代品：
    它把截斷、NOT NULL、外鍵違反**全部**降級成警告，於是壞資料靜默變成
    no-op 而餘額已經扣掉了。正解是捕捉錯誤碼 `1062`（ER_DUP_ENTRY）。

27. **MongoDB 讀端是「衍生資料」，絕不可成為任何資料的唯一真相**：
    讀模型由 Kafka 事件投影而成，壞掉就重放重建，因此**沒有備份需求**。
    但這條界線一旦模糊（例如「這個欄位只有 Mongo 有」），
    CQRS 就退化成「兩個都是主庫」，而且是**沒有交易保護的兩個主庫**。
    帳務真相永遠在 MySQL。

28. **三套環境要能同時跑，所以埠全部錯開**：團隊 Java 版、`lucky-star-notify-go`、
    本專案。撞埠的訊息（`port is already allocated`）很清楚，但要等 compose
    跑到一半才看得到。埠表見 `deploy/docker-compose.infra.yml` 檔頭與 §3。

29. **Windows 上 `go test -race` 需要 C compiler，而且它的安裝路徑不能含空白**：
    `-race` 依賴 cgo，錯誤訊息是 `-race requires cgo`——但**設環境變數沒用**，
    真正缺的是 gcc。而若把 gcc 裝在 `C:\Users\Alex Chang\...` 這種含空白的路徑，
    連結階段會在空白處斷成兩截，錯誤訊息是
    `C:/Users/Alex: file not recognized`——**看起來像目的檔壞掉，其實是路徑被切開**。
    解法見 §4「Windows 本機環境」。⚠️ 8.3 短檔名與 junction **都無效**。

30. **⭐ MySQL 的預設定序不分大小寫，會同時弱化冪等鍵與列舉約束**：
    MySQL 8.4 預設 `utf8mb4_0900_ai_ci`——**ai = 不分音標、ci = 不分大小寫**。
    PostgreSQL 的預設定序區分大小寫，所以團隊 Java 版從來沒遇過這兩件事：
    - **UNIQUE 冪等鍵被弱化**：`checkin-42` 與 `CHECKIN-42` 在索引裡是同一把鍵。
      兩把本來不同的鍵被判定重複 → 第二筆被當成「冪等命中」跳過 → **少入一筆帳**。
      更陰險的是 ai：`e` 與 `é` 也相等。
    - **`CHECK ... IN (...)` 列舉被弱化**：`CHECK (type IN ('DEBIT',...))`
      **放行小寫 `'debit'`** 並原樣存入 → Go 端 `tx.Type == "DEBIT"` 是 false、
      事件 payload 帶著小寫進 Kafka、下游 switch 落到 default。
      PostgreSQL 會在 INSERT 當場拒絕。

    兩者的共同點是**沒有任何錯誤訊息可以指認真因**。
    解法：帳務表裡「參與相等性判定或列舉約束」的字串欄位一律
    `COLLATE utf8mb4_bin`（見
    `internal/platform/migrate/migrations/00001_wallet_schema.sql`）。
    ⚠️ **不要改全域預設**——暱稱、商品名稱這類欄位**應該**是 ci 的
    （搜尋「Alex」要找得到「alex」）。定序是 per-column 的正確性選擇。
    ⚠️ DSN 裡的 `collation=` 是**連線定序**，管不到欄位：比對時欄位定序
    （coercibility 2）優先於字面值的連線定序（coercibility 4）。
    設了連線定序不代表你安全了。
    已於 2026-08-01 對 MySQL 8.4.10 實測驗證兩個方向，並由
    `internal/wallet/store/schema_infra_test.go` 釘住。

31. **⭐ MySQL 沒有交易式 DDL，所以 migration 失敗會留下半套 schema**：
    PostgreSQL 的 DDL 可以放進交易裡回滾，團隊 Java 版因此從來不必想這件事。
    MySQL 的 DDL 會**隱式 commit**——把 DDL 包在 `BEGIN` 裡不會報錯，
    只是那個交易在第一條 DDL 執行時就已經自己 commit 掉了。後果：
    - 一個 migration 裡兩條 `CREATE TABLE`，第二條失敗 → 第一條**已經在了**，
      而版本表**沒有記錄** → 下次重跑撞 duplicate，工具卻認為「從沒跑過」。
    - **多副本服務同時啟動一起下 DDL**，不是「重複做一次白工」，
      而是併發 DDL 撞在一起留下半套 schema。
      ⚠️ goose 對 PostgreSQL 有 advisory lock 可擋（`WithSessionLocker`），
      **對 MySQL 沒有內建的**。

    因此本專案的規矩（`docs/ADR-003`）：
    - migration **不在服務啟動時自動跑**，走獨立的 `go run ./cmd/migrate up`
      （K8s 用 Job / initContainer）。服務端只做 `migrate.VerifyVersion`
      ——**檢查**版本，不**修改** schema。
    - DDL migration 明寫 `-- +goose NO TRANSACTION`：反正沒有原子性，
      不如讓這件事在檔案裡看得見。⚠️ 純 DML 的 migration **要保留交易**。
    - 一個 migration 盡量只放一條 DDL，多條時要能重入。
    - ⚠️ 這也是否決 golang-migrate 的主因：它失敗會把版本表標成 dirty
      並拒絕後續執行，而在 MySQL 上 dirty 是**常態**不是意外。

32. **⭐ MySQL 的 REPEATABLE READ 快照會讓「回查剛提交的那一列」失敗**：
    MySQL 預設隔離級別是 **REPEATABLE READ**，交易的快照在**第一次一致性讀**
    固定，之後別人提交的列一律看不到。PostgreSQL 預設 **READ COMMITTED**，
    每條語句重取快照——所以團隊 Java 版從來沒遇過這件事。
    踩到的位置是 wallet debit 的併發同鍵補償路徑：
    ① 條件 UPDATE 扣款 → ② 點查餘額（**這一刻固定快照**）→
    ③ INSERT 流水撞 1062（代表對手已提交）→ ④ 回查贏家紀錄 → **查不到**。
    於是「衝突了卻找不到贏家」，整筆交易回滾。
    解法：回查改用 **locking read**（`SELECT ... FOR SHARE`）——
    locking read 一律讀最新的已提交版本，行為與 PG 版對齊。
    ⚠️ 不要改成全域 READ COMMITTED 來解：那會同時改掉所有查詢的語義，
    而你只需要一條語句讀最新值。
    ⚠️ 判準：**「先讀過東西、再依賴別人剛提交的結果」的交易一律要當心。**
    冷路徑（條件 UPDATE 是交易的第一條語句）不受影響——造成 NOT EXISTS
    不成立的那一列必定在 UPDATE 之前就已提交。
    由 `internal/wallet/store/repository_infra_test.go` 的
    `TestRepeatableReadSnapshotHidesCommittedWinner` 釘住。

33. **⭐ Go 的零值 `""` 與 Java 的 `null` 在 JSON 與 SQL 都不是同一個值**：
    Java 的 `String referenceId` 沒帶時是 `null`，Jackson 預設**會**輸出
    `"referenceId": null`（本專案的 Java 版沒有設 `NON_NULL`，已確認）。
    Go 的 `string` 零值是 `""`，會輸出 `"referenceId": ""`；寫進 DB 也是
    空字串而不是 `NULL`。後果：
    - 對帳查詢 `WHERE reference_id IS NULL` **一筆都找不到**
    - 讀端投影進 MongoDB 之後，同一個欄位有的文件是 `null`、有的是 `""`
    - 契約測試 diff payload 時兩邊不相等，而**沒有任何一層會報錯**

    解法：**選填欄位一律用 `*string` / `*int64`**，並用
    `domain.OptionalString` 做 `"" → nil` 的轉換（同一個轉換要共用一份，
    寫兩份必定漂移成「事件裡是 null、DB 裡是空字串」）。
    ⚠️ 這條適用於**每一個服務**的每一個選填欄位，不只 wallet——
    Java 的欄位預設可為 null，Go 的預設不行，這個落差在整個重構裡到處都是。

34. **⭐⭐ MySQL 的 gap lock 讓「UPDATE 裡帶 NOT EXISTS 子查詢」在併發下穩定死鎖**：
    這是 PostgreSQL → MySQL 目前**後果最嚴重**的一條，而且它打在 wallet 的熱路徑上。
    debit 的條件 UPDATE 帶 `NOT EXISTS (SELECT 1 FROM wallet_transactions
    WHERE idempotency_key = ?)` 做冪等預檢。鍵不存在時，InnoDB 會對唯一索引的
    **supremum 下 S 型 gap lock**；另一個已持有 `wallets` 行 X 鎖的交易要 INSERT
    同一個 gap，需要 **insert intention lock**，兩者互斥 → 循環等待。
    ⚠️ **不同的冪等鍵也會撞**：索引還小的時候，所有不存在的鍵都落在**同一個
    supremum gap**。實測 20 筆同玩家併發下注（每筆不同鍵）**19 筆 1213**。
    PostgreSQL 沒有 gap lock，所以團隊 Java 版結構上不可能踩到。

    **解法：把扣款交易明寫成 READ COMMITTED**（`sql.TxOptions{Isolation:
    sql.LevelReadCommitted}`，見 `internal/wallet/store.debitTxOptions`）。
    RC 不對搜尋下 gap lock，環就斷了。⚠️ 這不是效能調校，是**等價**——
    Java 版跑在 PostgreSQL 上，而 PG 的預設就是 RC；沿用 MySQL 的 RR 預設
    等於憑空引入兩個原版不存在的失敗模式（另一個是地雷 #32）。
    ⚠️ **只設在該筆交易上，不要改全域或連線預設**：偷偷改掉全域，後來的人
    完全看不出哪裡變了。

    **RC 帶來的行為差異（必須知道，但不是錯誤）**：同鍵併發時後到者走的是
    **補償回沖路徑**而不是冷路徑——RC 的快照是 per-statement 的，後到者在被
    行鎖擋住之前就取好了快照，那裡面還沒有贏家的流水。Java 版在 PG 上把這條
    路徑稱為「極窄競態」，在 MySQL 上它是**常態**。
    淨額、流水數、事件數都正確，但 **`version` 不是「餘額變動次數」**：
    每個回沖的 loser 會讓它 +2。拿 version 當計數器會得到錯的答案。

    **另外仍要保留死鎖重試**（`maxDeadlockRetries`）：RC 之後 debit 不再死鎖，
    但鎖順序會隨資料分布與執行計畫改變，「不可能死鎖」在 MySQL 上不是能保證的事。
    ⚠️ 重試安全的前提是**冪等鍵不變**（地雷 #4）——換鍵重試就是重複扣款。
    ⚠️ 只重試 1213 / 1205。1062 重試一萬次還是 1062，而餘額已經被扣掉了。

35. **⭐⭐ MySQL 的 1062 不中止交易，所以照抄 Java 的 credit catch 會重複入帳**：
    這是地雷 #34 的孿生兄弟——同一個「PG → MySQL」的落差，打在 credit 上，
    而且方向相反：#34 是 MySQL **多**了一個失敗模式，這條是 MySQL **少**了一層保護。

    Java 的 `WalletService.credit`（Step 5，`:220-233`）撞到唯一鍵衝突時
    直接回查贏家並正常返回。那在 PostgreSQL 上活得下來，靠的是 **PG 的約束違反會讓
    整筆交易 aborted**——catch 裡那句回查自己也會炸，於是交易回滾、餘額沒多加。
    **Java 是被 PG 的語義意外保護的，不是它自己處理對了**（實際結局是 500）。
    ⚠️ `WalletTransaction` 是 `GenerationType.IDENTITY`，所以 `save()` 會**立刻**
    送出 INSERT，例外確實落在 try 區塊內——這是判斷結局的依據，不是推測。

    InnoDB 的 1062 只是**語句級**失敗，交易還活著（`docs/ADR-002` 決策 4）。
    逐行照抄的後果是：
    ① 樂觀鎖存檔已經把 `balance + amount` 寫進去 → ② INSERT 撞 1062、流水沒寫
    → ③ catch 回查贏家、正常 return → ④ **交易 commit**。
    結果是**餘額多加了一次而流水只有一筆**，且**沒有任何錯誤訊息**。

    可達的交錯（RC 之下是常態不是理論值）：
    `T2 冪等檢查（查不到）→ T1 整筆提交 → T2 讀錢包（讀到新 version）
    → T2 樂觀鎖過關 → T2 INSERT 撞 1062`。

    解法：與 debit 的 `compensate` 對稱，**同交易內回沖**
    （`internal/wallet/store.compensateCredit`），再回查贏家。
    ⚠️ 回沖加回的必須是**實際解凍量**，不是請求帶進來的 `unfreezeAmount`：
    Java 的 `max(0, frozen - unfreeze)` 會夾住超額請求，兩者可能不同。
    加回請求值會讓 `frozen_amount` 憑空長大 → 可用餘額變小 → **假的餘額不足**，
    而 `CHECK (frozen_amount >= 0)` 只擋負數，擋不住這個方向。
    由 `TestCreditDupEntryDoesNotDoubleCredit` 釘住（拿掉補償就會紅在
    「balance = 2000, want 1500」）。

    ⚠️ **判準比這個案例更廣**：只要一筆交易「先做了寫入、再依賴一個可能失敗的
    唯一鍵 INSERT」，從 PG 搬到 MySQL 時就要重新問一次
    「這個錯誤之後交易還活著嗎？活著的話前面那些寫入怎麼辦？」

36. **⭐ credit 是讀改寫 + 樂觀鎖，同玩家高併發下成功率是 1/N**：
    團隊只對 debit 做過 T-090 B2 那次「壓成一條語句」的改寫，credit 到現在仍是
    JPA 的讀改寫 + `@Version`（`WalletService.java:168-260`）。後果是可量測的：

    | | debit（條件 UPDATE） | credit（讀改寫 + 樂觀鎖） |
    |---|---|---|
    | 20 筆同玩家不同鍵併發 | **20 筆全成功**（DB 序列化） | **成功 1、409 十九筆**（實測） |
    | 往返數 | 3（+outbox） | 4（+outbox） |

    N 個交易同時讀到 `version = v`，只有一個 UPDATE 得逞，其餘 N-1 個
    `WHERE version = v` 全部落空。**PostgreSQL 的 EPQ 行為相同**，所以這不是
    MySQL 的問題，是 Java 版的既有行為——`ErrConcurrentModification` → HTTP 409，
    呼叫端帶**原本那把冪等鍵**重試（地雷 #4）。

    ⚠️ **不要「順手」把 credit 也改成條件 UPDATE**。那樣做 409 會整個消失，
    是對外行為漂移。要改必須先讓契約測試涵蓋它，再進藍圖 §5 當成刻意的改進。
    ⚠️ 也**不要**在 store 層自動重試樂觀鎖衝突：重試權在知道冪等鍵怎麼來的那一層，
    而且悄悄重掉會讓呼叫端從此再也看不到 409。
    由 `TestCreditConcurrentSamePlayer` 釘住這個對照。

---

## 3. 約定速查

### 技術棧（決策理由見 `docs/藍圖.md` §3）

| 層 | 選型 | 一句話理由 |
|---|---|---|
| 語言 | Go 1.25+ | 職缺共同要求；前身專案已驗證 |
| HTTP（業務服務） | **Gin** | 9 個職缺提及最多，生態與招募現實對齊 |
| HTTP（推播服務） | **標準庫 `net/http`** | 自幹 STOMP 是賣點，**不要為了統一而改** |
| 服務間通訊 | **gRPC + Protobuf** | 取代現有的服務間 REST |
| 寫入主庫 | **MySQL 8.4** | 見 `docs/ADR-001`（推翻團隊 ADR-001 的 PG） |
| 讀端 | **MongoDB 8.0** | CQRS 讀模型是反正規化文件，天生適合文件庫 |
| ORM | **GORM** | 職缺點名最多。⚠️ 帳務關鍵路徑要 `db.Debug()` 把 SQL 印出來 |
| 快取／主儲存 | **Redis 7** | 分散式鎖、ZSET 排行榜、捕魚 session |
| 訊息 | **Kafka**（`segmentio/kafka-go`） | 純 Go，可 `CGO_ENABLED=0` 靜態編譯 |
| Log | **`log/slog`**（標準庫） | 不引入 zap/zerolog |
| 追蹤 | OpenTelemetry + Jaeger | 跨服務鏈路是「微服務」唯一的硬證據 |

**明確不做**：Echo/Fiber、NATS/RabbitMQ、Kong/APISIX、Elasticsearch（延後）、
Memcached、自建區塊鏈節點、冷熱錢包、Vault/KMS、GKE。理由見藍圖 §9。
⚠️ **「不做」的理由比「做」更重要**——面試問「為什麼不用 X」答得出來才是判斷力。

### 對外埠一覽

| 埠 | 用途 | 團隊 repo | notify-go |
|---|---|---|---|
| **3308** | MySQL | 3307 | — |
| **27018** | MongoDB | — | — |
| **6380** | Redis | 6379 | — |
| **9095** | Kafka（HOST listener） | 9092 | 9094 |

⚠️ MongoDB 用 27018 而非預設 27017，是把 27017 留給開發者本機自己裝的 MongoDB。

### 目錄

`internal/` 底下是實作細節（外部 module 無法 import，比 Java 的 package-private 更硬）。
本專案**全部放 `internal/`**——沒有任何東西是給外部 module 用的，
所以不需要 `pkg/`。**空目錄 git 不會追蹤**，只有裝了程式碼的目錄會進 repo。

```
cmd/<service>/        每個服務一個 main
cmd/migrate/          schema migration CLI（docs/ADR-003）
internal/<service>/   各服務私有實作
internal/platform/    跨服務共用（設定、DB 連線、log、Kafka）
internal/platform/migrate/            migration 本體，SQL 用 //go:embed 打進 binary
internal/platform/migrate/migrations/ ⭐ schema 的**唯一真相**
internal/platform/mysqltest/          infra 測試共用的臨時資料庫（⚠️ 只准測試檔 import）
deploy/               compose 與部署設定
docs/                 藍圖與 ADR
docs/notes/           團隊 Java 版的實地查證筆記（唯讀參考，非本專案設計）
test/contract/        跨語言黑箱契約測試（同一份對 Java 與 Go 都跑）
test/load/            Go 自寫壓測 client
```

### Git / 提交

- **個人 repo，單人開發**，但**採用分支流程**：
  - `main` —— 穩定版
  - `develop` —— 整合分支，**PR 一律進這裡**
  - `feat/*` `fix/*` `chore/*` —— 工作分支，完成後開 PR 進 `develop`
- ⭐ **每個新切片一律從最新的 `develop` 開新分支，不准接在已合併的舊分支後面**：

  ```bash
  git checkout develop && git pull && git checkout -b feat/<新切片>
  ```

  2026-08-02 踩過一次：credit 直接接在已合併的 `feat/wallet-debit` 後面繼續做，
  症狀有兩個——**分支名與內容對不上**（那條分支最後裝了 debit 與 credit 兩批
  已合併的工作），以及**開 PR 前用本機過期的 `origin/develop` ref 誤判了 PR 範圍**，
  於是 PR 標題寫成「debit + credit」而實際只含 credit，事後才更正。
  ⚠️ `git log origin/develop..HEAD` 讀的是**本機快取的 ref**。
  判斷「我到底領先 develop 幾個 commit」之前一定要先 `git fetch`，
  否則會拿到一個看起來很有說服力的錯答案。
- commit 格式：`type(scope): 中文描述`
  例：`feat(wallet): 冪等鍵改走 UNIQUE 衝突而非先查後寫`
- scope 用服務或套件名：`wallet` / `gateway` / `game` / `member` / `rank` /
  `admin` / `notify` / `platform` / `deploy` / `docs`
- **一個 PR 一個切片**。切片的粒度是「能獨立說清楚一件事」——
  debit 是一個、credit 是一個。合在一起不會比較快，只會讓 review 與
  CHANGELOG 都失焦。

### CHANGELOG / ADR

- **`CHANGELOG.md` 在根目錄，單一份**。任何影響行為的變更都在最上方新增一筆，含
  **為什麼**（決策理由）與**如何驗證**（例：`go test -race ./...` 結果）。
- 純錯字、格式微調可略過。
- 架構級決策另寫 `docs/ADR-00X.md` 並在 CHANGELOG 引用。
- ⚠️ **推翻團隊既有 ADR 時，必須在新 ADR 裡寫明 `Supersedes`**
  並逐條回應原本的理由——哪些依然成立、哪些因情境改變而不再成立、
  哪些當初就判斷錯了。**這是本專案最有價值的敘事，不要省略。**

### Subagent（`.claude/agents/`）

明細與決策理由見 `.claude/agents/README.md`。

⚠️ **冷啟動成本**：subagent 會自己重讀本檔 + `CLAUDE.md`，每次約 10k token。
**小改動、單檔修正、問問題一律主執行緒直接做**；只有雜訊大、要來回試錯的工作
才值得派。地雷知識**只維護在本檔 §2**，不要複製進 agent 檔案（會漂移）。
改完 agent 檔案要**重開 session** 才註冊。

---

## 4. 驗證指令（提交前自查）

```bash
go vet ./...
go test -race ./...
golangci-lint run
go build ./...
```

**跑起基礎設施**：

```bash
cp deploy/.env.example deploy/.env    # 首次
docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait

# ⚠️ compose up 之後 schema 是空的——initdb.d 已經拿掉（地雷 #17、docs/ADR-003）
set -a && . deploy/.env && set +a
go run ./cmd/migrate up
go run ./cmd/migrate status          # 確認每個版本都是 applied

# 需要基礎設施真的起來的測試（環境變數同上）
go test -race -tags=infra ./...

# 收工。⚠️ 不要隨手加 -v，Redis 是主儲存（地雷 #16）
docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env down
```

**規則**：
- **改 schema 一律是「加一個新的 migration 檔」**，不是去改既有的那個。
  改既有的檔在你的機器上會「看起來沒事」（版本已 applied，goose 不會重跑），
  但新環境會拿到不同的 schema——**沒有錯誤訊息**的那種不一致。
- **`-race` 是硬性要求，不是選配**。這是併發服務，沒有競態偵測器的測試等於沒測。
  （Java 沒有等價工具，這點值得在 README 講。）
- 模糊測試找到的失敗案例會被寫進 `testdata/fuzz/`——**有價值的要手動搬進版控**
  當回歸案例，別讓 `.gitignore` 吃掉。
- `golangci-lint` 安裝（**v2 的路徑多了 `/v2`**，用舊路徑會裝到 v1.64）：
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`

**Windows 本機環境**（跟機器綁定，換機器要重做）：

```powershell
winget install -e --id GoLang.Go
# ⚠️ gcc 的安裝位置「絕對不能含空白」，見地雷 #29
winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT --location "C:\toolchains\winlibs"
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
go install golang.org/x/tools/gopls@latest
claude mcp add gopls -- gopls mcp
```

Go 與 gcc 都**不在**預設 PATH，且 Claude Code 起的子行程拿到的是舊 PATH 快照，
跑指令前要自己補：

```powershell
$env:PATH = "C:\Program Files\Go\bin;C:\toolchains\winlibs\mingw64\bin;$env:USERPROFILE\go\bin;$env:PATH"
```

Git Bash 裡不能直接沿用上面那串（反斜線路徑塞進 bash 的 `PATH` 找不到 gcc，
錯誤訊息還是那句 `-race requires cgo`，指不到真因）：

```bash
export PATH="/c/Program Files/Go/bin:/c/toolchains/winlibs/mingw64/bin:/c/Users/<user>/go/bin:$PATH"
```

- ⚠️ 用 PowerShell 的 `Get-Content` 看含中文的檔案會顯示成亂碼，那是**顯示問題
  不是檔案壞了**。要確認內容請用編輯器或 `Get-Content -Encoding utf8`。
- ⚠️ 對原生 exe 做 `2>&1` 重導向，PowerShell 5.1 會把 stderr 每一行包成
  ErrorRecord 並讓 `$?` 變 `$false`，即使程式回 0。跑服務時**不要重導 stderr**。

---

## 5. 更新本檔

踩到新雷、改變約定、加新套件時，**當場更新對應段落**（並依 §3 記一筆 CHANGELOG）。
本檔的價值來自「累積」，不是「一次寫好」。
