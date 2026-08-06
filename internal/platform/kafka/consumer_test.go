package kafka

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// fetchResult 是假 reader 的一次 FetchMessage 結果：訊息或錯誤，二選一。
type fetchResult struct {
	msg kafka.Message
	err error
}

// commitCall 記下每一次 commit 的訊息**與當下 context 的狀態**。
//
// ⭐ `ctxAlreadyDone` 是這整個假物件存在的主要理由：地雷 #20 的後半段是
// 「收工時 commit 要用 context.WithoutCancel」，而那件事只有從
// **commit 收到的 ctx** 才看得出來——從外面看，兩種寫法在正常情況下
// 行為完全相同，只有關機的那一瞬間不同。
type commitCall struct {
	msg            kafka.Message
	ctxAlreadyDone bool
}

// fakeReader 依序吐出預先排好的 fetch 結果；用完之後就掛在 ctx 上等收工。
type fakeReader struct {
	mu      sync.Mutex
	queue   []fetchResult
	fetched int
	commits []commitCall

	commitErr error
	closed    bool
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	f.mu.Lock()
	f.fetched++
	if len(f.queue) > 0 {
		next := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return next.msg, next.err
	}
	f.mu.Unlock()

	// 佇列空了就模擬「broker 沒有新訊息」：掛著等，直到 ctx 被取消。
	// 真的 kafka.Reader 也是這個形狀（MaxWait 到期會重試，對外看起來就是阻塞）。
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (f *fakeReader) CommitMessages(ctx context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.commits = append(f.commits, commitCall{msg: m, ctxAlreadyDone: ctx.Err() != nil})
	}
	return f.commitErr
}

func (f *fakeReader) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeReader) snapshot() (commits []commitCall, fetched int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]commitCall(nil), f.commits...), f.fetched
}

func msgAt(offset int64) kafka.Message {
	return kafka.Message{Topic: "wallet.debit", Partition: 0, Offset: offset, Key: []byte("42")}
}

// ⭐ TestConsumerCommitsRegardlessOfHandlerOutcome 釘住地雷 #20 的前半段。
//
// 對齊 Java 的 `finally { ack.acknowledge(); }`。**不 commit 的話 offset 會卡在
// 那一則壞訊息上**，kafka-go 下次 fetch 拿到的還是它——一則壞訊息就能讓整個
// topic 停擺，而從外面看只是「某個功能突然沒了」：group 還在、服務健康檢查全過。
func TestConsumerCommitsRegardlessOfHandlerOutcome(t *testing.T) {
	tests := []struct {
		name      string
		handleErr error
	}{
		{name: "處理成功", handleErr: nil},
		{name: "處理失敗仍要 commit", handleErr: errors.New("投影寫入失敗")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeReader{queue: []fetchResult{{msg: msgAt(7)}}}

			ctx, cancel := context.WithCancel(t.Context())
			consumer := NewConsumer(reader, "test", func(context.Context, kafka.Message) error {
				// 這一則處理完就收工，讓 Run 在下一輪 fetch 時返回。
				defer cancel()
				return tt.handleErr
			}, discardLogger())

			if err := consumer.Run(ctx); err != nil {
				t.Fatalf("Run() = %v, want nil（ctx 取消是正常收工，不是錯誤）", err)
			}

			commits, _ := reader.snapshot()
			if len(commits) != 1 {
				t.Fatalf("commit 了 %d 則，want 1——handler 回 error 時漏 commit 會讓 offset 卡住整個 partition", len(commits))
			}
			if commits[0].msg.Offset != 7 {
				t.Errorf("commit 的 offset = %d, want 7", commits[0].msg.Offset)
			}
		})
	}
}

// ⭐ TestConsumerCommitsWithUncancelledContext 釘住地雷 #20 的後半段。
//
// 收工時 root ctx **已經被取消了**（那正是我們要收工的原因）。直接把它傳給
// CommitMessages 的話，最後一則的 commit 一出生就失敗 → 下次開機重播。
// ⚠️ 這個 bug 在正常情況下完全看不出來：只有關機的那一瞬間會發生，
// 而症狀（重複消費一則）會被冪等的消費端吃掉，於是它可以存在很久。
func TestConsumerCommitsWithUncancelledContext(t *testing.T) {
	reader := &fakeReader{queue: []fetchResult{{msg: msgAt(11)}}}

	ctx, cancel := context.WithCancel(t.Context())
	consumer := NewConsumer(reader, "test", func(context.Context, kafka.Message) error {
		// 模擬「處理到一半收到 SIGTERM」：handler 還沒返回，root ctx 就死了。
		cancel()
		return nil
	}, discardLogger())

	if err := consumer.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	commits, _ := reader.snapshot()
	if len(commits) != 1 {
		t.Fatalf("commit 了 %d 則，want 1", len(commits))
	}
	if commits[0].ctxAlreadyDone {
		t.Error("commit 收到的 context 已經被取消——收工時最後一則會 commit 失敗並在下次開機重播；" +
			"這裡需要 context.WithoutCancel（地雷 #20）")
	}
}

// TestConsumerContinuesAfterCommitFailure 確認 commit 失敗不會讓迴圈停下來。
//
// commit 失敗的代價只是「這一則會被重播」，而消費端本來就必須冪等
// （at-least-once，地雷 #6）。因此而中止消費，是拿一個小問題換一個大問題。
func TestConsumerContinuesAfterCommitFailure(t *testing.T) {
	reader := &fakeReader{
		queue:     []fetchResult{{msg: msgAt(1)}, {msg: msgAt(2)}},
		commitErr: errors.New("coordinator 不通"),
	}

	var handled int
	ctx, cancel := context.WithCancel(t.Context())
	consumer := NewConsumer(reader, "test", func(context.Context, kafka.Message) error {
		handled++
		if handled == 2 {
			cancel()
		}
		return nil
	}, discardLogger())

	if err := consumer.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if handled != 2 {
		t.Errorf("處理了 %d 則，want 2——commit 失敗不該中止消費", handled)
	}
}

// TestConsumerBacksOffOnFetchError 確認 fetch 失敗會退避，而不是 tight loop。
//
// ⚠️ 沒有退避的話，Kafka 不通時這個迴圈會把一顆 CPU 燒滿並且每秒印上千行日誌。
// 這裡只驗「有等」與「等完會繼續」，加倍與上限交給 TestNextFetchBackoff——
// 靠真的等待去驗指數退避又慢又 flaky。
func TestConsumerBacksOffOnFetchError(t *testing.T) {
	reader := &fakeReader{queue: []fetchResult{
		{err: errors.New("dial tcp: connection refused")},
		{msg: msgAt(3)},
	}}

	ctx, cancel := context.WithCancel(t.Context())
	consumer := NewConsumer(reader, "test", func(context.Context, kafka.Message) error {
		cancel()
		return nil
	}, discardLogger())

	start := time.Now()
	if err := consumer.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	elapsed := time.Since(start)

	commits, fetched := reader.snapshot()
	if len(commits) != 1 {
		t.Fatalf("commit 了 %d 則，want 1——退避之後應該要繼續消費，不是放棄", len(commits))
	}
	if fetched < 2 {
		t.Errorf("只 fetch 了 %d 次，want ≥ 2——fetch 失敗後沒有重試", fetched)
	}
	if elapsed < fetchBackoffMin {
		t.Errorf("整個迴圈只花了 %s，短於一次退避 %s——fetch 失敗後沒有等待就重試（tight loop）",
			elapsed, fetchBackoffMin)
	}
}

// TestConsumerStopsOnCancelledContext 確認收工是「正常結束」而不是錯誤。
//
// ⚠️ 判斷收工要看 ctx 而不是 error 的型別：kafka-go 在 ctx 取消時回的可能是
// context.Canceled，也可能是連線被關掉產生的 io 錯誤。用 errors.Is 判斷會讓
// 後者被當成故障，於是每次關機都印一行看起來很嚇人的 ERROR。
func TestConsumerStopsOnCancelledContext(t *testing.T) {
	tests := []struct {
		name     string
		fetchErr error
	}{
		{name: "fetch 回 context.Canceled", fetchErr: context.Canceled},
		{name: "fetch 回連線被關掉的 io 錯誤", fetchErr: errors.New("use of closed network connection")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // 先取消，模擬「fetch 掛著的時候收到 SIGTERM」

			reader := &fakeReader{queue: []fetchResult{{err: tt.fetchErr}}}
			consumer := NewConsumer(reader, "test", func(context.Context, kafka.Message) error {
				t.Error("ctx 已取消，不該再處理任何訊息")
				return nil
			}, discardLogger())

			done := make(chan error, 1)
			go func() { done <- consumer.Run(ctx) }()

			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run() = %v, want nil——收工不是錯誤", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run() 在 ctx 取消後沒有返回")
			}
		})
	}
}

func TestNextFetchBackoff(t *testing.T) {
	tests := []struct {
		name    string
		current time.Duration
		want    time.Duration
	}{
		{name: "從最小值加倍", current: fetchBackoffMin, want: 2 * fetchBackoffMin},
		{name: "接近上限時夾住", current: 4 * time.Second, want: fetchBackoffMax},
		{name: "已在上限就不再長", current: fetchBackoffMax, want: fetchBackoffMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextFetchBackoff(tt.current); got != tt.want {
				t.Errorf("nextFetchBackoff(%s) = %s, want %s", tt.current, got, tt.want)
			}
		})
	}
}

func TestConsumerCloseClosesReader(t *testing.T) {
	reader := &fakeReader{}
	consumer := NewConsumer(reader, "test", nil, discardLogger())

	if err := consumer.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if !reader.closed {
		t.Error("Close() 沒有關掉底層 reader——LeaveGroup 不會送出，" +
			"coordinator 要等 SessionTimeout 才踢掉這個 member（地雷 #25）")
	}
}
