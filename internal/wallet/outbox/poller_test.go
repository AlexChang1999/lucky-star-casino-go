package outbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// fakeStore 讓 poller 的測試完全不必碰 MySQL 或 Kafka。
//
// ⭐ 它之所以只有一個方法，是因為介面定義在**消費端**（見 poller.go 的
// eventStore）。若介面是由 store 套件定義的「Repository 的全部方法」，
// 這個假物件就得跟著長出 Debit、Credit……而它們與投遞邏輯毫無關係。
type fakeStore struct {
	publishPending func(ctx context.Context, batchSize int, publish store.PublishFunc) (store.PublishStats, error)
}

func (f fakeStore) PublishPending(ctx context.Context, batchSize int, publish store.PublishFunc) (store.PublishStats, error) {
	return f.publishPending(ctx, batchSize, publish)
}

func testOutboxConfig() config.Outbox {
	return config.Outbox{PollInterval: time.Millisecond, BatchSize: 7, Retention: 24 * time.Hour}
}

// noopPublish 是一個什麼都不送的 PublishFunc。
func noopPublish(_ context.Context, _ []store.PendingEvent) ([]int64, error) { return nil, nil }

func TestPollerRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := fakeStore{publishPending: func(context.Context, int, store.PublishFunc) (store.PublishStats, error) {
		return store.PublishStats{}, nil
	}}

	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, testOutboxConfig(), discardLogger()).Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 沒有在 context 取消後返回——關機會卡在寬限期直到 SIGKILL")
	}
}

// ⭐ TestPollerRoundIgnoresShutdownSignal 是 context.WithoutCancel 的回歸測試。
//
// 為什麼這件事重要：一輪的後半段是「把**已經送進 Kafka** 的事件標成 SENT」。
// 收工訊號若能中斷它，那些事件會維持 PENDING、下次開機重送一次——
// at-least-once 容許重複，但這個重複是我們自己在每一次關機時製造出來的。
//
// ⚠️ 這個測試在拿掉 WithoutCancel 之後**一定會紅**：context.CancelFunc 返回前
// 就已經關掉所有子 context 的 done channel，所以 cancel() 之後再讀 roundCtx.Err()
// 必定非 nil。
func TestPollerRoundIgnoresShutdownSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	release := make(chan struct{})
	var roundErr error

	s := fakeStore{publishPending: func(roundCtx context.Context, _ int, _ store.PublishFunc) (store.PublishStats, error) {
		close(entered)
		<-release // 卡在這裡，模擬「正在等 Kafka ack」
		roundErr = roundCtx.Err()
		return store.PublishStats{}, nil
	}}

	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, testOutboxConfig(), discardLogger()).Run(ctx)
		close(done)
	}()

	<-entered
	cancel()       // 等同收到 SIGTERM
	close(release) // 放行那一輪

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 沒有返回")
	}
	if roundErr != nil {
		t.Errorf("這一輪的 context 被關機訊號取消了（%v）——標記 SENT 會失敗，"+
			"已送出的事件下次開機會重送一次", roundErr)
	}
}

// TestPollerKeepsRunningAfterFailure 釘住「投遞失敗不可以讓迴圈停掉」。
//
// ⚠️ 這是最容易寫錯的地方：Go 的慣例是「錯誤往上回傳」，但這裡往上回傳
// 就是讓 goroutine 結束——服務還活著、健康檢查照過、HTTP 照收，
// 只有事件從此再也沒送出去過。典型的「每一項檢查都顯示正常」故障。
func TestPollerKeepsRunningAfterFailure(t *testing.T) {
	var calls atomic.Int64
	s := fakeStore{publishPending: func(context.Context, int, store.PublishFunc) (store.PublishStats, error) {
		calls.Add(1)
		return store.PublishStats{Fetched: 1, Failed: 1}, errors.New("kafka 掛了")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		NewPoller(s, noopPublish, testOutboxConfig(), discardLogger()).Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("投遞連續失敗之後 poller 停了（只跑了 %d 輪）", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// TestPollerPassesBatchSizeAndPublish 釘住設定值真的有走到 store。
//
// ⚠️ 這條看起來瑣碎，但 batchSize 傳錯（例如傳成 0）的症狀是
// 「LIMIT 0 永遠撈不到事件」——outbox 只進不出，而服務完全正常。
func TestPollerPassesBatchSizeAndPublish(t *testing.T) {
	gotBatchSize := make(chan int, 1)
	publishCalled := make(chan struct{}, 1)

	publish := store.PublishFunc(func(context.Context, []store.PendingEvent) ([]int64, error) {
		select {
		case publishCalled <- struct{}{}:
		default:
		}
		return nil, nil
	})

	s := fakeStore{publishPending: func(ctx context.Context, batchSize int, p store.PublishFunc) (store.PublishStats, error) {
		select {
		case gotBatchSize <- batchSize:
		default:
		}
		// store 會在交易裡呼叫 publish；這裡照做，證明傳進來的就是我們給的那一個。
		_, _ = p(ctx, nil)
		return store.PublishStats{}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewPoller(s, publish, testOutboxConfig(), discardLogger()).Run(ctx)

	select {
	case got := <-gotBatchSize:
		if got != testOutboxConfig().BatchSize {
			t.Errorf("batchSize = %d, want %d", got, testOutboxConfig().BatchSize)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poller 沒有呼叫 PublishPending")
	}
	select {
	case <-publishCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("poller 傳給 store 的不是我們給的 PublishFunc")
	}
}
