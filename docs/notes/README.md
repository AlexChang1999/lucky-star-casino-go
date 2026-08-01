# docs/notes — 團隊 Java 版的實地查證筆記

> **這個資料夾裡的東西描述「被替換掉的那個系統」，不是本專案的設計。**
> 本專案的定案永遠以 `docs/藍圖.md` 與 `docs/ADR-*.md` 為準。

## 為什麼要留這些

`AGENTS.md` §1 說「不確定就去讀 Java 原始碼」——但每次都從 555 個 `.java` 檔重查一次，
成本高且結論會漂移。這些筆記是**已經查證過的中間產物**：每一條都標了檔名與行號，
可以隨時回原始碼複驗。

它們的價值不在「Java 怎麼寫」，在**「為什麼要這樣寫」**——
Outbox 為什麼存在、指令與事件為什麼必須分兩個 topic、
`ZADD` 為什麼不能加去重、分散式鎖的取鎖與 TTL 為什麼必須是同一個指令。
**這些理由與語言無關，Go 版要原封不動照做。**

## 清單

| 檔案 | 內容 | 對應的重寫 Phase |
|---|---|---|
| `Java版-CQRS-與指令事件分離.md` | Transactional Outbox 的完整資料流、為什麼不同步雙寫（4 個理由）、`wallet.credit` vs `wallet.credit.request` 的無限迴圈事故 | **A（wallet）** |
| `Java版-Redis-用途全解.md` | 全部 Redis key 的 inventory（含 TTL 與讀寫方）、JWT 撤銷三件套、`ZADD`/`ZINCRBY` 冪等判準、捕魚 Lua CAS、fail-open vs fail-closed | A / B（gateway）/ C（game）/ E（rank） |

## ⚠️ 地雷編號對照（團隊 repo → 本 repo）

兩份筆記寫的是**團隊 `AGENTS.md` 的雷區編號**，本 repo 的 `AGENTS.md` §2 重新編過號
（A/B/C 三類、共 29 條）。引用時務必換算，**不要直接把數字抄進本 repo 的文件**：

| 團隊 # | 內容 | 本 repo # |
|---|---|---|
| 5 | wallet 是雙資料源，沒有跨庫交易 | **#1** |
| 6 | `wallet.credit` 是事件、`.request` 才是指令 | **#2** |
| 8 | 帳務＝冪等鍵 + 樂觀鎖防超扣 | **#3** |
| 16 | 捕魚血量/傷害模型（含 Session 樂觀鎖 ADR-008） | **#11** |
| 17 | 風控 RTP 門檻 per-game 且含本金 | **#10** |
| 18 | wallet `sub_type` 新增要四同步 | **#8** |
| 20 | 禮品商城在 wallet 內，不是獨立服務 | **#9** |
| 21 | 後台 JWT 與玩家 JWT 是兩套 secret | **#14** |
| 22 | game→wallet credit 失敗落補償單，冪等鍵不可換 | **#4** |
| 23 | wallet 事件走 Transactional Outbox | **#5** |
| 24 | 非冪等累加要去重、冪等寫入不可去重 | **#6** |
| 25 | Micrometer gauge 回呼不可直接查 DB | **#23**（轉成 Go 的 collector） |
| 26 | 舊 DB volume 缺 migration 開機即死 | **#17** |
| 30 | `mem_limit` 與 `-Xmx` 成對 | **#24**（轉成 `GOMEMLIMIT`） |
| 31 | Gateway 兩套獨立限流 + 熔斷 | **#15** |
| 32 | Redis 不只是快取，是主儲存 | **#16** |

## 維護規則

- **這些是唯讀快照，不要「更新」它們去追團隊 repo 的變化**。
  日期就寫在檔頭；過期了就重查一份新的，舊的保留。
- 讀出**新的**業務規則時，判斷它屬於哪一類：
  - 「不知道會踩坑」→ 進 `AGENTS.md` §2 地雷清單
  - 「架構決策」→ 進 `docs/ADR-*.md`
  - 「查證過程與細節」→ 才留在這裡
- 已修正的原文錯誤（例如 CQRS 那份檔頭的 `D:\` 路徑）就地改掉並註明，
  不要為了「保持原樣」而留著錯的資訊。
