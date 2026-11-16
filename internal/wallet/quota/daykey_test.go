package quota

import (
	"testing"
	"time"
)

// TestDayZoneIsTaipei 釘住「時區真的載得到，而且是台北」。
//
// ⭐ 這個測試在本機與 CI 上**永遠會過**（兩者都有 $GOROOT/lib/time/zoneinfo.zip），
// 所以它防的**不是**「tzdata 匯入被刪掉」——那件事只有 scratch 容器會炸，
// 見 daykey.go 的說明。它防的是另一件事：**有人把時區改成 UTC**。
// 那個改動在容器裡跑得好好的，只是玩家每天早上 8 點多一次領取機會。
func TestDayZoneIsTaipei(t *testing.T) {
	if got := dayZone.String(); got != "Asia/Taipei" {
		t.Fatalf("dayZone = %q, want \"Asia/Taipei\"（日界是業務語義，不可改成 UTC，見 daykey.go）", got)
	}

	// 再確認一次偏移真的是 +8：光看名字不夠，載到一份壞掉的時區表也會叫這個名字。
	_, offset := time.Date(2026, 8, 13, 12, 0, 0, 0, dayZone).Zone()
	if want := 8 * 60 * 60; offset != want {
		t.Fatalf("台北時區偏移 = %d 秒, want %d 秒", offset, want)
	}
}

func TestDayStamp(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{
			// 台北 23:59:59，還是同一天。
			name: "台北日終前一秒",
			at:   time.Date(2026, 8, 13, 15, 59, 59, 0, time.UTC),
			want: "2026-08-13",
		},
		{
			// ⭐ 這一格就是「用 UTC 算日界會錯」的證據：同一個時刻，
			// UTC 說是 8/13，台北說是 8/14。玩家的「今天」是後者。
			name: "台北日界剛過（UTC 仍是前一天）",
			at:   time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC),
			want: "2026-08-14",
		},
		{
			name: "跨月",
			at:   time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC),
			want: "2026-09-01",
		},
		{
			name: "跨年",
			at:   time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC),
			want: "2027-01-01",
		},
		{
			// 傳進來就是台北時間時也要對——實務上 time.Now() 在容器裡是 UTC，
			// 但這一層不該假設呼叫端用哪個時區。
			name: "輸入本身就是台北時間",
			at:   time.Date(2026, 8, 13, 9, 30, 0, 0, dayZone),
			want: "2026-08-13",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dayStamp(tt.at); got != tt.want {
				t.Errorf("dayStamp(%s) = %q, want %q", tt.at.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// TestDayStampFormatMatchesJavaLocalDate 釘住 key 的格式。
//
// Java 用 `LocalDate.toString()`，輸出是 ISO-8601 的 `2026-08-13`
// （零填補、連字號分隔）。這是 Redis key 的一部分，也就是跨版本的隱含契約
// ——差一個字元就等於「Java 認為領過、Go 認為沒領」，同一天領兩次。
func TestDayStampFormatMatchesJavaLocalDate(t *testing.T) {
	// 刻意選個位數的月與日，這是零填補最容易出錯的地方。
	got := dayStamp(time.Date(2026, 1, 5, 12, 0, 0, 0, dayZone))
	if want := "2026-01-05"; got != want {
		t.Fatalf("dayStamp = %q, want %q（Java LocalDate.toString() 會零填補）", got, want)
	}
}

func TestUntilNextMidnight(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want time.Duration
	}{
		{
			name: "台北午夜整點",
			at:   time.Date(2026, 8, 13, 0, 0, 0, 0, dayZone),
			want: 24 * time.Hour,
		},
		{
			name: "台北正午",
			at:   time.Date(2026, 8, 13, 12, 0, 0, 0, dayZone),
			want: 12 * time.Hour,
		},
		{
			name: "台北日終前一秒",
			at:   time.Date(2026, 8, 13, 23, 59, 59, 0, dayZone),
			want: time.Second,
		},
		{
			// 月底：time.Date 的進位讓 8/32 自動變成 9/1，不需要特別判斷。
			name: "跨月",
			at:   time.Date(2026, 8, 31, 23, 0, 0, 0, dayZone),
			want: time.Hour,
		},
		{
			name: "跨年",
			at:   time.Date(2026, 12, 31, 23, 0, 0, 0, dayZone),
			want: time.Hour,
		},
		{
			// 輸入是 UTC 但代表台北的午夜整點，答案一樣是 24 小時。
			name: "UTC 輸入換算後剛好是台北午夜",
			at:   time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC),
			want: 24 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := untilNextMidnight(tt.at); got != tt.want {
				t.Errorf("untilNextMidnight(%s) = %s, want %s", tt.at.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// TestUntilNextMidnightNeverRoundsDownToZeroMillis 釘住那個我第一次寫錯的夾值。
//
// ⭐ 這是整個套件裡最容易被誤判成「不可能發生」的邊界：
// 午夜前不到 1 毫秒進來的請求，TTL 算出來是幾百奈秒——**是正數**，
// 所以「夾在 > 0」看起來沒問題。但呼叫端傳給 Lua 的是 `.Milliseconds()`，
// 幾百奈秒轉成毫秒是 **0**，而 `PEXPIRE key 0` 在 Redis 是**當場刪掉這個 key**。
//
// 後果：那個玩家的當日額度 key 一寫進去就被自己刪掉 → 額度歸零重來 →
// 他在午夜前後那一瞬間可以無限贈送。沒有錯誤訊息，日誌上一切正常。
//
// 判準：**夾值的門檻必須與使用它的單位一致。**
func TestUntilNextMidnightNeverRoundsDownToZeroMillis(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
	}{
		{"午夜前 1 奈秒", time.Date(2026, 8, 13, 23, 59, 59, 999_999_999, dayZone)},
		{"午夜前 100 微秒", time.Date(2026, 8, 13, 23, 59, 59, 999_900_000, dayZone)},
		{"午夜前 999 微秒", time.Date(2026, 8, 13, 23, 59, 59, 999_001_000, dayZone)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ttl := untilNextMidnight(tt.at)
			if ttl < time.Millisecond {
				t.Errorf("untilNextMidnight = %s，小於 1 毫秒", ttl)
			}
			// 這一行才是真正的斷言：Lua 收到的是這個數字。
			if ms := ttl.Milliseconds(); ms < 1 {
				t.Errorf("傳進 PEXPIRE 的毫秒數 = %d，Redis 會把 key 直接刪掉", ms)
			}
		})
	}
}

// TestBuiltKeysMatchJava 釘住三個 key 的組裝格式。
//
// 來源是逐行讀過的 Java 原始碼：
//
//	GiftService.java:82-83        "wallet:gift:sent:" + senderId + ":" + date
//	                              "wallet:gift:recv:" + receiverId + ":" + date
//	BankruptcyAidService.java:84  "wallet:bankruptcy-aid:" + playerId + ":" + date
//
// ⚠️ 特別注意 `bankruptcy-aid` 是**連字號**不是底線。這種一個字元的差異
// 在灰度切換時就是「兩版各自記各自的領取狀態」，於是同一天領兩次。
func TestBuiltKeysMatchJava(t *testing.T) {
	const day = "2026-08-13"

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"破產救助", bankruptcyAidKey(42, day), "wallet:bankruptcy-aid:42:2026-08-13"},
		{"贈出額度", giftSentKey(42, day), "wallet:gift:sent:42:2026-08-13"},
		{"收受額度", giftRecvKey(7, day), "wallet:gift:recv:7:2026-08-13"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("key = %q, want %q", tt.got, tt.want)
			}
		})
	}
}
