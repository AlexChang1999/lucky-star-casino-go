package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/apiprotocol"
)

func respondOK(c fiber.Ctx, data any) error {
	return c.Status(fiber.StatusOK).JSON(apiprotocol.OK(data))
}

func respondError(c fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(apiprotocol.Fail(message))
}
