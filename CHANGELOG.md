# CHANGELOG

本專案所有「會影響行為」的變更都記在這裡（規則見 `AGENTS.md` §3）。
最新的在最上面。

---

## [feat] — 2026-08-02 — wallet HTTP 層與 `cmd/wallet`：第一個跑得起來的服務

Phase A 的第四個切片。到這一輪為止 wallet 一直是「一包函式」，
現在它第一次是一個**服務**——有埠、有端點、有開機自檢、會優雅關機。
這也是契約測試唯一的前提：在這之前沒有東西可以打。

Kafka／outbox poller／玩家端點（`/api/v1/wallet/**`）仍未實作。

**Added**

- **`internal/wallet/httpapi`——HTTP 邊界**（`POST /internal/wallet/debit` 與
  `/credit`）。端點路徑、回應信封、狀態碼、**訊息文字**全部逐字對齊 Java，
  來源是逐檔讀過的 `InternalWalletController`、`GlobalExceptionHandler`、
  `ApiResponse`、`InternalSecretFilter`。
- **`cmd/wallet`——服務進入點**。依賴用**明確傳參**組裝，不引入 wire/fx/dig
  （CLAUDE.md §2）：設定 → 連線 → 開機自檢 → repository → handler → 伺服器，
  由上往下讀一次就看得完。
- **`internal/platform/store.NewGormLogger`——把 GORM 的 SQL 導進 slog**。
  這結掉 debit 那一輪記下的待辦（「`db.Debug()` 尚未接上，因為還沒有 `cmd/wallet`」）。
  ⚠️ 不用 `db.Debug()` 是因為那是**全域**開關且輸出是純文字；本專案的 log 一律
  走結構化輸出，SQL 要能當欄位被 grep。實測輸出含 `sql` / `rows` / `elapsedMs`
  ——其中 **`rows` 是最重要的欄位**：帳務的條件扣款與樂觀鎖靠的就是「受影響列數
  是不是 1」（地雷 #3），事後查帳時一筆 `rows=0` 的 UPDATE 就是全部的答案。
- **`config.LoadWallet` / `config.HTTPServer` / `config.LoadLogLevel`**。
  `http.Server` 的四個逾時全部顯式設定——它的零值是「**永不逾時**」，
  這是 Go 相對 Spring Boot 最容易吃虧的地方（Tomcat 有一整組預設值，Go 沒有）。

**⭐ 最重要的一條：餘額不足是 422，不是 400**

切片規格原本寫「`ErrInsufficientBalance` → 400」。
`GlobalExceptionHandler:23-27` 明寫 `@ResponseStatus(HttpStatus.UNPROCESSABLE_ENTITY)`
——**是 422**。語義上也說得通：JSON 語法對、欄位全部合法，是**業務狀態**不允許，
那正是 422 與 400 的分界。實作照 Java（CLAUDE.md §5：等價 > 品味）。

完整對照（每一格都來自原始碼，不是直覺）：

| Java 例外 | 狀態 | 訊息 |
|---|---|---|
| `WalletNotFoundException` | 404 | `Wallet not found for player: {id}` |
| `InsufficientBalanceException` | **422** | `Insufficient balance` |
| `ObjectOptimisticLockingFailureException` | 409 | `Concurrent modification detected, please retry` |
| `MethodArgumentNotValidException` | 400 | `Invalid request: {field} {message}` |
| 其餘（含 `IllegalStateException`） | 500 | `Internal server error` |

**⭐ 規格漏掉的一項：`/internal/**` 有 `X-Internal-Secret` 保護**

Java 的 `InternalSecretFilter` 對所有 `/internal/` 開頭的路徑驗一個共享 secret，
不符就回 401 `{"success":false,"data":null,"message":"Unauthorized"}`。
沒做的話 Go 版會在契約測試第一格就紅，而且是**安全性方向**的漂移。已補上：

- 定值時間比對（`subtle.ConstantTimeCompare`，對齊 Java 的 `MessageDigest.isEqual`）。
  用 `==` 的話攻擊方可以靠反覆計時把 secret 一個位元組一個位元組猜出來。
- ⚠️ 中介層掛**全域**而不是掛 gin 的路由群組。掛群組的話
  `POST /internal/wallet/nope` 會直接落到 404 而**不經過驗證**，
  於是沒有 secret 的人可以用「401 還是 404」探測哪些內部端點存在。有測試釘住。
- `INTERNAL_SECRET` 為空一律拒絕啟動：空 secret 會讓
  `ConstantTimeCompare` 對「同樣沒帶 header」的請求回 1 —— `/internal/**`
  對全世界敞開，而服務看起來、log 看起來、健康檢查看起來全部正常。

**⭐ 地雷 #33 在 HTTP 層的第二個入口：請求 DTO 也必須用指標**

之前只在**回應**與 DB 欄位上處理過 `""` vs `null`。這一輪發現**請求**這一側
更陰險，因為 Java 的 `Long` / `String` 分得出 null，而 Go 的零值分不出：

| 請求 | Java | Go 用 `int64`/`string` 會變成 |
|---|---|---|
| `{"amount": null}` | 400 `amount must not be null` | 與 `amount: 0` 無法區分 |
| `{"amount": 0}` | 400 `amount must be greater than 0` | 同上，**兩個錯誤變成一個** |
| debit 不帶 `subType` | 預設 `BET` | 與下一列無法區分 |
| debit 帶 `{"subType": ""}` | 400（@Pattern 不符） | **被當成 BET 記成一筆下注** |

最後一列是真正危險的那個：**一筆本該被拒絕的請求會靜靜地變成一筆合法的扣款**。
解法是選填欄位一律 `*int64` / `*string`，並有表格測試逐格釘住。

同一類還有兩條 domain 表達不了、少了就會出事的規則：
- `@NotBlank` 是 **trim 之後**不可為空，而 domain 只擋 `""`——
  少了 HTTP 層這一條，`"idempotencyKey": "   "` 會變成一把由三個空白組成的
  合法冪等鍵，而且真的會被寫進帳務流水。
- `referenceId` 的 `@Size(max=100)` **沒有任何一層擋**（domain 不管它、
  DB 是 VARCHAR(100)）——101 字元會撞 MySQL 1406 → 500，而 Java 是 400。

**Changed**

- **`platformstore.OpenMySQL` 多一個 `gormlogger.Interface` 參數**（傳 nil 走原本的
  Warn 等級）。兩個既有呼叫端都是測試，改動範圍就是這兩行。
- **`AGENTS.md` §3 埠表**新增 wallet 8182，並訂下「業務服務的埠 = Java 版 + 100」。
  ⚠️ 不走 notify-go 那套「+1」：那邊只搬一個服務，本專案七個都要並存，
  而團隊已占滿 8080–8087、notify-go 又占了 8088，+1 一定撞。
- **`AGENTS.md` §4** 新增跑 wallet 的指令，並記下「錢包不會被 HTTP 建出來」。
- **`docs/藍圖.md` §5** 新增第 9、10 條（見下）。

**刻意的分歧（進了藍圖 §5，不是無聲漂移）**

1. **壞 JSON：Java 500 → Go 400**。`GlobalExceptionHandler` 沒有處理
   `HttpMessageNotReadableException`，但它註冊了 `@ExceptionHandler(Exception.class)`，
   於是 `ExceptionHandlerExceptionResolver` 會搶在 Spring 內建、真正會回 400 的
   `DefaultHandlerExceptionResolver` 之前接住它。「呼叫端送壞 JSON，
   伺服器說自己壞了」是缺陷不是契約。
   ⚠️ **這條的 Java 行為是讀原始碼推導的，尚未對跑起來的實例複驗**，
   契約測試建起來時要當第一批確認項目。
2. **`playerId: 0` 或負數：Java 404 → Go 400**。Java 的 `playerId` 只有
   `@NotNull` 沒有 `@Positive`，非法 ID 會一路走到「查不到錢包」。
3. **`{"referenceId": ""}`：Java 存 `''` → Go 存 `NULL`**。
   這是 debit 那一輪 `domain.OptionalString` 決策的延伸（地雷 #33），
   已由 `TestDebitEmptyReferenceIDBecomesNull` 釘住。**未帶**的情況兩邊一致（NULL）。

**看起來像抄錯、其實是對的：`subType subType`**

Java 的 `handleValidation` 組訊息的方式是
`"Invalid request: " + fe.getField() + " " + fe.getDefaultMessage()`，
而 `CreditRequest` 的自訂 `@Pattern` 訊息本身就以欄位名開頭，
於是真實輸出是 `Invalid request: subType subType must be one of ...`——**欄位名兩次**。
Go 版把它做成 `(field, message)` 兩個欄位再串起來，讓這個重複是**結構造成的**，
而不是某個人手抄了一句奇怪的訊息。順手修掉就是行為漂移。

**如何驗證**

```
gofmt -l .                                # 無輸出
go vet ./... && go vet -tags=infra ./...  # OK
go build ./...                            # OK
golangci-lint run                         # 0 issues
go test -race -count=1 ./...              # 全綠
go test -race -tags=infra -count=1 ./...  # 全綠（wallet/store 26.3s）
go test -race -count=5 ./cmd/wallet/...   # 連跑 5 次，無 flake
```

新增 3 個測試檔、9 個測試函式（含 3 張表格共 26 格）：

| 測試 | 釘住的主張 |
|---|---|
| `TestDebitSuccess` / `TestCreditSuccessCarriesFrozenAfter` | 回應**逐位元組**比對——有人加 `omitempty` 就會紅 |
| `TestCreditIdempotentHitReturnsNullFrozenAfter` | 冪等命中回 `"frozenAfter":null` 而不是 `0`（地雷 #33） |
| `TestStoreErrorMapping`（5 格） | ⭐ 錯誤 → 狀態碼，含 422 那一格 |
| `TestValidation`（16 格） | 逐條對齊 Bean Validation，且**驗證失敗絕不可碰到帳務層** |
| `TestMalformedBodyIsRejected`（4 格） | 壞 JSON / 空 body / 型別錯 / int64 溢位 |
| `TestInternalSecret`（5 格） | 401 的四種情況 + healthz 放行；未通過驗證時 store 呼叫數必須是 0 |
| `TestServeGracefulShutdown` | ⭐ 見下 |
| `TestServeReportsListenFailure` | 埠被占用要當成**啟動失敗**，不是 log 一行就算了 |

⭐ **`TestServeGracefulShutdown` 是刻意補的，因為本機 smoke test 測不到它**：
Windows 沒有真正的 signal，Git Bash 的 `kill -TERM` 對原生 exe 走的是
`TerminateProcess`，Go 的 handler 根本不會跑（實測 smoke test 收到 exit 143，
關機日誌一行都沒有）。也就是說「本機測過了」完全不代表關機路徑是對的，
而它在 Linux 容器裡每次部署都會走到。這個測試繞過 signal 直接取消 context，
順便當 `context.WithoutCancel` 的回歸測試——少了它，shutdown 用的 context
一出生就是 done，`Shutdown` 立刻放棄、in-flight 的請求被硬斷，
而且**沒有任何錯誤訊息**。

⚠️ 寫這個測試時撞到一條平台差異：Windows 允許 `0.0.0.0:P` 與 `127.0.0.1:P`
同時存在，所以拿 `127.0.0.1` 去占埠，`serve`（綁 `:P`）照樣綁得起來 →
`TestServeReportsListenFailure` 不會拿到錯誤，而是真的開始服務並卡在 select 上，
**測試永遠跑不完**。已改成兩邊都用 wildcard，並加了逾時保險。

**Smoke test（真的連上 MySQL 8.4.10 的 compose 環境）**

```
healthz                       200
debit 沒帶 secret              401 {"success":false,"data":null,"message":"Unauthorized"}
debit 300                     200 balanceBefore=5000 balanceAfter=4700 idempotent=false
debit 重送同鍵                 200 同一個 transactionId=73，idempotent=true
credit 1000 (WIN)             200 balanceAfter=5700 frozenAfter=0
credit 重送同鍵                200 frozenAfter=null（地雷 #33）
debit 999999                  422 Insufficient balance
debit 不存在的玩家              404 Wallet not found for player: 999002
credit subType=BET            400 Invalid request: subType subType must be one of WIN/...
```

事後 DB 狀態：`balance=5700`、`version=2`、**流水恰好 2 筆**（重送沒有多寫）、
**outbox 恰好 2 筆 PENDING**，payload 逐欄位正確。

**誠實記錄的負面後果**

- 🔶 **契約測試仍然無法建立**，因為 **Java 沒有任何 HTTP 路徑能建出錢包**
  ——`WalletService.createWallet` 的唯一呼叫端是 `MemberEventListener:30`
  （`member.registered` 事件），`/api/v1/wallet/balance` 查不到就是 404，沒有
  lazy-create。所以契約測試要嘛各自直接寫自己的主庫、要嘛先做 `member.registered`
  的消費者。**這是切片 9 的前置條件，本輪只確認了事實，沒有做決定。**
- 🔶 **沒有 Dockerfile 與 compose service**。`docker-compose.infra.yml` 的檔頭明寫
  「只有基礎設施，沒有業務服務」，硬塞進去會違反它存在的理由。
  目前的跑法是 `go run ./cmd/wallet`。
- 🔶 **`/api/v1/wallet/**` 玩家端點全部未做**（balance / transactions / gift /
  bankruptcy-aid），它們要 `X-User-Id` 而不是 `X-Internal-Secret`。
  其中 gift 與 bankruptcy-aid 是 `docs/notes/` §7 尚未查證的四項之二，
  **動之前必須回讀 Java 原始碼**（CLAUDE.md §5）。
- 🔶 **`/healthz` 不是 Java 契約的一部分**。Java 走 Spring Actuator 的
  `/actuator/health`，這裡刻意不模仿那個 JSON 形狀——為了一個探針把 Actuator
  的結構搬過來，是把 Spring 的實作細節當成契約。探針路徑由部署設定，不由契約決定。
- **順手刪掉一個既有的死碼**：`repository_credit_infra_test.go` 的 `mustCredit`
  從來沒被呼叫過（credit 那一輪留下的，`golangci-lint --build-tags=infra` 唯一的
  issue）。依 CLAUDE.md §3 先回報再刪，且獨立成一個 `chore` commit——
  它不是本切片的一部分，混進同一個 commit 會讓「這一輪改了什麼」失焦。
- **SQL log 預設開著**（`WALLET_SQL_LOG=true`）是刻意的取捨：帳務要看得見 SQL
  （藍圖 §3.2），代價是每筆請求多 3~5 行日誌。壓測時要記得關掉，
  否則量到的會是 log I/O 而不是帳務路徑。

---

## [feat] — 2026-08-02 — wallet credit：讀改寫 + 樂觀鎖，並補上 Java 版沒有的補償回沖

Phase A 的第三個切片。credit 與 debit **形狀不同**——團隊只對 debit 做過
T-090 B2 那次「壓成一條語句」的改寫，credit 到現在仍是 JPA 的讀改寫 + `@Version`
（`WalletService.java:168-260`，已逐行讀過）。本輪刻意**保留那個形狀**，
並在兩個 MySQL 與 PostgreSQL 行為不同的地方自己補上保護。
HTTP 層、`wallet.credit.request` 指令消費者、outbox poller 仍未實作。

**Added**

- **`internal/wallet/store.Repository.Credit`——入帳路徑**。
  冪等快路徑 → 載入錢包 → 樂觀鎖存檔 → 寫流水 → 寫 outbox，全部在**同一筆交易**裡。
  credit **沒有餘額守衛**（是加錢），所以餘額 0 也照入不誤；
  選填解凍以 `max(0, frozen - unfreeze)` 夾住並 `log.Warn`，**不拒絕請求**
  ——對齊 Java（`:196-200`），改成回 400 會讓現有呼叫端從成功變失敗。
- **`domain.Movement.UnfreezeAmount` 與 `NewCredit` 的 unfreeze 參數**，
  含「負數不合法」與「只有 CREDIT 可解凍」兩條驗證。
  ⚠️ 負的解凍會讓 `frozen_amount` **變大** → 可用餘額憑空變小 → 之後下注拿到
  **假的餘額不足**，而 `CHECK (frozen_amount >= 0)` 只擋負數、擋不住這個方向。
- **`CreditResult.FrozenAfter` 是 `*Amount`**（地雷 #33）：Java 在冪等命中時
  明確回 `null`（`:179`、`:229`，註解寫「不重算凍結；以當初入帳結果為準」）。
  用 `Amount` 的話那個 null 會靜靜變成 0——而 0 是一個**合法的凍結金額**，
  呼叫端分不出「沒有這個資訊」與「凍結金額是 0」。
- **地雷 #35 / #36**（見下）與 `docs/ADR-002` 決策 7（a~d 四小節）。

**Changed**

- **`domain.DebitEvent` → `domain.MovementEvent`，`DebitEvent` / `CreditEvent`
  改為型別別名**。Java 的兩個 record（`WalletDebitEvent` / `WalletCreditEvent`）
  component **完全相同**（8 個、同名同序同型，已逐行比對）——那是 Java 沒有型別
  別名的結果，不是刻意的語義區分。複製兩份 Go struct 只會得到兩個必定漂移的定義，
  而漂移的症狀是「某個 topic 的 payload 少一個欄位」，沒有錯誤訊息。
  ⚠️ 用 `=`（別名）而不是 `type X MovementEvent`（新型別）：要的是同一個型別的
  兩個名字，不是兩個需要顯式轉換的型別。
- **debit 的死鎖重試迴圈抽成泛型的 `withRetry`**，credit 共用。
  ⚠️ Go 的**方法不能有型別參數**，所以它是自由函式而不是 `*Repository` 的方法
  ——這是 Java 泛型的類比破功處之一。
- **`debitResultOf` 的共同邏輯抽成 `idempotentHitOf`**：跨玩家碰撞的留痕與
  「餘額欄位為 NULL」的判斷都很難重新推導，複製兩份必定漂移，
  而漂移的症狀是「credit 的碰撞沒人看得到」。

**⭐⭐ 地雷 #35：MySQL 的 1062 不中止交易，所以照抄 Java 的 catch 會重複入帳**

本輪最重要的發現，是地雷 #34 的孿生兄弟——同一個 PG→MySQL 落差、方向相反：
#34 是 MySQL **多**了一個失敗模式，這條是 MySQL **少**了一層保護。

Java 的 Step 5 catch（`:220-233`）撞唯一鍵時直接回查贏家並正常返回。那在
PostgreSQL 上活得下來，靠的是 **PG 的約束違反會讓整筆交易 aborted**——catch 裡
那句回查自己也會炸，於是交易回滾、餘額沒多加。**Java 是被 PG 的語義意外保護的，
不是它自己處理對了**（實際結局是 500）。
複驗依據：`WalletTransaction` 是 `GenerationType.IDENTITY`，`save()` 必須立刻送出
INSERT 才拿得到主鍵，所以例外確實落在 try 區塊內——不是推測。

InnoDB 的 1062 只是**語句級**失敗，交易還活著（`ADR-002` 決策 4）。逐行照抄的結果：
樂觀鎖存檔已經把錢加進去 → INSERT 撞 1062、流水沒寫 → catch 正常 return →
**交易 commit** → **餘額多加一次而流水只有一筆，且沒有任何錯誤訊息**。

⚠️ 這推翻了決策 4 當初的樂觀語氣（「這一條比原版簡單」）——**用在 credit 上它反而
更危險**。解法是與 debit 對稱的 `compensateCredit`，且回沖加回的必須是**實際解凍量**
而不是請求值（兩者因為 clamp 可能不同）。

**⭐ 地雷 #36：credit 同玩家高併發的成功率是 1/N（實測）**

| | debit（條件 UPDATE） | credit（讀改寫 + 樂觀鎖） |
|---|---|---|
| 20 筆同玩家不同鍵併發 | **20 筆全成功**（DB 序列化） | **成功 1、409 十九筆** |
| 熱路徑往返數 | 3（+outbox） | 4（+outbox） |

N 個交易同時讀到 `version = v`，只有一個 UPDATE 得逞。**PostgreSQL 的 EPQ 行為
相同**，所以這是 Java 版的既有行為、不是 MySQL 引入的。
⚠️ **刻意不改**：壓成條件 UPDATE 會讓 409 這個對外行為消失（藍圖 §5 第 8 條）；
store 層也**不自動重試**樂觀鎖衝突——重試權在知道冪等鍵怎麼來的那一層，
悄悄重掉會讓呼叫端再也看不到 409。

**如何驗證**

```
go vet ./... && go vet -tags=infra ./...          # 通過
go test -race ./...                                # 通過
go test -race -tags=infra ./...                    # 通過（wallet/store 27.5s）
```

新增 7 條 credit infra 測試。其中 `TestCreditDupEntryDoesNotDoubleCredit`
是地雷 #35 的證據：拿掉 `compensateCredit` 它會紅在
「balance = 2000, want 1500」。
⚠️ 它**手工重演**往返 2~4 而不是呼叫 `Credit()`——要打中的窗口在往返 1 與往返 2
之間，而兩者都是非鎖定讀，**沒辦法用行鎖把執行緒卡在它們中間**。

⚠️ 過程中試過用 `information_schema.innodb_trx` 的 `LOCK WAIT` 當編排的同步點，
**實測查不到**那筆等待中的交易（goroutine dump 明確顯示它卡在 `applyCredit` 的
`Exec` 上，而 innodb_trx 只列得出對手那一筆）。已改成「確認被測交易尚未結束」，
並在註解記下：這個測試**沒有靜默通過的路徑**——編排若沒成立，credit 會成功，
`want ErrConcurrentModification` 那條斷言直接紅。

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
