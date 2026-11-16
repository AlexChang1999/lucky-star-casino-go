// Package enum 放 wallet 跨套件共用的固定值。這一層不 import 任何業務套件。
package enum

// GiftStatus 是贈禮狀態單的狀態，對應 gift_transfers.status 的 CHECK 約束。
//
// 放在 enum 而不是 repository：repository（寫入條件轉移）與 service（決定下一步）
// 都要用它，定義在任何一邊都會讓另一邊反過來 import。
type GiftStatus string

const (
	GiftPending   GiftStatus = "PENDING"   // 已立單，尚未確認扣款
	GiftDebited   GiftStatus = "DEBITED"   // 送出方已扣款，等待入帳
	GiftCompleted GiftStatus = "COMPLETED" // 接收方已入帳（終態）
	GiftRefunding GiftStatus = "REFUNDING" // 入帳放棄，正在退款給送出方
	GiftRefunded  GiftStatus = "REFUNDED"  // 已退款（終態）
)
