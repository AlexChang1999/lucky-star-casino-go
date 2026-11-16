package domain

// TopicMemberRegistered 是 wallet **消費**（不是產出）的事件。
//
// 獨立於 event.go 而不是加進同一個檔：那邊的 MovementEvent 是 wallet 發出的，
// 這裡是 wallet 收的，方向相反。混在一起容易在改動時看錯方向——正是地雷 #2
// 講的「wallet.credit 是事件、wallet.credit.request 才是指令」搞反會造成
// 無限迴圈的同一類錯誤，值得用檔案切開讓它一眼看得出來。
const TopicMemberRegistered = "member.registered"

// MemberRegisteredEvent 對齊 Java 的 record MemberRegisteredEvent
// （member-service 產出，wallet-service 消費）：
//
//	record MemberRegisteredEvent(Long playerId, String username, String email) {}
//
// wallet 只用 PlayerID 建立錢包，Username/Email 目前用不到，但型別上留著——
// 隱藏掉會讓人以為線上契約只有一個欄位，之後有人要用 Username 時得回去翻
// Java 原始碼才知道它其實一直都在。
type MemberRegisteredEvent struct {
	PlayerID int64  `json:"playerId"`
	Username string `json:"username"`
	Email    string `json:"email"`
}
