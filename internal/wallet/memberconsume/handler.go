// Package memberconsume 消費 member.registered、建立星幣與鑽石錢包。
//
// 這是 wallet 服務**唯一**建立錢包的路徑（兩版皆然，沒有任何 HTTP 端點會
// lazy-create）：對齊 Java 的 MemberEventListener → WalletService.createWallet
// + DiamondWalletService.createDiamondWallet。
package memberconsume

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
)

const (
	// maxCreateAttempts / createRetryBackoff 是「重試幾次才送 DLT」的時距。
	//
	// ⚠️ 這兩個數字跟 Java 的 `FixedBackOff(2000ms, 3)` **刻意不同**，不是照抄
	// 漏了：Java 的重試發生在 Kafka redelivery 層（container 重新投遞整則
	// 訊息，不受單則處理時間限制）；Go 版的重試在這個 handler 內部跑，而
	// internal/platform/kafka 的 handleTimeout 是 10 秒的**單則處理總預算**。
	// 3×2s 的退避已經吃掉 6 秒，加上 DB 呼叫本身會頂著上限。這裡不是業務口徑
	// （不受「不要猜業務規則」限制），純粹是等待多久合理的工程時距常數，
	// 改成 3 次、每次 500ms（總退避 1 秒），留更多預算給實際的 DB 呼叫。
	maxCreateAttempts  = 3
	createRetryBackoff = 500 * time.Millisecond
)

// creator 是這個 handler 需要的 repo 子集合。
//
// ⭐ 介面定義在消費端（CLAUDE.md §2），跟 internal/platform/kafka 的
// messageReader 是同一個模式：store 套件不知道這個介面存在，只是剛好有這
// 兩個方法。好處是測試的假物件只要實作兩個方法，不必引入整個 *store.Repository。
type creator interface {
	CreateWallet(ctx context.Context, playerID int64) error
	CreateDiamondWallet(ctx context.Context, playerID int64) error
}

// dltPublisher 是送 DLT 需要的能力，讓測試可以換成假物件。
type dltPublisher interface {
	publish(ctx context.Context, key, value []byte) error
	close() error
}

// Handler 處理 member.registered、建立錢包，實作 kafka.Handler 的簽章。
type Handler struct {
	repo   creator
	dlt    dltPublisher
	logger *slog.Logger
}

// NewHandler 組裝 Handler。brokers 用來建立內部的 DLT writer——放在這裡而不是
// 由呼叫端傳一個組好的 writer 進來，是因為 DLT writer 是這個 handler 私有的
// 實作細節，呼叫端（cmd/wallet/main.go）不需要知道它的存在，只需要知道
// 「這個 handler 需要 Kafka brokers 位址」與「用完要 Close」（見 Close）。
func NewHandler(repo creator, brokers []string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{repo: repo, dlt: newDLT(brokers, logger), logger: logger}
}

// Handle 實作 kafka.Handler：解析事件、依序建立星幣與鑽石錢包，失敗時重試，
// 重試耗盡後送 DLT。
//
// ⚠️ 不論走哪條路徑都回傳 nil 或一個「已經處理過」的結果——offset 一律會被
// internal/platform/kafka.Consumer 提交（地雷 #20），這裡的回傳值只影響
// 一行 log 的層級，不影響 commit 是否發生。
func (h *Handler) Handle(ctx context.Context, msg kafka.Message) error {
	var evt domain.MemberRegisteredEvent
	if err := json.Unmarshal(msg.Value, &evt); err != nil {
		return h.deadLetter(ctx, msg, fmt.Errorf("member.registered payload 解析失敗: %w", err))
	}
	if evt.PlayerID <= 0 {
		return h.deadLetter(ctx, msg, fmt.Errorf("member.registered 帶了非法 playerId=%d", evt.PlayerID))
	}

	var err error
	for attempt := 1; attempt <= maxCreateAttempts; attempt++ {
		if err = h.create(ctx, evt.PlayerID); err == nil {
			return nil
		}
		if attempt < maxCreateAttempts {
			h.logger.Warn("建立錢包失敗，退避後重試",
				"playerID", evt.PlayerID, "attempt", attempt, "err", err)
			if !sleepCtx(ctx, createRetryBackoff) {
				break // 收工訊號，不再重試
			}
		}
	}
	return h.deadLetter(ctx, msg,
		fmt.Errorf("建立錢包重試 %d 次仍失敗（playerID=%d）: %w", maxCreateAttempts, evt.PlayerID, err))
}

// create 依序建立星幣與鑽石錢包，對齊 Java 的呼叫順序。
//
// 星幣先、鑽石後，且 CreateWallet 本身冪等：如果整個 create 因為鑽石那步
// 失敗而重跑，星幣那步會落到 dup-entry no-op，不會重複建立——這保留了
// Java 版「兩步都靠 redelivery 安全重跑」的特性，不需要額外記錄做到哪一步。
func (h *Handler) create(ctx context.Context, playerID int64) error {
	if err := h.repo.CreateWallet(ctx, playerID); err != nil {
		return fmt.Errorf("建立星幣錢包失敗（playerID=%d）: %w", playerID, err)
	}
	if err := h.repo.CreateDiamondWallet(ctx, playerID); err != nil {
		return fmt.Errorf("建立鑽石錢包失敗（playerID=%d）: %w", playerID, err)
	}
	return nil
}

// deadLetter 把原始訊息送進 member.registered.DLT。
//
// ⚠️ **刻意跟 Java 一樣「發了沒人收」**（AGENTS.md 地雷 #40）：Java 的
// DeadLetterListener 沒有對應 member.registered.DLT 的 listener，Go 版不建
// 一個沒人查的 reader，那是這個切片之外的過度設計。這裡只確保訊息至少被
// 送到那個 topic，而不是無聲丟棄。
func (h *Handler) deadLetter(ctx context.Context, msg kafka.Message, cause error) error {
	h.logger.Error("member.registered 處理失敗，送進 DLT（地雷 #40：目前沒有任何東西會再讀它）",
		"topic", msg.Topic, "partition", msg.Partition, "offset", msg.Offset, "err", cause)

	if err := h.dlt.publish(ctx, msg.Key, msg.Value); err != nil {
		return fmt.Errorf("處理失敗（%w），且送 DLT 也失敗: %w", cause, err)
	}
	return nil
}

// Close 關閉內部的 DLT writer。必須在 kafka.Consumer.Run 返回之後才呼叫，
// 理由與 outbox.Publisher.Close 相同：writer 先關的話，最後一筆還在送的
// DLT 訊息會被誤判成失敗。
func (h *Handler) Close() error {
	return h.dlt.close()
}

// sleepCtx 等待 d，中途 ctx 被取消就提早返回 false。
//
// 跟 internal/platform/kafka 的同名私有函式是同一個判準（不要用 time.Sleep，
// 否則關機訊號來的時候會把整個收工流程壓在那裡等滿）——兩邊都小到沒有共用
// 的必要，共用一個跨套件的工具函式換不到什麼，倒是多一個要追的 import。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
