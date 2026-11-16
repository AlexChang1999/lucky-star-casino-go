# ADR-004 — 業務服務 HTTP 框架由 Gin 改為 Fiber，並導入 handler / service / store 分層

| 項目 | 內容 |
|------|------|
| **狀態** | ✅ 已接受（Accepted） |
| **日期** | 2026-11-15 |
| **決策者** | 單人（作品集專案） |
| **影響範圍** | 業務服務的 HTTP 層。wallet 是第一個（`internal/wallet/handler`、`internal/wallet/service`） |
| **Supersedes** | `docs/藍圖.md` §3 的「Gin ✅ 做 / Echo、Fiber ❌ 不做」兩列 |
| **後續** | 2026-11-16 起，套件命名（`store` → `repository`、`model/dto`、`apiprotocol`）由 `docs/ADR-006` 取代；本檔的框架選擇與分層方向不變 |

---

## 背景

藍圖 §3 選 Gin 的理由是「9 個職缺提及最多」，並明寫 Fiber「Gin 已覆蓋同一格能力，
多學一個框架換不到任何東西」。這個判斷在**求職覆蓋面**上至今仍成立。

現在改選 Fiber 的原因是**作者的決定**：目標職缺／團隊慣用的分層（router → handler →
service → repository，DI 在 `main`）以 Fiber 為載體。這是一個**以學習與對齊為目的的取捨**，
不是 Fiber 在技術上勝過 Gin——下面逐條回應。

## 逐條回應原本的理由

| 原理由 | 現況 |
|---|---|
| Gin 職缺提及最多 | **依然成立**。改選 Fiber 放棄了一部分這個優勢；以「Gin 與 Fiber 的 handler 形狀幾乎一對一」作為緩解，面試可兩者都談 |
| 多學一個框架換不到東西 | **當初判斷對、現在情境變了**：換框架本身確實沒有技術收益，收益來自「對齊目標分層慣例」與「親身比較兩者差異」（見下方地雷） |
| 生態與招募現實對齊 | Fiber 生態足夠本專案用（本切片只需路由、JSON、中介層） |

## 決策

1. 業務服務 HTTP 層改用 **Fiber v3**（`github.com/gofiber/fiber/v3`）。推播服務維持標準庫，不變。
2. wallet 的 HTTP 路徑拆成三層，**依賴只往下**：

   ```
   cmd/wallet/main.go（手動組裝，無 DI 容器）
        → internal/wallet/handler   解 JSON、請求格式驗證、結果碼→狀態碼、信封
        → internal/wallet/service   store 錯誤分類成 Code、錯誤只在起點記一次日誌
        → internal/wallet/repository     SQL（不變）
   ```
3. **`go-layered-arch` skill 的適配**（該 skill 為 `src/container` 結構而寫，與 `CLAUDE.md` §2 有三處衝突，一律以 `CLAUDE.md` 為準）：

   | skill 規則 | 本專案 |
   |---|---|
   | `container.Register()` 統一組裝 | ❌ 不用容器，`main.go` 明確傳參 |
   | 各層 `New()` 回傳**自己的**介面 | ❌ interface 定義在**消費端**；`service.New` 回傳 `*Service`，handler 自己宣告它需要的小介面 |
   | `*co.APIError` + `apiprotocol` 狀態碼體系 | ❌ 改用 `service.Code`（4 個）+ handler 內的對外訊息常數；對外格式必須等於 Java 版 |
   | 依賴只往下、handler 無業務判斷、service 不 import 框架、錯誤只在起點記一次 | ✅ 採用 |

## 已知代價與地雷（實測／文件查證）

- **`fiber.Ctx` 不是會取消的 context**：v3 的 `Ctx` 雖然實作 `context.Context`，但
  `Done()` 回 `nil`、`Err()` 恆為 nil（官方文件明寫）。把 `c` 直接傳給 DB 呼叫，
  **逾時與取消永遠不會發生**，而且沒有錯誤訊息。一律傳 `c.Context()`。
  Gin 的 `c.Request.Context()` 在客戶端斷線時會取消；fasthttp 沒有等價行為，
  所以帳務呼叫的上限只剩 store 層自己的逾時。
- **fasthttp 沒有 `ReadHeaderTimeout`**：Slowloris 的防線由 `ReadTimeout` 兜底，
  比 net/http 粗。`config.HTTPServer.ReadHeaderTimeout` 在 Fiber 路徑上不再使用。
- **`fiber.Ctx` 會被池化重用**：handler 返回後不可保留 `c` 或其回傳的 `[]byte`／`string`
  （fasthttp 零拷貝）。本切片所有帳務呼叫皆同步完成，不受影響。
- **與標準庫生態的接縫**：prometheus／otel 的 net/http middleware 不能直接掛，
  要用 adaptor 或 Fiber 版套件。Phase H 前沒有影響。

## 驗證方式

`internal/wallet/handler` 的測試逐條移植自舊 `httpapi` 的測試（回應 body 逐位元組比對、
驗證訊息逐字比對、store 錯誤→狀態碼表格）；`go test -race ./...` 全綠；
Gin 於最後一個 commit 自 `go.mod` 移除。
