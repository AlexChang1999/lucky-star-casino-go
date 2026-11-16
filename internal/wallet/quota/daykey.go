package quota

import (
	"fmt"
	"time"

	// ⭐ 這個空白匯入是**必要的**，不是保險。理由見 dayZone。
	_ "time/tzdata"
)

// dayZone 是「當日」的時區。
//
// ⚠️⚠️ **這裡必須是 Asia/Taipei，而且不可以「順手改成 UTC」**——
// 這是 AGENTS.md 地雷 #39 的**鏡像**，方向相反，很容易搞錯：
//
//   - 地雷 #39 講的是**排程時間**：Java 的 `@Scheduled(cron="0 0 4 * * *")`
//     那個 `4` 是台北的凌晨四點，本專案一律 UTC，所以**照抄數字會錯**
//     （要換算成 UTC 20 點）。
//   - 這裡講的是**業務日界**：「今天有沒有領過破產救助」對玩家而言是
//     **台北的今天**。把它改成 UTC 會讓日界移到台北早上 8 點——
//     於是玩家在 07:59 領過之後，08:00 又可以再領一次。
//
// 判準一句話：**排程時間是實作細節（換算），日界是業務語義（保留）。**
// 兩者都來自 Java 的 `ZoneId.of("Asia/Taipei")`，但處置完全相反。
//
// ⚠️ 而且這個錯誤**沒有錯誤訊息**：改成 UTC 之後所有測試照樣綠
// （測試自己也會用同一個時區算期望值），只有真實玩家會在某個早上發現
// 自己一天領了兩次救助。
//
// ⭐ **為什麼需要 `_ "time/tzdata"`**：本專案的最終映像是 **scratch**
// （Dockerfile 有寫），裡面**一張時區表都沒有**。`time.LoadLocation` 會依序找
// `$ZONEINFO` → 系統的 /usr/share/zoneinfo → `$GOROOT/lib/time/zoneinfo.zip`，
// 在 scratch 裡三個都不存在，於是回傳 `unknown time zone Asia/Taipei`。
//
// 最惡劣的地方是**它只在容器裡壞**：開發機裝了 Go，`$GOROOT` 的 zoneinfo.zip
// 就在那裡，所以本機測試全綠、CI（也有 Go）全綠，**只有部署上去才炸**。
// `time/tzdata` 把整份時區資料庫編進 binary，
// 於是「能不能解析時區」在建置時就決定了，不再取決於映像裡有什麼。
//
// ⭐ **2026-08-13 實測**（`CGO_ENABLED=0 GOOS=linux`、`-trimpath -ldflags="-s -w"`、
// 兩顆 binary 放進同一個 scratch 映像跑）：
//
//	沒有這行匯入   FAIL: unknown time zone Asia/Taipei（exit 1）   1,650,850 bytes
//	有這行匯入     OK: Asia/Taipei 2026-08-13T12:00:00+08:00       2,060,450 bytes
//
// 代價是 **+409,600 bytes（剛好 400 KiB）**。對照 notify-go 的 18.6 MB 映像，
// 那是 2% 出頭——換掉一個「只有正式環境會發生、且沒有測試抓得到」的失敗模式。
//
// ⚠️ 匯入位置刻意放在**這個葉子套件**而不是 `cmd/wallet/main.go`：
// 放 main 的話，之後任何一個 import quota 的新服務（rank、admin）都得記得
// 自己再加一次，而忘記的症狀又是「本機好好的、容器炸掉」。
// 代價是所有 import quota 的 binary 都會變大，但那正好是需要它的那些。
var dayZone = mustLoadZone("Asia/Taipei")

// mustLoadZone 在啟動時就把時區載好，載不到直接 panic。
//
// ⚠️ panic 看起來很粗暴，但這裡是**對的失敗方式**，因為這個錯誤只有一種成因：
// 有人把上面那行 `_ "time/tzdata"` 刪掉了。那是建置期的錯誤，不是執行期狀況。
//
// 而它的傳播路徑很特別，值得知道：刪掉之後**本機與 CI 都不會紅**
// （兩者都有 $GOROOT/lib/time/zoneinfo.zip），只有 scratch 容器會炸。
// 此時 panic 的效果是「容器一啟動就 crash loop」——**吵、立刻、在部署當下**。
// 相對地，如果改成回傳 error 並在呼叫端降級成 UTC，症狀會變成
// 「服務跑得好好的，但玩家每天早上 8 點可以多領一次救助」，
// 那種問題要等對帳才會發現。**寧可開不起來，也不要算錯帳**（同 main.go 的開機自檢）。
func mustLoadZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf(
			"quota: 載入時區 %s 失敗（是不是把 `_ \"time/tzdata\"` 匯入刪掉了？）: %v", name, err))
	}
	return loc
}

// dayStamp 回傳 t 在台北當日的日期字串，格式為 ISO-8601 的 `2026-08-13`。
//
// ⚠️ 格式必須逐字對齊 Java 的 `LocalDate.toString()`，因為它是 **Redis key 的
// 一部分**，而 key 名稱是服務之間的隱含契約（docs/notes/Java版-Redis-用途全解.md
// §3.1 那條雷）。開發期兩版各自有自己的 Redis（6379 / 6380）撞不到，
// 但灰度切換時兩版會對著同一個 Redis 跑——那時格式差一個字元，
// 就等於「Java 認為玩家今天領過、Go 認為沒有」，於是**同一天領兩次**。
func dayStamp(t time.Time) string {
	return t.In(dayZone).Format(time.DateOnly)
}

// DayStamp 是 dayStamp 的匯出版，給需要「與額度 key 同一個日期」的呼叫端
// （破產救助的入帳冪等鍵）。不要在別處重寫一份——日界只能有一個定義。
func DayStamp(t time.Time) string { return dayStamp(t) }

// untilNextMidnight 回傳從 t 到台北下一個午夜的時距，也就是當日 key 的 TTL。
//
// ⭐ **為什麼是相對時距（TTL）而不是絕對時刻（EXPIREAT / PXAT）**：
// Redis 兩種都支援，而且 Java 版兩種都用了（claim 用相對、gift 計數器用絕對，
// 它自己不一致）。這裡一律用**相對**，理由是**時鐘偏移**：
//   - 絕對時刻是拿「我們算出來的牆上時間」去對「Redis 的牆上時間」，
//     兩台機器差 5 分鐘，key 就早 5 分鐘或晚 5 分鐘過期。
//   - 相對時距只要求兩邊的**時間流速**一致（那是石英晶體的事，不是 NTP 的事），
//     Redis 只負責倒數，偏移完全不影響。
//
// ⚠️ **回傳值一定要夾在 1 毫秒以上**，這是一個沒有錯誤訊息的坑：
// `PEXPIRE key 0` 在 Redis 裡不是「不過期」也不是「立刻過期」，
// 而是**當場刪掉這個 key**。剛好在午夜前那一瞬間進來的請求，
// 額度 key 會一寫進去就被自己刪掉，玩家的當日額度瞬間歸零重來。
//
// ⚠️⚠️ 夾在「大於 0」是**不夠的**，這個版本我第一次就寫錯了：
// 23:59:59.9999999 算出來是 100 奈秒，是正數所以通過了「> 0」的檢查，
// 但呼叫端傳進 Lua 的是 `.Milliseconds()`，而 100ns 轉成毫秒是 **0**。
// 夾的門檻必須與**使用它的單位**一致，否則夾了等於沒夾。
// 由 TestUntilNextMidnightNeverRoundsDownToZeroMillis 釘住。
//
// （附帶一提：`SetNX` 那條路徑剛好逃過一劫——go-redis 的 formatMs 會把
// 小於 1ms 的時距截成 1ms 並印一行警告。**但那是別人的函式庫幫我們兜底，
// 不是我們的正確性。**換一個 client 或改用 Lua 就沒了。）
//
// ⚠️ 用 `time.Date(y, m, d+1, ...)` 而不是 `t.Add(24*time.Hour)`：
// 後者在有日光節約時間的時區一年會錯兩次（那兩天不是 24 小時）。
// 台北 1979 年後沒有 DST，所以今天兩種寫法結果相同——
// 但這段邏輯之後會被複製到別的時區用，而複製的人不會回來讀這段註解。
// **寫成一開始就對的形狀，比寫一句「台北沒有 DST」的註解安全。**
func untilNextMidnight(t time.Time) time.Duration {
	local := t.In(dayZone)
	y, m, d := local.Date()
	// time.Date 會自己處理進位（12/31 + 1 天 = 隔年 1/1），不必特別判斷月底。
	next := time.Date(y, m, d+1, 0, 0, 0, 0, dayZone)
	if ttl := next.Sub(local); ttl >= time.Millisecond {
		return ttl
	}
	return time.Millisecond
}
