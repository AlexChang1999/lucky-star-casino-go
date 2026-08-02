//go:build infra

// 需要真的跑起來的 Kafka：
//
//	docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait
//	go test -race -tags=infra ./internal/wallet/outbox/
//
// ⚠️ 每個測試自己建一個**只有 1 個 partition** 的臨時 topic，用完就刪。
// 1 個 partition 不是偷懶，是為了讓「讀回來」這件事是確定的：kafka-go 的
// Reader 不帶 GroupID 時只讀單一 partition，而正式的 topic 有 6 個。
// （多 partition 的消費要靠 consumer group，那是切片 8 的事，而且會踩到地雷 #19。）
package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// newScratchTopic 建一個用完就刪的單 partition topic。
func newScratchTopic(t *testing.T) (brokers []string, topic string) {
	t.Helper()

	cfg, err := config.LoadKafka()
	if err != nil {
		t.Fatalf("載入 Kafka 設定失敗（是不是忘了 set -a; . deploy/.env; set +a）: %v", err)
	}
	topic = fmt.Sprintf("wallet.scratch.%d", time.Now().UnixNano())

	client := &kafka.Client{Addr: kafka.TCP(cfg.Brokers...), Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}},
	})
	if err != nil {
		t.Fatalf("建立測試 topic 失敗（Kafka 起來了嗎？）: %v", err)
	}
	if err := res.Errors[topic]; err != nil {
		t.Fatalf("建立測試 topic %s 失敗: %v", topic, err)
	}

	t.Cleanup(func() {
		// ⚠️ 新的 context：測試結束時原本那個早就取消了，沿用會讓刪除送不出去，
		// 臨時 topic 就永遠留在 broker 的 volume 裡（與 mysqltest 的 DROP 同一個坑）。
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := client.DeleteTopics(cleanupCtx, &kafka.DeleteTopicsRequest{Topics: []string{topic}}); err != nil {
			t.Errorf("刪除測試 topic %s 失敗（要手動清）: %v", topic, err)
		}
	})
	return cfg.Brokers, topic
}

// ⭐ TestPublisherRoundTrip 是「事件真的進得了 Kafka」的唯一證據。
//
// 三件事一起驗，因為它們是同一條契約的三個面向：
//   - payload **逐位元組**等於 outbox 那一列的字串（契約測試會 diff 它）
//   - key 是 playerID（同玩家事件同 partition，順序才有意義）
//   - Publish 回報的 id 是**已 ack** 的，不是「大概送出去了」
func TestPublisherRoundTrip(t *testing.T) {
	brokers, topic := newScratchTopic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	publisher := NewPublisher(NewWriter(brokers, 500, discardLogger()), discardLogger())
	t.Cleanup(func() {
		if err := publisher.Close(); err != nil {
			t.Errorf("關閉 writer 失敗: %v", err)
		}
	})

	// payload 刻意用 domain 產生的那個形狀（欄位順序與 Java 的 record 一致）。
	events := []store.PendingEvent{
		{ID: 1, Topic: topic, KafkaKey: ptr("42"), Payload: `{"transactionId":1,"playerId":42,"amount":300,"referenceId":null}`},
		{ID: 2, Topic: topic, KafkaKey: ptr("42"), Payload: `{"transactionId":2,"playerId":42,"amount":100,"referenceId":"round-9527"}`},
	}
	sent, err := publisher.Publish(ctx, events)
	if err != nil {
		t.Fatalf("投遞失敗: %v", err)
	}
	if len(sent) != 2 || sent[0] != 1 || sent[1] != 2 {
		t.Fatalf("回報已送達的 id = %v, want [1 2]", sent)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: 0,
		MaxWait:   500 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("關閉 reader 失敗: %v", err)
		}
	})
	if err := reader.SetOffset(kafka.FirstOffset); err != nil {
		t.Fatalf("設定 offset 失敗: %v", err)
	}

	for i, want := range events {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("讀第 %d 則訊息失敗: %v", i, err)
		}
		if string(msg.Value) != want.Payload {
			t.Errorf("第 %d 則 payload 不一致（契約測試會逐位元組 diff 它）\ngot:  %s\nwant: %s",
				i, msg.Value, want.Payload)
		}
		if string(msg.Key) != *want.KafkaKey {
			t.Errorf("第 %d 則 key = %q, want %q——key 錯了同玩家事件就不再同 partition", i, msg.Key, *want.KafkaKey)
		}
	}
}

// ⭐⭐ TestPublisherDoesNotWaitForBatchTimeout 是地雷 #21 的實測回歸測試。
//
// kafka-go 的 `BatchTimeout` 預設是 **1 秒**：批次沒裝滿時 producer 會壓滿
// 一秒才送出。outbox 大部分時間一輪只有幾筆事件，所以那一秒會直接變成
// 「下注到排行榜更新」的延遲。
//
// ⚠️ 這一秒**日誌與指標都看不出來**——延遲指標量的是訊息時間戳之後的事，
// 日誌只會看到「送出成功」。只有像這樣拿碼表量端到端才會發現。
// 把 writerFlushDelay 改回 kafka-go 的預設，這個測試就會紅。
func TestPublisherDoesNotWaitForBatchTimeout(t *testing.T) {
	brokers, topic := newScratchTopic(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	publisher := NewPublisher(NewWriter(brokers, 500, discardLogger()), discardLogger())
	t.Cleanup(func() { _ = publisher.Close() })

	one := []store.PendingEvent{{ID: 1, Topic: topic, KafkaKey: ptr("42"), Payload: `{"n":1}`}}

	// 第一次順便暖機：建連線、拿 metadata，那些成本不屬於 BatchTimeout。
	if _, err := publisher.Publish(ctx, one); err != nil {
		t.Fatalf("暖機投遞失敗: %v", err)
	}

	start := time.Now()
	if _, err := publisher.Publish(ctx, []store.PendingEvent{{ID: 2, Topic: topic, KafkaKey: ptr("42"), Payload: `{"n":2}`}}); err != nil {
		t.Fatalf("投遞失敗: %v", err)
	}
	elapsed := time.Since(start)

	// 門檻取 700ms：預設值是 1s，實際值應該是 10ms 級，中間有很大的緩衝，
	// 慢一點的機器不會誤報，而改回預設值一定會被抓到。
	if elapsed > 700*time.Millisecond {
		t.Errorf("送一則訊息花了 %s——BatchTimeout 是不是回到 kafka-go 預設的 1 秒了？（地雷 #21）", elapsed)
	}
	t.Logf("單則訊息端到端耗時 %s（預設 BatchTimeout 會是 1s 起跳）", elapsed)
}
