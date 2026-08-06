package kafka

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ⭐ TestNewReaderPinsSilentDefaults 釘住 kafka-go 那些「不設就出事、
// 而且出事時完全沒有訊息」的 Reader 預設值。
//
// 與 outbox 的 TestNewWriterPinsSilentDefaults 是對稱的一對：那邊擋的是
// 「事件沒送出去卻被標成 SENT」，這邊擋的是「consumer 加入了 group 卻永遠
// 收不到訊息」。兩者的共同點是**每一項檢查都顯示正常**。
func TestNewReaderPinsSilentDefaults(t *testing.T) {
	r, err := NewReader([]string{"localhost:9095"}, "wallet.debit", "wallet-read-sync-go", discardLogger())
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	cfg := r.Config()

	tests := []struct {
		name string
		got  any
		want any
		why  string
	}{
		{
			name: "WatchPartitionChanges 必須是 true",
			got:  cfg.WatchPartitionChanges, want: true,
			why: "預設 false（地雷 #19）：consumer 比 topic 早啟動時被分到 0 個 partition，之後沒有任何事件會觸發 rebalance，它會永遠收不到訊息而每一項檢查都正常",
		},
		{
			name: "PartitionWatchInterval 要顯式設定",
			got:  cfg.PartitionWatchInterval, want: partitionWatchInterval,
			why: "與 WatchPartitionChanges 成對，只有後者為 true 時才會被使用",
		},
		{
			name: "CommitInterval 必須是 0（同步 commit）",
			got:  cfg.CommitInterval, want: time.Duration(0),
			why: "非 0 會變成背景批次 commit，CommitMessages 只把 offset 丟進 channel 就回 nil，關機時那些 commit 全部消失",
		},
		{
			name: "StartOffset 要是 earliest",
			got:  cfg.StartOffset, want: kafka.FirstOffset,
			why: "Spring Kafka 預設是 latest，剛好相反；讀模型從 latest 開始的話，group 建立之前的流水永遠不會進讀端而且沒有錯誤訊息",
		},
		{
			name: "MaxWait 要短於 docker stop 的寬限期",
			got:  cfg.MaxWait < 10*time.Second, want: true,
			why: "kafka-go 預設 10 秒，加上 LeaveGroup 會逼近 10 秒寬限期；被 SIGKILL 就留下幽靈 member（地雷 #25）",
		},
		{
			name: "GroupID 有帶進去",
			got:  cfg.GroupID, want: "wallet-read-sync-go",
			why: "沒有 GroupID 的 Reader 是「指定 partition 直讀」模式，不會有 offset 管理也不會有 rebalance",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v\n原因: %s", tt.got, tt.want, tt.why)
			}
		})
	}
}

// ⭐ TestNewReaderRejectsSharedGroupID 釘住地雷 #22。
//
// Go 版與 Java 版並存時共用 group id，Kafka 會把 partition 分給兩邊，
// **每則事件只有其中一版收到**。症狀看起來像「Go 版隨機漏訊息」，
// 但兩邊的日誌都正常——所以這件事只能在啟動時擋，事後查不出來。
func TestNewReaderRejectsSharedGroupID(t *testing.T) {
	tests := []struct {
		name    string
		groupID string
		wantErr bool
	}{
		{name: "帶 -go 後綴", groupID: "wallet-read-sync-go", wantErr: false},
		{name: "沿用 Java 版的 group id", groupID: "wallet-read-sync", wantErr: true},
		{name: "後綴在中間不算", groupID: "wallet-go-read-sync", wantErr: true},
		{name: "空字串", groupID: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewReader([]string{"localhost:9095"}, "wallet.debit", tt.groupID, discardLogger())
			if err == nil {
				t.Cleanup(func() { _ = r.Close() })
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewReader(%q) error = %v, wantErr = %v", tt.groupID, err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.groupID) {
				// 錯誤訊息要帶著那個被拒絕的值，否則設定是從環境變數來的時候，
				// 讀日誌的人得自己去猜到底哪個 group id 出問題。
				t.Errorf("錯誤訊息 %q 沒有包含被拒絕的 group id %q", err, tt.groupID)
			}
		})
	}
}
