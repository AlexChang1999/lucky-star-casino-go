//go:build contract

package contract

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ⚠️ 玩家 ID 用一個**不會與任何真實資料重疊**的區段。
// 團隊 repo 的 `seed_test_data.sql` 會塞進小號碼的測試玩家，
// 兩邊撞在一起的症狀是「昨天綠的測試今天紅了」，而程式碼一個字都沒改。
const contractPlayerBase = 9_900_000

// 每個測試一個專屬玩家：測試之間不共用狀態，所以順序、平行、單獨重跑
// 都不會互相影響。⚠️ 契約測試最常見的 flaky 來源就是共用一個玩家。
const (
	playerDebitHappy     = contractPlayerBase + 1
	playerDebitIdem      = contractPlayerBase + 2
	playerInsufficient   = contractPlayerBase + 3
	playerNoWallet       = contractPlayerBase + 4
	playerCreditHappy    = contractPlayerBase + 5
	playerCreditIdem     = contractPlayerBase + 6
	playerValidation     = contractPlayerBase + 7
	playerMalformed      = contractPlayerBase + 8
	playerSubTypeDefault = contractPlayerBase + 9
	playerAuth           = contractPlayerBase + 10
)

const (
	debitPath  = "/internal/wallet/debit"
	creditPath = "/internal/wallet/credit"
)

// setup 載入目標並確認它已就緒。
func setup(t *testing.T) target {
	t.Helper()
	tg := loadTarget(t)
	tg.waitForService(t)
	t.Logf("契約測試目標：%s（%s）", tg.name, tg.baseURL)
	return tg
}

// ── ① 授權 ─────────────────────────────────────────────────────────────────

// TestInternalEndpointsRequireSecret 釘住 `/internal/**` 的守門。
//
// ⚠️ 第三格（不存在的 /internal 路徑）不是湊數的：它問的是
// 「驗證跑在路由**之前**還是之後」。跑在之後的話，沒有 secret 的人可以靠
// 「回 401 還是 404」把內部端點一個一個探出來——而功能測試全部都會過。
func TestInternalEndpointsRequireSecret(t *testing.T) {
	tg := setup(t)

	body := fmt.Sprintf(`{"playerId":%d,"amount":1,"idempotencyKey":"auth-probe"}`, playerAuth)

	tests := []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{"完全不帶 header", debitPath, nil},
		{"帶錯的 secret", debitPath, map[string]string{"X-Internal-Secret": "wrong-secret"}},
		{"帶空字串", debitPath, map[string]string{"X-Internal-Secret": ""}},
		{"不存在的 internal 路徑（驗證必須在路由之前）", "/internal/wallet/nope", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tg.postWithHeaders(t, tt.path, body, tt.headers)
			got.requireStatus(t, http.StatusUnauthorized)
			if got.body.Success {
				t.Errorf("success 應該是 false；完整回應:\n%s", got.rawBody)
			}
			if msg := got.message(); msg != "Unauthorized" {
				t.Errorf("message = %q, want %q；完整回應:\n%s", msg, "Unauthorized", got.rawBody)
			}
		})
	}
}

// ── ② debit ────────────────────────────────────────────────────────────────

func TestDebitHappyPath(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerDebitHappy, 1000)

	got := tg.post(t, debitPath, fmt.Sprintf(
		`{"playerId":%d,"amount":300,"idempotencyKey":"contract-debit-happy","referenceId":"round-1"}`,
		playerDebitHappy))
	got.requireStatus(t, http.StatusOK)

	if !got.body.Success {
		t.Errorf("success 應該是 true；完整回應:\n%s", got.rawBody)
	}
	if got.body.Message != nil {
		t.Errorf("成功時 message 應該是 null，得到 %q", *got.body.Message)
	}

	d := got.data(t)
	assertEqual(t, "playerId", d.PlayerID, int64(playerDebitHappy))
	assertEqual(t, "amount", d.Amount, int64(300))
	assertEqual(t, "balanceBefore", d.BalanceBefore, int64(1000))
	assertEqual(t, "balanceAfter", d.BalanceAfter, int64(700))
	if d.Idempotent {
		t.Error("第一次扣款不該是冪等命中")
	}
	if d.TransactionID <= 0 {
		t.Errorf("transactionId = %d，應該是正整數", d.TransactionID)
	}

	// ⭐ 回應說了什麼是一回事，DB 裡是什麼是另一回事。
	// 只信回應的話，「回應算對了但沒存進去」這種 bug 是測不出來的。
	assertEqual(t, "DB 餘額", tg.balanceOf(t, playerDebitHappy), int64(700))
	assertEqual(t, "流水筆數", int64(tg.countTransactions(t, playerDebitHappy)), int64(1))
}

// ⭐ TestDebitIsIdempotent 是整份契約測試最重要的一格。
//
// 冪等鍵是帳務的核心（AGENTS.md 地雷 #3）：同一把鍵重送必須**不再扣一次**，
// 而且要回**當初那一筆**的結果。⚠️ 只斷言 `idempotent: true` 是不夠的——
// 那是實作自己說的。真正的證據是**流水只有一筆、餘額只少了一次**。
func TestDebitIsIdempotent(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerDebitIdem, 1000)

	body := fmt.Sprintf(
		`{"playerId":%d,"amount":250,"idempotencyKey":"contract-debit-idem","referenceId":"round-2"}`,
		playerDebitIdem)

	first := tg.post(t, debitPath, body)
	first.requireStatus(t, http.StatusOK)
	firstData := first.data(t)

	second := tg.post(t, debitPath, body)
	second.requireStatus(t, http.StatusOK)
	secondData := second.data(t)

	if !secondData.Idempotent {
		t.Errorf("重送同一把冪等鍵應該回 idempotent=true；完整回應:\n%s", second.rawBody)
	}
	assertEqual(t, "第二次的 transactionId（應該是當初那一筆）",
		secondData.TransactionID, firstData.TransactionID)
	assertEqual(t, "第二次的 balanceAfter", secondData.BalanceAfter, firstData.BalanceAfter)

	assertEqual(t, "DB 餘額（只該扣一次）", tg.balanceOf(t, playerDebitIdem), int64(750))
	assertEqual(t, "流水筆數（只該有一筆）",
		int64(tg.countTransactions(t, playerDebitIdem)), int64(1))
}

func TestDebitInsufficientBalance(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerInsufficient, 100)

	got := tg.post(t, debitPath, fmt.Sprintf(
		`{"playerId":%d,"amount":500,"idempotencyKey":"contract-debit-insufficient"}`,
		playerInsufficient))

	// ⚠️ **422 不是 400**。Java 的 GlobalExceptionHandler 對
	// InsufficientBalanceException 明寫 @ResponseStatus(UNPROCESSABLE_ENTITY)。
	// 語義上也說得通：JSON 語法對、欄位全部合法，是**業務狀態**不允許。
	got.requireStatus(t, http.StatusUnprocessableEntity)
	assertMessage(t, got, "Insufficient balance")

	assertEqual(t, "餘額不足時餘額不可以變", tg.balanceOf(t, playerInsufficient), int64(100))
	assertEqual(t, "餘額不足時不可以留下流水",
		int64(tg.countTransactions(t, playerInsufficient)), int64(0))
}

func TestDebitWalletNotFound(t *testing.T) {
	tg := setup(t)
	tg.deleteWallet(t, playerNoWallet)

	got := tg.post(t, debitPath, fmt.Sprintf(
		`{"playerId":%d,"amount":10,"idempotencyKey":"contract-debit-404"}`, playerNoWallet))

	got.requireStatus(t, http.StatusNotFound)
	// 訊息帶 playerId，逐字對齊 Java 的
	// `"Wallet not found for player: " + request.getPlayerId()`。
	assertMessage(t, got, fmt.Sprintf("Wallet not found for player: %d", playerNoWallet))
}

// TestDebitDefaultsSubTypeToBet 釘住「不帶 subType 就是 BET」。
//
// ⚠️ 這條只能從 DB 看：回應裡沒有 subType 欄位，所以少了這個查證，
// 「預設值變成別的東西」會完全隱形——直到有人去看後台報表，
// 發現有一整類交易不見了（地雷 #8 的四同步就是在講這個）。
func TestDebitDefaultsSubTypeToBet(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerSubTypeDefault, 500)

	got := tg.post(t, debitPath, fmt.Sprintf(
		`{"playerId":%d,"amount":50,"idempotencyKey":"contract-debit-subtype-default"}`,
		playerSubTypeDefault))
	got.requireStatus(t, http.StatusOK)

	subType := tg.execSQL(t, fmt.Sprintf(
		"SELECT sub_type FROM wallet_transactions WHERE player_id = %d;", playerSubTypeDefault))
	if strings.TrimSpace(subType) != "BET" {
		t.Errorf("不帶 subType 時應該記成 BET，DB 裡是 %q", subType)
	}
}

// ── ③ credit ───────────────────────────────────────────────────────────────

func TestCreditHappyPath(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerCreditHappy, 1000)

	got := tg.post(t, creditPath, fmt.Sprintf(
		`{"playerId":%d,"amount":250,"subType":"WIN","idempotencyKey":"contract-credit-happy"}`,
		playerCreditHappy))
	got.requireStatus(t, http.StatusOK)

	d := got.data(t)
	assertEqual(t, "balanceBefore", d.BalanceBefore, int64(1000))
	assertEqual(t, "balanceAfter", d.BalanceAfter, int64(1250))
	if d.Idempotent {
		t.Error("第一次入帳不該是冪等命中")
	}
	assertEqual(t, "DB 餘額", tg.balanceOf(t, playerCreditHappy), int64(1250))
}

// TestCreditIsIdempotent 除了「不重複入帳」，還釘住一個 Go 很容易寫壞的地方：
// **冪等命中時 `frozenAfter` 必須是 `null` 而不是 `0`**（地雷 #33）。
//
// Java 明確回 null（「不重算凍結；以當初入帳結果為準」）。Go 若用 `int64`
// 而不是 `*int64`，那個 null 會靜靜變成 0——而 0 是一個**合法的凍結金額**，
// 呼叫端分不出「沒有這個資訊」與「凍結金額真的是 0」。
func TestCreditIsIdempotent(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerCreditIdem, 1000)

	body := fmt.Sprintf(
		`{"playerId":%d,"amount":400,"subType":"WIN","idempotencyKey":"contract-credit-idem"}`,
		playerCreditIdem)

	first := tg.post(t, creditPath, body)
	first.requireStatus(t, http.StatusOK)
	firstData := first.data(t)

	second := tg.post(t, creditPath, body)
	second.requireStatus(t, http.StatusOK)
	secondData := second.data(t)

	if !secondData.Idempotent {
		t.Errorf("重送同一把冪等鍵應該回 idempotent=true；完整回應:\n%s", second.rawBody)
	}
	if secondData.FrozenAfter != nil {
		t.Errorf("冪等命中時 frozenAfter 必須是 null，得到 %d；完整回應:\n%s",
			*secondData.FrozenAfter, second.rawBody)
	}
	assertEqual(t, "第二次的 transactionId", secondData.TransactionID, firstData.TransactionID)

	assertEqual(t, "DB 餘額（只該加一次）", tg.balanceOf(t, playerCreditIdem), int64(1400))
	assertEqual(t, "流水筆數（只該有一筆）",
		int64(tg.countTransactions(t, playerCreditIdem)), int64(1))
}

// ── ④ 請求驗證 ──────────────────────────────────────────────────────────────

// TestValidationRejectsBadRequests 逐格對照 Bean Validation 的**訊息文字**。
//
// ⭐ 為什麼連訊息都要 diff：那是**對外契約**的一部分，呼叫端會拿它顯示給人看。
// 而且訊息裡藏著一個看起來像 bug 其實是對的地方——自訂的 @Pattern 訊息本身
// 就以欄位名開頭，串上 handler 的 `"Invalid request: " + field + " "` 之後
// 欄位名會出現**兩次**：`Invalid request: subType subType must be one of ...`。
// Go 版照抄了這個重複（CLAUDE.md §5：等價 > 品味），這一格就是它的證據。
func TestValidationRejectsBadRequests(t *testing.T) {
	tg := setup(t)
	tg.seedWallet(t, playerValidation, 1000)

	tests := []struct {
		name    string
		path    string
		body    string
		message string
	}{
		{
			"amount 是 null",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":null,"idempotencyKey":"contract-v1"}`, playerValidation),
			"Invalid request: amount must not be null",
		},
		{
			"amount 是 0",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":0,"idempotencyKey":"contract-v2"}`, playerValidation),
			"Invalid request: amount must be greater than 0",
		},
		{
			"amount 是負數",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":-5,"idempotencyKey":"contract-v3"}`, playerValidation),
			"Invalid request: amount must be greater than 0",
		},
		{
			"playerId 是 null",
			debitPath,
			`{"playerId":null,"amount":10,"idempotencyKey":"contract-v4"}`,
			"Invalid request: playerId must not be null",
		},
		{
			"idempotencyKey 是空字串",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"idempotencyKey":""}`, playerValidation),
			"Invalid request: idempotencyKey must not be blank",
		},
		{
			// ⚠️ 全空白：@NotBlank 是 **trim 之後**不可為空。少了這條，
			// 「   」會變成一把由三個空白組成的合法冪等鍵，而且真的會寫進流水。
			"idempotencyKey 全是空白",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"idempotencyKey":"   "}`, playerValidation),
			"Invalid request: idempotencyKey must not be blank",
		},
		{
			"idempotencyKey 超過 100 字元",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"idempotencyKey":"%s"}`,
				playerValidation, strings.Repeat("k", 101)),
			"Invalid request: idempotencyKey size must be between 0 and 100",
		},
		{
			"referenceId 超過 100 字元",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"idempotencyKey":"contract-v5","referenceId":"%s"}`,
				playerValidation, strings.Repeat("r", 101)),
			"Invalid request: referenceId size must be between 0 and 100",
		},
		{
			// ⚠️ 空字串與「不帶」是兩件事：不帶 → 預設 BET；帶空字串 → @Pattern 不符。
			// 混為一談的話，一筆本該被拒絕的請求會靜靜變成一筆合法的下注。
			"debit 的 subType 是空字串",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"subType":"","idempotencyKey":"contract-v6"}`, playerValidation),
			"Invalid request: subType subType must be one of BET/SHOP_PURCHASE",
		},
		{
			"debit 的 subType 不在白名單（WIN 是 credit 的）",
			debitPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"subType":"WIN","idempotencyKey":"contract-v7"}`, playerValidation),
			"Invalid request: subType subType must be one of BET/SHOP_PURCHASE",
		},
		{
			"credit 不帶 subType（credit 沒有預設值）",
			creditPath,
			fmt.Sprintf(`{"playerId":%d,"amount":10,"idempotencyKey":"contract-v8"}`, playerValidation),
			"Invalid request: subType must not be blank",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tg.post(t, tt.path, tt.body)
			got.requireStatus(t, http.StatusBadRequest)
			assertMessage(t, got, tt.message)
		})
	}

	// 驗證失敗的請求一筆都不該落庫。
	assertEqual(t, "驗證失敗不可以留下流水",
		int64(tg.countTransactions(t, playerValidation)), int64(0))
	assertEqual(t, "驗證失敗不可以改餘額", tg.balanceOf(t, playerValidation), int64(1000))
}

// ── ⑤ 已知的刻意分歧（藍圖 §5）──────────────────────────────────────────────

// TestMalformedJSON 是藍圖 §5 第 9 條的**複驗**。
//
// 那一條當初標著「Java 行為是讀原始碼推導的，尚未對跑起來的實例複驗」。
// 對 java 目標跑這個測試就是複驗：紅了代表推導錯了，而那比綠更有價值。
func TestMalformedJSON(t *testing.T) {
	tg := setup(t)

	got := tg.post(t, debitPath, `{"playerId":`+fmt.Sprint(playerMalformed)+`,"amount":`)
	got.requireStatus(t, tg.malformedJSONStatus)
	if got.body.Success {
		t.Errorf("success 應該是 false；完整回應:\n%s", got.rawBody)
	}
}

// TestZeroPlayerID 是藍圖 §5 第 10 條的複驗（Java 404 / Go 400）。
//
// ⚠️ 這裡只斷言狀態碼：兩邊的訊息本來就不同（Go 是驗證訊息、Java 是
// 「查不到錢包」），把訊息也寫進 target 只會讓那個結構長出一堆
// 「只有一格會用到」的欄位。狀態碼才是呼叫端真正會分支的東西。
func TestZeroPlayerID(t *testing.T) {
	tg := setup(t)

	got := tg.post(t, debitPath, `{"playerId":0,"amount":10,"idempotencyKey":"contract-zero-player"}`)
	got.requireStatus(t, tg.zeroPlayerIDStatus)
	if got.body.Success {
		t.Errorf("success 應該是 false；完整回應:\n%s", got.rawBody)
	}
}

// ── 小工具 ─────────────────────────────────────────────────────────────────

func assertEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func assertMessage(t *testing.T, r httpResult, want string) {
	t.Helper()
	if r.body.Success {
		t.Errorf("success 應該是 false；完整回應:\n%s", r.rawBody)
	}
	if got := r.message(); got != want {
		t.Errorf("message =\n  %q\nwant\n  %q\n完整回應:\n%s", got, want, r.rawBody)
	}
}
