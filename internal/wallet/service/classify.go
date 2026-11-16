package service

import (
	"errors"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// classify 把 store 的 sentinel 翻成 *Error，並在**這裡**記一次日誌（錯誤的起點）。
// handler 之後只看 Code，不再記同一件事。
//
// ⭐ 這張表來自逐行讀過的 Java `GlobalExceptionHandler`：
//
//	ErrWalletNotFound         → 找不到錢包
//	ErrInsufficientBalance    → 餘額不足
//	ErrConcurrentModification → 樂觀鎖衝突
//	其餘                       → Internal（含 ErrIdempotencyWinnerMissing 等）
func (s *Service) classify(err error, m domain.Movement) *Error {
	switch {
	case errors.Is(err, walletstore.ErrWalletNotFound):
		return &Error{Code: CodeWalletNotFound, Err: err}

	case errors.Is(err, walletstore.ErrInsufficientBalance):
		return &Error{Code: CodeInsufficientBalance, Err: err}

	case errors.Is(err, walletstore.ErrConcurrentModification):
		// ⚠️ 用 Warn 而不是 Error：credit 是讀改寫 + 樂觀鎖，同玩家高併發下成功率是 1/N
		// （地雷 #36），409 在那個情境是**常態**。記成 Error 會讓告警一直響，
		// 然後大家開始忽略告警——那比沒有告警更糟。
		s.logger.Warn("樂觀鎖衝突，呼叫端應帶原本那把冪等鍵重試",
			"playerID", m.PlayerID,
			"idempotencyKey", m.IdempotencyKey,
		)
		return &Error{Code: CodeConcurrentModification, Err: err}

	default:
		// 內部要記全：冪等鍵是事後追一筆帳唯一有用的線索。對外一律 500、不外洩細節。
		s.logger.Error("帳務操作失敗",
			"type", m.Type,
			"subType", m.SubType,
			"playerID", m.PlayerID,
			"amount", int64(m.Amount),
			"idempotencyKey", m.IdempotencyKey,
			"err", err,
		)
		return &Error{Code: CodeInternal, Err: err}
	}
}
