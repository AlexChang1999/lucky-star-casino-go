//go:build contract

// Package contract 是**跨語言黑箱契約測試**：同一份測試碼，對 Java 版 wallet
// 與 Go 版 wallet 各跑一次，兩邊都必須綠。
//
// ⭐ 這是整個重構唯一的「等價」證據（藍圖 §2 原則 4、§7 的 Phase DoD）。
// 在它存在之前，「Go 版與 Java 版行為相同」這句話的依據只是
// **「我很仔細地讀過 Java 原始碼」**——那不是證據，那是意圖。
//
// ⚠️ **黑箱**這兩個字是硬性的，有三條規矩：
//
//  1. **不 import 本專案的任何 internal 套件**。共用型別就等於共用 bug：
//     兩邊都用同一個 `debitRequest` struct 的話，欄位名打錯會在兩邊
//     以完全相同的方式錯掉，而測試是綠的。這裡的 JSON 一律手寫字串。
//  2. **只透過 HTTP 觀察**。斷言只能用狀態碼、回應 body、以及「事後從 DB
//     數出來的列數」——不可以呼叫實作的函式。
//  3. **兩個目標的差異只能出現在 target 這個結構裡**，測試本體不准出現
//     `if target == "java"`。這條的價值見下方 malformedJSONStatus。
//
// 跑法見 test/contract/README.md。
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── 目標設定 ────────────────────────────────────────────────────────────────

// target 是「要打哪一個實作」的完整描述。
//
// ⭐ **已知的刻意分歧一律是這個結構的欄位**（目前有兩個，都在藍圖 §5 有案）。
// 這是本檔最重要的設計：契約測試的產出不只是「兩邊都綠」，而是
// **把不等價的地方逼成一份可以數得出來的清單**。
// 寫成 `if target.name == "java"` 散在各個測試裡的話，分歧會變成
// 「某幾行 if」——沒有人數得出來總共有幾個，也沒有人會去審查它們該不該存在。
type target struct {
	name       string
	baseURL    string
	secret     string
	healthPath string

	// seed 用的資料庫連線（透過 docker exec，見 execSQL 的說明）
	dbContainer string
	dbUser      string
	dbPassword  string
	dbName      string
	dbCLI       string // "mysql" 或 "psql"

	// ── 已知的刻意分歧 ──────────────────────────────────────────────────
	//
	// malformedJSONStatus：**壞掉的 JSON 該回什麼狀態碼**。
	//   Java 500 / Go 400（藍圖 §5 第 9 條）。
	//
	// Java 的 `GlobalExceptionHandler` 沒有處理 `HttpMessageNotReadableException`，
	// 但它註冊了 `@ExceptionHandler(Exception.class)`——於是
	// `ExceptionHandlerExceptionResolver` 搶在 Spring 內建的
	// `DefaultHandlerExceptionResolver`（那個才回 400）之前接住它，變成 500。
	// Go 版回 400：「呼叫端送了壞 JSON，伺服器不該說自己壞了」。
	//
	// ⚠️ 這一格正是藍圖 §5 標註「**尚未對跑起來的實例複驗**」的那一條。
	// 對 java 目標跑這個測試就是複驗——它紅了代表當初讀原始碼推導錯了，
	// 那比「測試綠」更有價值。
	malformedJSONStatus int

	// zeroPlayerIDStatus：**playerId 傳 0 該回什麼**。
	//   Java 404 / Go 400（藍圖 §5 第 10 條）。
	//
	// Java 的 `DebitRequest.playerId` 只有 `@NotNull` 沒有 `@Positive`，
	// 於是 0 一路走到「查不到錢包」→ 404。Go 版在 domain 就擋下來 → 400：
	// 非正整數的玩家 ID 不可能有錢包，「請求有問題」比「資源不存在」準確。
	// ⚠️ 影響面為零——真實呼叫端（game-service）送的都是真的 ID。
	zeroPlayerIDStatus int
}

// loadTarget 從環境變數組出目標設定。
//
// ⚠️ `CONTRACT_TARGET` **沒有預設值**，這是刻意的：契約測試的整個意義在於
// 「同一份測試跑了哪一個實作」，讓它有預設值就等於允許「我以為我測了 Java，
// 其實測的是 Go」——而那種錯誤跑完是綠的。
func loadTarget(t *testing.T) target {
	t.Helper()

	switch name := os.Getenv("CONTRACT_TARGET"); name {
	case "go":
		return target{
			name:       "go",
			baseURL:    envOr("CONTRACT_BASE_URL", "http://localhost:8182"),
			secret:     requireEnv(t, "INTERNAL_SECRET", "set -a && . deploy/.env && set +a"),
			healthPath: "/healthz",

			dbContainer: envOr("CONTRACT_DB_CONTAINER", "casino-go-mysql"),
			dbUser:      requireEnv(t, "MYSQL_USER", "set -a && . deploy/.env && set +a"),
			dbPassword:  requireEnv(t, "MYSQL_PASSWORD", "set -a && . deploy/.env && set +a"),
			dbName:      requireEnv(t, "MYSQL_DATABASE", "set -a && . deploy/.env && set +a"),
			dbCLI:       "mysql",

			malformedJSONStatus: http.StatusBadRequest,
			zeroPlayerIDStatus:  http.StatusBadRequest,
		}
	case "java":
		return target{
			name:    "java",
			baseURL: envOr("CONTRACT_BASE_URL", "http://localhost:8082"),
			secret:  requireEnv(t, "INTERNAL_SECRET", "set -a && . /h/Lucky_Star_Casino/.env && set +a"),
			// ⚠️ Java 走 Spring Actuator，Go 版刻意不模仿那個形狀
			// （為了一個探針把 Actuator 的 JSON 結構搬過來，是把 Spring 的
			// 實作細節當成契約）。所以路徑放在 target 裡，兩邊各自設定。
			healthPath: "/actuator/health",

			dbContainer: envOr("CONTRACT_DB_CONTAINER", "lucky-star-postgres"),
			dbUser:      requireEnv(t, "POSTGRES_USER", "set -a && . /h/Lucky_Star_Casino/.env && set +a"),
			dbPassword:  requireEnv(t, "POSTGRES_PASSWORD", "set -a && . /h/Lucky_Star_Casino/.env && set +a"),
			dbName:      requireEnv(t, "POSTGRES_DB", "set -a && . /h/Lucky_Star_Casino/.env && set +a"),
			dbCLI:       "psql",

			malformedJSONStatus: http.StatusInternalServerError,
			zeroPlayerIDStatus:  http.StatusNotFound,
		}
	case "":
		t.Fatal("CONTRACT_TARGET 未設定。契約測試必須明說在測哪一個實作：\n" +
			"  CONTRACT_TARGET=go    對 Go 版（:8182）\n" +
			"  CONTRACT_TARGET=java  對 Java 版（:8082）\n" +
			"有預設值的話，「我以為我測了 Java，其實測的是 Go」跑完會是綠的。")
		return target{}
	default:
		t.Fatalf("CONTRACT_TARGET=%q 不認得，只能是 go 或 java", name)
		return target{}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func requireEnv(t *testing.T, key, howToFix string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("環境變數 %s 未設定。先跑：\n  %s", key, howToFix)
	}
	return v
}

// ── HTTP ────────────────────────────────────────────────────────────────────

// apiResponse 是兩版共用的回應信封（Java 的 ApiResponse）。
//
// ⚠️ Message 是 *string 而不是 string（地雷 #33）：成功時 Java 回
// `"message": null`，用 string 接會變成 ""，於是「沒有訊息」與「空訊息」
// 分不出來——而契約測試正是要分出這種差別的地方。
type apiResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Message *string         `json:"message"`
}

// movementData 是 debit / credit 成功時 data 的形狀。
//
// ⚠️ FrozenAfter 是 *int64：冪等命中時 Java 明確回 null
// （「不重算凍結；以當初入帳結果為準」）。用 int64 接會變成 0，
// 而 0 是一個**合法的凍結金額**。
type movementData struct {
	TransactionID int64  `json:"transactionId"`
	PlayerID      int64  `json:"playerId"`
	Amount        int64  `json:"amount"`
	BalanceBefore int64  `json:"balanceBefore"`
	BalanceAfter  int64  `json:"balanceAfter"`
	FrozenAfter   *int64 `json:"frozenAfter"`
	Idempotent    bool   `json:"idempotent"`
}

// httpResult 是一次呼叫的原始結果。保留 rawBody 是為了讓失敗訊息能把
// 整個 body 印出來——契約測試紅掉時，「兩邊哪裡不一樣」全在那串字裡。
type httpResult struct {
	status  int
	body    apiResponse
	rawBody string
}

// post 送一個帶 secret 的請求。body 是**手寫的 JSON 字串**，不是 struct——
// 見套件註解第 1 條：共用型別就等於共用 bug。
func (tg target) post(t *testing.T, path, body string) httpResult {
	t.Helper()
	return tg.postWithHeaders(t, path, body, map[string]string{"X-Internal-Secret": tg.secret})
}

func (tg target) postWithHeaders(t *testing.T, path, body string, headers map[string]string) httpResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tg.baseURL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("組請求失敗: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("打 %s%s 失敗（服務起來了嗎？）: %v", tg.baseURL, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("讀回應失敗: %v", err)
	}

	result := httpResult{status: resp.StatusCode, rawBody: string(raw)}
	// ⚠️ 解不開**不**當成失敗：回應信封本身就是契約的一部分，
	// 解不開時要讓斷言去報「body 長得不對」並印出原文，
	// 而不是在這裡丟一個看不出上下文的 JSON 錯誤。
	_ = json.Unmarshal(raw, &result.body)
	return result
}

// data 把 data 欄位解成 movementData。
func (r httpResult) data(t *testing.T) movementData {
	t.Helper()
	var d movementData
	if err := json.Unmarshal(r.body.Data, &d); err != nil {
		t.Fatalf("data 欄位不是預期的形狀（%v）；完整回應:\n%s", err, r.rawBody)
	}
	return d
}

// message 回傳 message 欄位，nil 時回 "<null>" 讓失敗訊息看得懂。
func (r httpResult) message() string {
	if r.body.Message == nil {
		return "<null>"
	}
	return *r.body.Message
}

// requireStatus 是最常用的斷言，失敗時把整個 body 印出來。
func (r httpResult) requireStatus(t *testing.T, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("狀態碼 = %d, want %d；完整回應:\n%s", r.status, want, r.rawBody)
	}
}

// waitForService 等服務就緒，最多 30 秒。
//
// ⚠️ 不是為了「等它慢慢起來」而存在，是為了**讓失敗訊息說人話**：
// 沒有它的話，服務沒起來時第一個測試會報 connection refused，
// 而那看起來像測試壞了而不是「你忘了 go run ./cmd/wallet」。
func (tg target) waitForService(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tg.baseURL+tg.healthPath, nil)
		if err != nil {
			cancel()
			t.Fatalf("組健康檢查請求失敗: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			cancel()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("健康檢查回 %d", resp.StatusCode)
		} else {
			lastErr = err
			cancel()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等不到 %s 目標就緒（%s%s）: %v\n"+
		"go 目標要先 `go run ./cmd/wallet`；java 目標要先起團隊 repo 的 wallet-service。",
		tg.name, tg.baseURL, tg.healthPath, lastErr)
}

// ── 資料庫（seed 與事後查證）────────────────────────────────────────────────

// execSQL 透過 `docker exec` 對目標的資料庫下 SQL。
//
// ⭐ 為什麼是 docker exec 而不是 Go 的資料庫驅動：
//   - **一種機制對兩個目標**。Go 版是 MySQL、Java 版是 PostgreSQL，
//     用驅動就要引入 pgx 這個只有測試會用到的依賴，而且兩條路徑會各自長大。
//   - **SQL 保持是看得見的字串**。seed 與查證語句在這個檔案裡是一整句
//     可以複製去貼進 client 執行的 SQL，不是一串 ORM 呼叫。
//
// ⚠️ 密碼走環境變數（MYSQL_PWD / PGPASSWORD）而不是命令列參數：
// 命令列參數會出現在容器內的 `ps` 輸出裡。
func (tg target) execSQL(t *testing.T, sql string) string {
	t.Helper()

	var args []string
	switch tg.dbCLI {
	case "mysql":
		// -N -B：不要表頭、用 tab 分隔——輸出就是一個裸值，好解析。
		args = []string{
			"exec", "-e", "MYSQL_PWD=" + tg.dbPassword, tg.dbContainer,
			"mysql", "-u" + tg.dbUser, "-N", "-B", tg.dbName, "-e", sql,
		}
	case "psql":
		// -t -A：同上（tuples only + unaligned）。
		// ON_ERROR_STOP=1 讓 SQL 出錯時 psql 回非零，否則錯誤只會印出來而
		// 退出碼是 0——seed 失敗卻繼續跑測試，症狀是後面每一格都莫名其妙。
		args = []string{
			"exec", "-e", "PGPASSWORD=" + tg.dbPassword, tg.dbContainer,
			"psql", "-U", tg.dbUser, "-d", tg.dbName, "-t", "-A", "-v", "ON_ERROR_STOP=1", "-c", sql,
		}
	default:
		t.Fatalf("不認得的 dbCLI: %q", tg.dbCLI)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("對 %s 執行 SQL 失敗: %v\nSQL: %s\n輸出: %s", tg.dbContainer, err, sql, out)
	}
	return strings.TrimSpace(string(out))
}

// seedWallet 把某個玩家的錢包重設成指定餘額，並清掉他的流水與 outbox。
//
// ⚠️ **為什麼契約測試需要直接寫 DB**：Java 版沒有任何 HTTP 端點可以建錢包——
// 唯一的路徑是 `member.registered` 事件（`MemberEventListener:30`），
// 而那條路徑本身還沒被重寫。所以「準備一個有餘額的錢包」只能從 DB 進去。
// 這不違反黑箱：**被觀察的行為**全部走 HTTP，DB 只用來擺放前置狀態與事後點算。
//
// ⚠️ 三句話的順序不可以換：流水先刪（它可能有外鍵指向 wallets），
// 再刪錢包，最後才插入。
//
// ⚠️ **刻意不清 wallet_outbox**，兩個理由：
//   - 沒有任何斷言看 outbox，留著的列只會被 poller 正常投遞掉
//   - 團隊那顆 PG volume 早於 outbox 上線（2026-07-21），**根本沒有這張表**
//     ——那是地雷 #17 的活體標本（initdb.d 只在 volume 全新時跑）。
//     為了清一張沒人斷言的表而讓整個 seed 失敗，是把測試綁在環境的歷史上。
func (tg target) seedWallet(t *testing.T, playerID, balance int64) {
	t.Helper()
	tg.execSQL(t, fmt.Sprintf(
		`DELETE FROM wallet_transactions WHERE player_id = %d;
		 DELETE FROM wallets WHERE player_id = %d;
		 INSERT INTO wallets (player_id, balance, frozen_amount, version) VALUES (%d, %d, 0, 0);`,
		playerID, playerID, playerID, balance))
}

// deleteWallet 確保某個玩家**沒有**錢包（給 404 那格用）。
func (tg target) deleteWallet(t *testing.T, playerID int64) {
	t.Helper()
	tg.execSQL(t, fmt.Sprintf(
		`DELETE FROM wallet_transactions WHERE player_id = %d;
		 DELETE FROM wallets WHERE player_id = %d;`, playerID, playerID))
}

// countTransactions 數某個玩家的流水筆數。
//
// ⭐ 這是冪等性唯一誠實的驗證方式：HTTP 回應說「idempotent: true」是
// **實作自己說的**，而流水只有一筆才是真的沒有重複入帳。
func (tg target) countTransactions(t *testing.T, playerID int64) int {
	t.Helper()
	out := tg.execSQL(t, fmt.Sprintf(
		"SELECT COUNT(*) FROM wallet_transactions WHERE player_id = %d;", playerID))
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("流水筆數解析失敗（輸出 %q）: %v", out, err)
	}
	return n
}

// balanceOf 直接從 DB 讀餘額，用來確認 HTTP 回應說的與實際落庫的一致。
func (tg target) balanceOf(t *testing.T, playerID int64) int64 {
	t.Helper()
	out := tg.execSQL(t, fmt.Sprintf(
		"SELECT balance FROM wallets WHERE player_id = %d;", playerID))
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("餘額解析失敗（輸出 %q）: %v", out, err)
	}
	return n
}
