# 幸運星幣城 — Go 全面重構

把一個**正在跑的** Java 21 / Spring Boot 微服務平台（7 個服務、555 個 `.java`）
逐服務重構為 Go，並用**黑箱契約測試**證明每一步都等價。

> **這不是「用 Go 寫一個娛樂城」。**
> 差別在於：每個行為都有一個**已存在的正確答案**，工作是對齊它，不是發明它。
> 而「證明對齊了」需要證據——那個證據就是同一份契約測試對 Java 版與 Go 版都綠。

---

## 目前狀態

🚧 **骨架階段**。基礎設施與治理層已就緒，業務服務尚未開工。

| 項目 | 狀態 |
|---|---|
| 基礎設施（MySQL / MongoDB / Redis / Kafka） | ✅ compose 一鍵起，煙霧測試全綠 |
| 連線層 `internal/platform`（設定驗證、連線池） | ✅ 含表格驅動測試 |
| 治理層（`AGENTS.md` / `CLAUDE.md` / subagent） | ✅ |
| ADR-000 為什麼現在改用 Go | ✅ |
| ADR-001 資料層：MySQL + MongoDB | ✅ |
| Phase A `wallet` 重構 | ⬜ 下一步 |

**已完成的前導專案**：`notification-service` →
[**lucky-star-notify-go**](https://github.com/AlexChang1999/Lucky_Star_Notify_Go)
（自幹 STOMP 1.2 伺服器子集、契約測試 14/14 兩邊全綠、映像 18.6 MB、
閒置 12.9 MiB）。**那是本重構的方法論驗證。**

---

## 技術棧

| 層 | 選型 | 一句話理由 |
|---|---|---|
| 語言 | Go 1.25+ | — |
| HTTP（業務服務） | Gin | 生態與招募現實對齊 |
| HTTP（推播服務） | 標準庫 `net/http` | 自幹 STOMP 是賣點，**不為了統一而改** |
| 服務間通訊 | gRPC + Protobuf | 取代現有的服務間 REST |
| 寫入主庫 | **MySQL 8.4** | 唯一真相。見 [ADR-001](docs/ADR-001-資料層-MySQL-與-MongoDB.md) |
| 讀端 | **MongoDB 8.0** | CQRS 讀模型是反正規化文件 |
| 鎖 / ZSET / session | Redis 7 | ⚠️ **主儲存不是快取** |
| 訊息 | Kafka（`segmentio/kafka-go`） | 純 Go，可靜態編譯 |
| Log | `log/slog` | 標準庫，不引入 zap/zerolog |

**刻意不做**：Echo/Fiber、NATS/RabbitMQ、Kong/APISIX、Elasticsearch（延後）、
自建區塊鏈節點、冷熱錢包、Vault/KMS、GKE。理由見 [藍圖 §9](docs/藍圖.md)。

> **「不做」的理由比「做」更重要。** 履歷上多一個 Elasticsearch，
> 面試被問「你的資料量為什麼需要它」答不出來，是負分。

---

## 快速開始

```bash
# 1. 基礎設施
cp deploy/.env.example deploy/.env          # 首次，改掉裡面的密碼
docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env up -d --wait

# 2. schema。⚠️ compose up 之後 schema 是空的——這一步不能省
set -a && . deploy/.env && set +a
go run ./cmd/migrate up
go run ./cmd/migrate status                  # 每個版本都該是 applied

# 3. 測試
go test -race ./...                          # 單元測試
go test -race -tags=infra ./...              # 需要基礎設施真的起來

# 4. 收工。⚠️ 不要隨手加 -v，Redis 是主儲存
docker compose -f deploy/docker-compose.infra.yml --env-file deploy/.env down
```

> **為什麼 schema 不是 `compose up` 就好？**
> MySQL 官方映像的 `/docker-entrypoint-initdb.d` **只在 volume 全新時執行**，
> 也就是它天生做不到「改 schema」——第二次改欄位就會變成「新環境有、舊環境沒有」
> 而且兩邊都不報錯。所以 schema 由 [goose](docs/ADR-003-schema-migration-以-goose-管理.md) 管，
> 而服務啟動時會檢查版本，忘了跑會**開不起來**而不是等到第一筆交易才炸。

**對外埠**（與團隊 repo、`notify-go` 全部錯開，三套要能同時跑）：

| 服務 | 本專案 | 團隊 repo | notify-go |
|---|---|---|---|
| MySQL | **3308** | 3307 | — |
| MongoDB | **27018** | — | — |
| Redis | **6380** | 6379 | — |
| Kafka | **9095** | 9092 | 9094 |

---

## 這個專案想證明什麼

不是「我會用 Go」，而是這四件事：

1. **能讀懂一份架構決策，並且有根據地推翻它。**
   團隊的 ADR-000 正式決定過「用 Java 而不是 Go」。
   [本專案的 ADR-000](docs/ADR-000-為什麼現在改用-Go.md) 逐條回應它：
   哪些理由依然成立、哪些因情境改變而不再成立、**哪些當初就判斷錯了**。

2. **能證明重寫是等價的，而不是「看起來對」。**
   同一份黑箱契約測試對 Java 版與 Go 版都要綠。這是唯一的證據。

3. **知道無聲失敗長什麼樣。**
   [`AGENTS.md` §2](AGENTS.md) 累積了 31 條地雷，其中多數的共同點是
   **完全沒有錯誤訊息**——健康檢查過、日誌乾淨、指標正常，就是不動作。

4. **能誠實地報告代價。**
   每份 ADR 都有一節寫「這個決定失去了什麼」。
   效能數字一律實測，同機施壓要標明污染。

---

## 文件

| 檔案 | 內容 |
|---|---|
| [`docs/藍圖.md`](docs/藍圖.md) | **單一真相來源**：取捨原則、技術選型、重寫順序、時間估計 |
| [`AGENTS.md`](AGENTS.md) | AI 與人類開工前必讀：31 條地雷、約定、驗證指令 |
| [`CLAUDE.md`](CLAUDE.md) | AI 協作準則（教學模式、規模相稱的抽象、測試先行） |
| [`docs/ADR-000`](docs/ADR-000-為什麼現在改用-Go.md) | 為什麼現在改用 Go |
| [`docs/ADR-001`](docs/ADR-001-資料層-MySQL-與-MongoDB.md) | 資料層：MySQL + MongoDB |
| [`docs/ADR-002`](docs/ADR-002-wallet-帳務語句在-MySQL-的等價實作.md) | 帳務語句在 MySQL 的等價實作（沒有 `RETURNING` 怎麼辦） |
| [`docs/ADR-003`](docs/ADR-003-schema-migration-以-goose-管理.md) | schema migration：goose，且不在啟動時自動跑 |
| [`.claude/agents/`](.claude/agents/README.md) | 三個 subagent 與「為什麼是三個」 |

---

## 分支流程

- `main` — 穩定版
- `develop` — 整合分支，**PR 一律進這裡**
- `feat/*` `fix/*` `chore/*` — 工作分支

---

## 授權與定位

個人作品集專案。原始 Java 專案 `Lucky_Star_Casino` 為團隊作品，
本 repo **不含**其任何原始碼，僅作為唯讀的行為參考。
