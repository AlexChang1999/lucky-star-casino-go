package service

import "errors"

// Code 是帳務操作的結果分類。handler 把它翻成 HTTP 狀態碼，service 不碰狀態碼。
type Code int

const (
	// CodeInternal 是 0 值，這是刻意的：忘了設 Code 的 Error 會被當成 500，
	// 而不是被當成某個看起來合理的 4xx。
	CodeInternal Code = iota
	CodeWalletNotFound
	CodeInsufficientBalance
	CodeConcurrentModification
	// 破產救助的兩種「不符資格」對外都是 422，但訊息不同（Java 兩句不同的文字），
	// 所以拆成兩個 Code，不合併成一個 CodeNotEligible。
	CodeAidBalanceTooHigh
	CodeAidAlreadyClaimed
	// 贈禮。sender / receiver 的「錢包不存在」拆成兩個 Code，因為對外訊息要指明是哪一方；
	// 兩種超額也各有一句訊息（理由同上面的破產救助）。
	CodeInvalidGift
	CodeGiftSentLimitExceeded
	CodeGiftRecvLimitExceeded
	CodeGiftSenderNotFound
	CodeGiftReceiverNotFound
	// CodeGiftRefunded：入帳重試用盡，錢已退回送出方。對送禮的人來說這是一次失敗的贈禮。
	CodeGiftRefunded
	// CodeGiftKeyConflict：這把冪等鍵屬於另一位送出方的贈禮。
	CodeGiftKeyConflict
)

// Error 是 service 層對外的唯一錯誤型別。
//
// Err 保留原始錯誤給 log 與 errors.Is 用；**不可以**原樣回給呼叫端
// （可能含連線字串、內部主機名，見 handler 的 TestInternalErrorDoesNotLeakDetails）。
type Error struct {
	Code Code
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// CodeOf 取出任意 error 的 Code；不是 *Error 一律視為 CodeInternal。
func CodeOf(err error) Code {
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return CodeInternal
}
