package handler

import "github.com/gofiber/fiber/v3"

// Health 是給編排器用的存活探針。
//
// ⚠️ **不是 Java 契約的一部分**：Java 走 Actuator 的 `/actuator/health`。
// 刻意不模仿那個 JSON 形狀——為一個探針搬 Actuator 結構，是把 Spring 的實作細節當成契約。
//
// ⚠️ 刻意**不查 DB**：存活探針回答的是「進程還活著嗎」。探針裡查 DB 會讓
// 「DB 抖一下」變成「編排器把服務殺掉重啟」，而重啟解決不了 DB 的問題。
func (h *Handler) Health(c fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"status": "ok"})
}
