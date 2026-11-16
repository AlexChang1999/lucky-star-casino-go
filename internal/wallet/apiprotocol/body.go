// Package apiprotocol 是 wallet 對外 HTTP 契約的單一出處：回應信封與對外訊息文字。
//
// 這個套件不認得 fiber、也不認得 service：它只描述「線上看得到的東西」。
// 狀態碼（HTTP status）由 handler 決定，因為 Java 版的結果是「HTTP 狀態 + 訊息」
// 而不是自訂的數字碼；訊息文字則逐字對齊 Java，契約測試會 diff 它們。
package apiprotocol

// Body 對齊 Java 的 `com.luckystar.wallet.common.ApiResponse`。
//
// ⚠️ 三個欄位都**不可以加 omitempty**（AGENTS.md 地雷 #33）。Java 沒設 `NON_NULL`，
// 成功時輸出 `"message": null`、失敗時輸出 `"data": null`。加了 omitempty 欄位會
// 整個消失——契約測試 diff 到的是「少一個 key」，下游若用 `"message" in resp`
// 判斷，行為會靜靜地改變。
type Body struct {
	Success bool    `json:"success"`
	Data    any     `json:"data"`
	Message *string `json:"message"`
}

// OK 是成功信封：data 帶結果，message 為 null。
func OK(data any) Body {
	return Body{Success: true, Data: data}
}

// Fail 是失敗信封：data 為 null，message 帶對外訊息。
func Fail(message string) Body {
	return Body{Success: false, Message: &message}
}
