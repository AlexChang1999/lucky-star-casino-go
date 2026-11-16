# ADR-006 — wallet 分層命名對齊 go-layered-arch：repository、model/{dto,enum}、apiprotocol

| 項目 | 內容 |
|------|------|
| **狀態** | ✅ 已接受（Accepted） |
| **日期** | 2026-11-16 |
| **決策者** | 單人（作品集專案） |
| **影響範圍** | `internal/wallet` 的套件命名與位置。**沒有行為變更**（HTTP 契約、SQL、事件皆不動） |
| **Supersedes** | `docs/ADR-004` 的套件命名（`store`、handler 內的 request/response 型別與訊息常數）；ADR-004 的框架與分層方向不變 |

---

## 決策

| 以前 | 現在 | 為什麼 |
|---|---|---|
| `internal/wallet/store`（型別 `Repository`） | `internal/wallet/repository` | 套件名與型別名一致，也是作者慣用的分層詞彙；`store` 在 Go 裡太泛（`quota.Store`、`platform/store` 各是別的東西） |
| handler 內 `debitRequest` / `creditRequest` / `giftRequest` | `model/dto/input`：`DebitReq` / `CreditReq` / `GiftReq` | dto 只放資料，轉換與驗證留在 handler（`debitToMovement`、`validateGift`） |
| handler 內 `debitResponse` 等 | `model/dto/output`：`DebitRes` / `CreditRes` / `GiftRes` / `BankruptcyAidRes` | 「回應」一律叫 output，欄位順序仍逐位元組對齊 Java |
| `repository.GiftStatus` | `model/enum`：`GiftStatus` | repository 與 service 都要用，定義在任一邊都會讓另一邊反向 import |
| handler 內的 `envelope` 與一堆 `xxxMessage` 常數 | `apiprotocol`：`Body`（`OK` / `Fail`）與對外訊息 | 對外契約（信封形狀、訊息文字）集中在一處，不認得 fiber |

## 沒有照 skill 做的地方（刻意）

1. **`apiprotocol` 沒有數字狀態碼表**。go-layered-arch 的 `apiprotocol` 是 iota 狀態碼 + `StatusMsg`，
   但 Java 版 wallet 對外是「HTTP 狀態 + 訊息文字」，沒有自訂數字碼；發明一套會變成契約漂移。
   結果碼仍是 `service.Code`（內部用），HTTP 狀態由 handler 決定。
2. **`New()` 仍回傳具體型別，interface 定義在消費端**（`service.Repository`、`service.GiftRepository`）。
   skill 的「provider 回傳 interface」與專案 `CLAUDE.md` §2 衝突，以專案為準。
3. **`domain` 沒有拆進 `model`**。`domain.TxType` / `SubType` 帶著 `subTypeDirection` 與建構子驗證，
   是帳務不變式不是單純列舉；搬動它會動到帳務核心，而這次的目標只是命名。
4. **`quota.Store`、`internal/platform/store` 沒改名**：前者是 Redis 額度層，後者是 DB 連線工廠，
   都不是 repository。
5. **沒有 `router/` 獨立套件**：`handler/router.go` 維持原樣（只有路由宣告）。

## 驗證

`go vet`（含 `-tags=infra`）、`go test -race ./...`、`golangci-lint run`；對真實 MySQL 8.4 跑
`go test -race -tags=infra ./internal/wallet/repository/ ./internal/wallet/outbox/`。
handler 既有的信封逐位元組比對與訊息逐字比對測試未改動、全數通過。
