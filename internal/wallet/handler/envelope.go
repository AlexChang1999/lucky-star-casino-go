package handler

import "github.com/gofiber/fiber/v3"

// 對外訊息。逐字對齊 Java 的 GlobalExceptionHandler，契約測試會 diff 它們。
const (
	internalErrorMessage = "Internal server error"
	unauthorizedMessage  = "Unauthorized"

	// ⚠️ malformedBodyMessage 是**本專案自己的**訊息，Java 沒有對應物（見 debit.go 的註解）。
	malformedBodyMessage = "Invalid request body"
)

// envelope 對齊 Java 的 `com.luckystar.wallet.common.ApiResponse`。
//
// ⚠️ 三個欄位都**不可以加 omitempty**（AGENTS.md 地雷 #33）。Java 沒設 `NON_NULL`，
// 成功時輸出 `"message": null`、失敗時輸出 `"data": null`。加了 omitempty 欄位會
// 整個消失——契約測試 diff 到的是「少一個 key」，下游若用 `"message" in resp`
// 判斷，行為會靜靜地改變。
type envelope struct {
	Success bool    `json:"success"`
	Data    any     `json:"data"`
	Message *string `json:"message"`
}

func respondOK(c fiber.Ctx, data any) error {
	return c.Status(fiber.StatusOK).JSON(envelope{Success: true, Data: data})
}

func respondError(c fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(envelope{Success: false, Message: &message})
}
