package memberconsume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCreator 記下每次呼叫，並依 playerID 回傳預先排好的錯誤。
type fakeCreator struct {
	mu sync.Mutex

	walletErr  error
	diamondErr error

	walletCalls  []int64
	diamondCalls []int64
}

func (f *fakeCreator) CreateWallet(_ context.Context, playerID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.walletCalls = append(f.walletCalls, playerID)
	return f.walletErr
}

func (f *fakeCreator) CreateDiamondWallet(_ context.Context, playerID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diamondCalls = append(f.diamondCalls, playerID)
	return f.diamondErr
}

func (f *fakeCreator) snapshot() (wallet, diamond []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.walletCalls...), append([]int64(nil), f.diamondCalls...)
}

// fakeDLT 記下每次送進 DLT 的訊息，可設定回傳錯誤來模擬「DLT 也送不出去」。
type fakeDLT struct {
	mu sync.Mutex

	publishErr error
	published  [][]byte
	closed     bool
}

func (f *fakeDLT) publish(_ context.Context, _, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, value)
	return f.publishErr
}

func (f *fakeDLT) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeDLT) snapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.published...)
}

func newTestHandler(creator *fakeCreator, dlt *fakeDLT) *Handler {
	return &Handler{repo: creator, dlt: dlt, logger: discardLogger()}
}

func validPayload(playerID int64) []byte {
	return []byte(fmt.Sprintf(`{"playerId":%d,"username":"alex","email":"alex@example.com"}`, playerID))
}

// ⭐ TestHandleHappyPath 確認合法事件依序建立星幣與鑽石錢包，且不送 DLT。
func TestHandleHappyPath(t *testing.T) {
	creator := &fakeCreator{}
	dlt := &fakeDLT{}
	h := newTestHandler(creator, dlt)

	msg := kafka.Message{Value: validPayload(42)}
	if err := h.Handle(t.Context(), msg); err != nil {
		t.Fatalf("Handle() = %v, want nil", err)
	}

	wallet, diamond := creator.snapshot()
	if len(wallet) != 1 || wallet[0] != 42 {
		t.Errorf("CreateWallet 呼叫 = %v, want [42]", wallet)
	}
	if len(diamond) != 1 || diamond[0] != 42 {
		t.Errorf("CreateDiamondWallet 呼叫 = %v, want [42]", diamond)
	}
	if len(dlt.snapshot()) != 0 {
		t.Error("成功路徑不該送 DLT")
	}
}

// TestHandleMalformedJSON 確認壞 JSON 不呼叫任何 create，直接送 DLT。
//
// 對齊 Java：JsonProcessingException 是不可重試例外，DefaultErrorHandler 直送 DLT。
func TestHandleMalformedJSON(t *testing.T) {
	creator := &fakeCreator{}
	dlt := &fakeDLT{}
	h := newTestHandler(creator, dlt)

	msg := kafka.Message{Value: []byte(`{not json`)}
	if err := h.Handle(t.Context(), msg); err != nil {
		t.Fatalf("Handle() = %v, want nil（DLT 送成功時回傳 nil）", err)
	}

	wallet, diamond := creator.snapshot()
	if len(wallet) != 0 || len(diamond) != 0 {
		t.Errorf("壞 JSON 不該呼叫任何 create，got wallet=%v diamond=%v", wallet, diamond)
	}
	if len(dlt.snapshot()) != 1 {
		t.Fatalf("送進 DLT 的訊息數 = %d, want 1", len(dlt.snapshot()))
	}
}

// TestHandleNonPositivePlayerID 確認非法 playerId 直接送 DLT，不呼叫 create。
func TestHandleNonPositivePlayerID(t *testing.T) {
	tests := []struct {
		name     string
		playerID int64
	}{
		{name: "zero", playerID: 0},
		{name: "negative", playerID: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creator := &fakeCreator{}
			dlt := &fakeDLT{}
			h := newTestHandler(creator, dlt)

			msg := kafka.Message{Value: validPayload(tt.playerID)}
			if err := h.Handle(t.Context(), msg); err != nil {
				t.Fatalf("Handle() = %v, want nil", err)
			}

			wallet, diamond := creator.snapshot()
			if len(wallet) != 0 || len(diamond) != 0 {
				t.Errorf("非法 playerId 不該呼叫任何 create，got wallet=%v diamond=%v", wallet, diamond)
			}
			if len(dlt.snapshot()) != 1 {
				t.Errorf("送進 DLT 的訊息數 = %d, want 1", len(dlt.snapshot()))
			}
		})
	}
}

// ⭐ TestHandleWalletCreateFailureShortCircuitsDiamond 對齊 Java 的短路行為：
// createWallet 拋出例外時，createDiamondWallet 完全不會被呼叫。
func TestHandleWalletCreateFailureShortCircuitsDiamond(t *testing.T) {
	creator := &fakeCreator{walletErr: errors.New("連線失敗")}
	dlt := &fakeDLT{}
	h := newTestHandler(creator, dlt)

	msg := kafka.Message{Value: validPayload(7)}
	if err := h.Handle(t.Context(), msg); err != nil {
		t.Fatalf("Handle() = %v, want nil（最終送進 DLT 後回傳 nil）", err)
	}

	_, diamond := creator.snapshot()
	if len(diamond) != 0 {
		t.Errorf("CreateWallet 持續失敗時不該呼叫 CreateDiamondWallet，got %v", diamond)
	}
	if len(dlt.snapshot()) != 1 {
		t.Errorf("重試耗盡後應該送一次 DLT，got %d 次", len(dlt.snapshot()))
	}
}

// TestHandleRetriesBeforeDeadLetter 確認持續失敗會重試 maxCreateAttempts 次
// 才送 DLT，而不是第一次失敗就放棄。
func TestHandleRetriesBeforeDeadLetter(t *testing.T) {
	creator := &fakeCreator{walletErr: errors.New("暫時性錯誤")}
	dlt := &fakeDLT{}
	h := newTestHandler(creator, dlt)

	msg := kafka.Message{Value: validPayload(99)}
	if err := h.Handle(t.Context(), msg); err != nil {
		t.Fatalf("Handle() = %v, want nil", err)
	}

	wallet, _ := creator.snapshot()
	if len(wallet) != maxCreateAttempts {
		t.Errorf("CreateWallet 被呼叫 %d 次, want %d（maxCreateAttempts）", len(wallet), maxCreateAttempts)
	}
}

// TestHandleReturnsErrorWhenDLTAlsoFails 確認 DLT 本身送不出去時，Handle
// 回傳非 nil error——這一則的失敗痕跡只剩 log（platform consumer 仍會 commit
// offset，這是既有不變量，不是這個 handler 能改變的）。
func TestHandleReturnsErrorWhenDLTAlsoFails(t *testing.T) {
	creator := &fakeCreator{walletErr: errors.New("持續失敗")}
	dlt := &fakeDLT{publishErr: errors.New("DLT 也連不上")}
	h := newTestHandler(creator, dlt)

	msg := kafka.Message{Value: validPayload(1)}
	if err := h.Handle(t.Context(), msg); err == nil {
		t.Fatal("Handle() = nil, want error（原因與 DLT 送不出去都要看得見）")
	}
}
