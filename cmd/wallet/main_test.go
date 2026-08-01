package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/platform/config"
)

// testHTTPConfig 是短逾時版的伺服器設定，讓測試失敗時快點失敗。
func testHTTPConfig(port int) config.HTTPServer {
	return config.HTTPServer{
		Port:              port,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
		ShutdownTimeout:   5 * time.Second,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// listenWildcard 綁一個 OS 指定的埠，並回傳 listener 與埠號。
//
// ⚠️ 位址必須是 wildcard（`:0`）而不是 `127.0.0.1:0`，因為 serve 綁的是
// `cfg.Addr()`＝`:port`。**這不是潔癖，是一條實際踩到的平台差異**：
// Windows 允許「0.0.0.0:P」與「127.0.0.1:P」同時存在（兩個不同的位址），
// 於是拿 127.0.0.1 去占埠，serve 照樣綁得起來——TestServeReportsListenFailure
// 因此不會拿到錯誤，而是真的開始服務並卡在 select 上，測試永遠跑不完。
// Linux 那邊 wildcard 與具體位址互斥，同一份測試會過。
func listenWildcard(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("借用埠失敗: %v", err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// freePort 借一個埠再馬上還掉。
//
// ⚠️ 這中間有一個理論上的競態（還掉之後、serve 綁上去之前被別人搶走）。
// 標準庫的 httptest 用的是「自己持有 listener」的做法，那需要把 serve 改成
// 接受 net.Listener——為了測試改動正式程式碼的形狀，在這裡不划算：
// 本機測試撞到那個窗口的機率遠低於一般的 flaky 來源。
func freePort(t *testing.T) int {
	t.Helper()
	ln, port := listenWildcard(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("歸還埠失敗: %v", err)
	}
	return port
}

// waitFor 反覆跑 cond 直到成立或逾時。用來取代 time.Sleep——
// sleep 在快的機器上浪費時間、在慢的機器上還是會 flaky。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待「%s」逾時", what)
}

func canDial(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ⭐ TestServeGracefulShutdown 釘住兩件在 Windows 上手動測不到的事。
//
// 為什麼手動測不到：Windows 沒有真正的 signal，Git Bash 的 `kill -TERM` 對原生
// exe 走的是 TerminateProcess——Go 的 signal handler 根本不會跑。也就是說
// 「本機 smoke test 過了」完全不代表關機路徑是對的，而它在 Linux 容器裡
// （docker stop / K8s）每一次部署都會走到。
//
// 這個測試繞過 signal，直接對 serve 取消 context，於是兩件事都驗得到：
//
//  1. **收工時 in-flight 的請求要跑完**，不是被硬斷。斷掉的話，一筆已經扣款、
//     還沒回應的 debit 會讓呼叫端看到逾時 → 它會重試 → 靠冪等鍵才不會重複扣。
//     也就是說關機不優雅時，安全網是冪等鍵而不是我們。
//  2. **serve 要乾淨返回 nil**。這一條是 `context.WithoutCancel` 的回歸測試：
//     少了它，shutdown 用的 context 一出生就已經 done，Shutdown 立刻放棄，
//     serve 會回「優雅關機逾時」——而這個測試會紅在那一行。
func TestServeGracefulShutdown(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	inHandler := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(inHandler)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, discardLogger(), testHTTPConfig(port), mux) }()

	waitFor(t, "伺服器開始接受連線", func() bool { return canDial(addr) })

	// 送一個會卡在 handler 裡的請求，模擬「關機時還有請求在跑」。
	requested := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow") //nolint:noctx // 測試裡的請求由 release 控制
		if err != nil {
			requested <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.ReadAll(resp.Body); err != nil {
			requested <- err
			return
		}
		if resp.StatusCode != http.StatusOK {
			requested <- fmt.Errorf("狀態碼 = %d, want 200", resp.StatusCode)
			return
		}
		requested <- nil
	}()

	<-inHandler // 請求確實已經進到 handler 裡
	cancel()    // 等同收到 SIGTERM

	// 等到 listener 真的關掉，才放行那個請求——這樣才能證明它是在
	// **Shutdown 已經開始之後**才完成的，而不是搶在關機前跑完。
	// 用輪詢而不是 sleep：sleep 在快的機器上浪費時間、慢的機器上照樣 flaky。
	waitFor(t, "listener 停止接受新連線", func() bool { return !canDial(addr) })
	close(release)

	if err := <-requested; err != nil {
		t.Errorf("關機期間 in-flight 的請求應該要跑完，卻失敗了: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve 應該乾淨返回，得到: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve 沒有在關機後返回")
	}
}

// TestServeReportsListenFailure 釘住「綁不上埠要當成啟動失敗」。
//
// ⚠️ 這條容易被寫成「log 一下就算了」。埠被占用時服務必須**開不起來**：
// 讓它活著但沒在聽，健康檢查會過（進程還在）、日誌只有一行警告，
// 而所有請求都打不進來——典型的「每一項檢查都顯示正常」故障。
func TestServeReportsListenFailure(t *testing.T) {
	ln, port := listenWildcard(t)
	defer func() { _ = ln.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ⚠️ 加逾時保險：serve 若因為某個平台差異而**綁得起來**，它會卡在 select
	// 等關機訊號，測試就永遠跑不完。讓它在這裡失敗，比讓 CI 掛住好。
	failed := make(chan error, 1)
	go func() { failed <- serve(ctx, discardLogger(), testHTTPConfig(port), http.NewServeMux()) }()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("埠已被占用時 serve 必須回錯誤")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("埠已被占用，serve 卻沒有回報失敗（它是不是真的綁起來了？）")
	}
}
