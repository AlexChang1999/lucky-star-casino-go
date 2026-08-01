package outbox

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ptr[T any](v T) *T { return &v }

// ⭐ TestNewWriterPinsSilentDefaults 釘住 kafka-go 那些「不設就出事、
// 而且出事時完全沒有訊息」的預設值。
//
// 這個測試看起來像在測設定檔，但它擋的每一條都是實際的資料正確性問題：
// 一年後有人為了「簡化」把某個欄位拿掉時，這裡會紅，而正式環境不會。
func TestNewWriterPinsSilentDefaults(t *testing.T) {
	w := NewWriter([]string{"localhost:9095"}, 500, discardLogger())

	tests := []struct {
		name string
		got  any
		want any
		why  string
	}{
		{
			name: "BatchTimeout 不可以是預設的 1 秒",
			got:  w.BatchTimeout, want: writerFlushDelay,
			why: "kafka-go 的預設是 1s（地雷 #21）：批次沒裝滿時 producer 會整整壓一秒才送，而日誌與指標都看不出來",
		},
		{
			name: "BatchSize 要等於 poller 的批次量",
			got:  w.BatchSize, want: 500,
			why: "設成 1 會讓每則訊息各自成一個 batch，退化成 Java 舊版 O(N) 循序等 ack 的瓶頸",
		},
		{
			name: "Async 必須是 false",
			got:  w.Async, want: false,
			why: "true 會讓 WriteMessages 立刻回 nil，於是還沒送出的事件被標成 SENT 再被清理排程刪掉——事件無聲蒸發",
		},
		{
			name: "RequiredAcks 要是 all",
			got:  w.RequiredAcks, want: kafka.RequireAll,
			why: "leader 收到就算送達的話，leader 在複寫前掛掉就是丟事件",
		},
		{
			name: "WriteTimeout 要短於 docker stop 的寬限期",
			got:  w.WriteTimeout < 10*time.Second, want: true,
			why: "一輪要能在 SIGKILL 之前跑完，否則每次關機都留下重複投遞",
		},
		{
			name: "MaxAttempts 對齊 Java 的 retries: 3",
			got:  w.MaxAttempts, want: writerMaxAttempts,
			why: "kafka-go 預設 10 次，重試期間那一輪的 DB 交易一直開著",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v\n原因: %s", tt.got, tt.want, tt.why)
			}
		})
	}

	// ⭐ Balancer 單獨驗，因為它比不了值：預設是 &kafka.Hash{}（FNV-1a），
	// Java 的 DefaultPartitioner 是 murmur2。用錯的話，同一個 playerId 在
	// Java 版與 Go 版會落到不同 partition——並存期間同玩家事件的順序保證直接失效，
	// 而三方（Kafka、producer、consumer）都不會報錯。
	if _, ok := w.Balancer.(*kafka.Murmur2Balancer); !ok {
		t.Errorf("Balancer = %T, want *kafka.Murmur2Balancer（要與 Java 的 partitioner 相容）", w.Balancer)
	}
	// Topic 留空是刻意的：每一列自己帶 topic（wallet.debit / wallet.credit）。
	if w.Topic != "" {
		t.Errorf("Writer.Topic = %q, want 空字串——寫死會讓其中一種事件靜靜送錯 topic", w.Topic)
	}
}

func TestMessagesFor(t *testing.T) {
	tests := []struct {
		name    string
		event   store.PendingEvent
		wantKey []byte
		why     string
	}{
		{
			name:    "一般事件：key 是 playerID 的字串",
			event:   store.PendingEvent{ID: 1, Topic: "wallet.debit", KafkaKey: ptr("42"), Payload: `{"a":1}`},
			wantKey: []byte("42"),
			why:     "同玩家的事件要落在同一個 partition，下游看到的順序才與帳務發生順序一致",
		},
		{
			name:    "kafka_key 為 NULL 時 key 必須是 nil",
			event:   store.PendingEvent{ID: 2, Topic: "wallet.credit", KafkaKey: nil, Payload: `{}`},
			wantKey: nil,
			why:     "nil key 走隨機 partition；空 []byte 是一個確定的雜湊值，會讓所有無 key 訊息擠在同一個 partition",
		},
		{
			name:    "kafka_key 是空字串時不可以變成 nil",
			event:   store.PendingEvent{ID: 3, Topic: "wallet.credit", KafkaKey: ptr(""), Payload: `{}`},
			wantKey: []byte(""),
			why:     "NULL 與空字串在 SQL 與 Kafka 都是兩個值（地雷 #33），這裡混掉會讓 partition 分配跟著變",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := messagesFor([]store.PendingEvent{tt.event})
			if len(got) != 1 {
				t.Fatalf("訊息數 = %d, want 1", len(got))
			}
			msg := got[0]
			if msg.Topic != tt.event.Topic {
				t.Errorf("Topic = %q, want %q", msg.Topic, tt.event.Topic)
			}
			if string(msg.Value) != tt.event.Payload {
				t.Errorf("Value = %q, want %q（payload 必須原封不動，不可重新序列化）", msg.Value, tt.event.Payload)
			}
			if (msg.Key == nil) != (tt.wantKey == nil) || string(msg.Key) != string(tt.wantKey) {
				t.Errorf("Key = %v(nil=%t), want %v(nil=%t)\n原因: %s",
					msg.Key, msg.Key == nil, tt.wantKey, tt.wantKey == nil, tt.why)
			}
		})
	}
}

// TestMessagesForPreservesOrder 釘住「送出順序＝撈出順序」。
//
// partition 內的順序就是送出呼叫的順序，而撈取是 ORDER BY created_at, id。
// 這一層若重排（例如為了效率先依 topic 分組），同一個玩家的
// 「先扣款、後派彩」在下游就會變成隨機順序，而 Kafka 不會為此報錯。
func TestMessagesForPreservesOrder(t *testing.T) {
	events := []store.PendingEvent{
		{ID: 10, Topic: "wallet.debit", KafkaKey: ptr("1"), Payload: `{"n":1}`},
		{ID: 11, Topic: "wallet.credit", KafkaKey: ptr("1"), Payload: `{"n":2}`},
		{ID: 12, Topic: "wallet.debit", KafkaKey: ptr("1"), Payload: `{"n":3}`},
	}
	got := messagesFor(events)
	for i, want := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		if string(got[i].Value) != want {
			t.Errorf("第 %d 則 = %s, want %s——順序被改掉了", i, got[i].Value, want)
		}
	}
}
