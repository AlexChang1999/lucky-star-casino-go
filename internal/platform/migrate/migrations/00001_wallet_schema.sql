-- ============================================================================
-- 00001 — wallet 帳務 schema（MySQL 8.4 寫入主庫）
--
-- 來源：團隊 Java repo 的 database/postgres/init.sql（唯讀參考），逐欄位翻譯。
-- 口徑查證紀錄見 docs/notes/Java版-wallet-帳務口徑.md，
-- PostgreSQL → MySQL 的三個非顯而易見差異見 docs/ADR-002。
-- 本檔原本掛在 /docker-entrypoint-initdb.d/，2026-08-01 改由 goose 管理（docs/ADR-003）。
--
-- ⚠️ NO TRANSACTION 是刻意的，不是偷懶（AGENTS.md 地雷 #31）：
-- MySQL 的 DDL 會**隱式 commit**，把 DDL 包在交易裡只會得到假的安全感——
-- 三條 CREATE TABLE 的第二條失敗時，第一條**已經存在了**，而且版本表沒記錄。
-- PostgreSQL 的 DDL 可回滾，所以團隊 Java 版從來不必想這件事。
-- 明寫 NO TRANSACTION 讓「這裡沒有原子性」變成程式碼裡看得到的事實。
-- ⚠️ 這條規則只適用於 DDL。純 DML 的 migration（補資料、改值）**要保留交易**。
--
-- ⚠️ 只有這個 baseline 使用 IF NOT EXISTS，之後的 migration 一律不准用。
-- 理由：它要能在「initdb.d 時代就已經建好表」的既有 volume 上安全地跑一次，
-- 把版本表補記成 1 而不是撞 duplicate。代價是它**看不出定義漂移**
-- （表存在但欄位不同時它靜靜跳過）——那正是 VerifyWalletSchema 存在的理由。
-- ============================================================================

-- +goose NO TRANSACTION
-- +goose Up

-- ── wallets：玩家錢包主表（唯一真相）─────────────────────────────────
-- 金額單位是「星幣」，整數、無小數 —— 所以用 BIGINT 而不是 DECIMAL。
-- CLAUDE.md §5 的規則是「DECIMAL 或整數最小單位」，這裡屬於後者。
-- ⚠️ 不要「順手」改成 DECIMAL：Java 版整條鏈路（DTO、事件 payload、
-- 前端顯示）都是整數，改型別會讓契約測試對不起來。
CREATE TABLE IF NOT EXISTS wallets (
    player_id     BIGINT       NOT NULL,
    balance       BIGINT       NOT NULL DEFAULT 0,   -- 可用餘額（星幣，整數）
    frozen_amount BIGINT       NOT NULL DEFAULT 0,   -- 凍結金額（保留欄位，debit 尚未實作凍結流程）
    version       BIGINT       NOT NULL DEFAULT 0,   -- 樂觀鎖版本號，每次更新 +1
    created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CONSTRAINT pk_wallets                PRIMARY KEY (player_id),
    CONSTRAINT chk_wallets_balance       CHECK (balance >= 0),
    CONSTRAINT chk_wallets_frozen_amount CHECK (frozen_amount >= 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- ⚠️ updated_at 刻意**不加** ON UPDATE CURRENT_TIMESTAMP(6)：
-- 對齊 Java 版——那邊每一條扣款/入帳 SQL 都明寫 `updated_at = CURRENT_TIMESTAMP`。
-- 交給 DB 自動更新看起來比較省事，但它會讓「哪些語句算一次異動」變成隱性知識，
-- 而帳務最不該有隱性知識。DATETIME 而非 TIMESTAMP 是為了避開 2038 上限
-- 與 TIMESTAMP 的隱式時區換算（容器已設 --default-time-zone=+00:00，一律存 UTC）。

-- ── wallet_transactions：帳務流水（寫入端唯一真相）───────────────────
CREATE TABLE IF NOT EXISTS wallet_transactions (
    id              BIGINT       NOT NULL AUTO_INCREMENT,
    player_id       BIGINT       NOT NULL,
    -- ⚠️ type / sub_type 也必須 utf8mb4_bin，理由與 idempotency_key **不同**：
    -- 在預設的 ci 定序下，`CHECK (type IN ('DEBIT',...))` 會**放行小寫 'debit'**
    -- （因為 'debit' = 'DEBIT' 成立），而且 MySQL 原樣存進去。
    -- 於是 Go 端 `tx.Type == "DEBIT"` 是 false、事件 payload 帶著小寫送進 Kafka，
    -- 下游的 switch 直接落到 default。PostgreSQL 會在 INSERT 當場拒絕。
    -- 換句話說：**ci 定序把列舉約束弱化成了「大小寫任意」**。
    type            VARCHAR(10)  COLLATE utf8mb4_bin NOT NULL,  -- DEBIT / CREDIT / BONUS
    sub_type        VARCHAR(20)  COLLATE utf8mb4_bin NOT NULL,
    amount          BIGINT       NOT NULL,
    balance_before  BIGINT       NULL,
    balance_after   BIGINT       NULL,
    -- ⭐ 冪等鍵：全專案最重要的一個欄位。
    -- COLLATE utf8mb4_bin 不是品味問題，是**正確性問題**——見下方大段註解。
    idempotency_key VARCHAR(100) COLLATE utf8mb4_bin NULL,
    reference_id    VARCHAR(100) NULL,                   -- roundId / eventId，供事後對帳
    created_at      DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CONSTRAINT pk_wallet_transactions PRIMARY KEY (id),
    CONSTRAINT uk_wallet_transactions_idempotency_key UNIQUE (idempotency_key),
    CONSTRAINT chk_wt_type   CHECK (type IN ('DEBIT', 'CREDIT', 'BONUS')),
    CONSTRAINT chk_wt_amount CHECK (amount > 0),
    CONSTRAINT chk_wt_sub_type CHECK (sub_type IN (
        'BET', 'SHOP_PURCHASE',
        'WIN', 'CHECKIN', 'TASK', 'GIFT', 'GM_REWARD', 'BANKRUPTCY_AID',
        'DIAMOND_EXCHANGE', 'TOPUP', 'CASHBACK', 'REFUND', 'MONTHLY_REWARD'
    )),
    -- 查詢型索引：對齊 Java 版（流水頁固定是「某玩家、時間倒序、分頁」）。
    -- MySQL 8.0 起真的支援 DESC 索引（5.7 只是解析後忽略），所以這裡照搬有效。
    -- ⚠️ 寫在 CREATE TABLE 裡而不是獨立的 CREATE INDEX，是為了讓整個 migration
    -- 只由 IF NOT EXISTS 的語句組成——MySQL 沒有 CREATE INDEX IF NOT EXISTS，
    -- 拆出去就會在既有 volume 上撞 duplicate key name。
    KEY idx_wallet_transactions_player_id   (player_id),
    KEY idx_wallet_transactions_created_at  (created_at),
    KEY idx_wallet_transactions_player_time (player_id, created_at DESC)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- ⭐⭐ 為什麼這張表的字串欄位全部要 COLLATE utf8mb4_bin（AGENTS.md 地雷 #30）
--
-- MySQL 8.4 的預設定序是 utf8mb4_0900_ai_ci —— **ai = 不分音標、ci = 不分大小寫**。
-- PostgreSQL 的預設定序區分大小寫，所以團隊 Java 版從來沒遇過下面兩件事：
--
-- ① **UNIQUE 冪等鍵被弱化**：'checkin-42' 與 'CHECKIN-42' 在索引裡是同一把鍵。
--    兩個本來不同的鍵被判定重複 → 第二筆被當成「冪等命中」→ **少入一筆帳**。
--    更陰險的是 ai：'e' 與 'é' 也相等，鍵裡含暱稱就會誤判。
-- ② **CHECK IN 列舉被弱化**：`CHECK (type IN ('DEBIT',...))` 放行小寫 'debit'
--    並原樣存入 → Go 端字串比對失敗、事件 payload 帶著小寫進 Kafka。
--
-- 兩者的共同點是**沒有任何錯誤訊息可以指認真因**：帳看起來少了一筆、
-- 或某個消費者「偶爾」沒反應，而每一層單看都正常。
--
-- 已於 2026-08-01 對 MySQL 8.4.10 實測驗證兩個方向（見 CHANGELOG）。
-- ⚠️ 這兩條由 internal/wallet/store 的 infra 測試釘住 —— 與其在文件裡宣稱，
-- 不如讓測試在有人「順手拿掉 COLLATE」時直接紅給你看。
--
-- 🔶 為什麼不乾脆把整個 database 的預設定序設成 bin？
-- 因為玩家暱稱、商品名稱這類欄位**應該**是 ci 的（搜尋「Alex」要找得到「alex」）。
-- 定序是 per-column 的正確性選擇，不是全域開關。

-- ── wallet_outbox：Transactional Outbox 待發事件（地雷 #5）────────────
-- 這是一張**普通的 MySQL 表**，不是 Kafka 的元件。它跟帳務異動落在
-- 同一個交易裡，把「跨系統一致性」壓縮回「單一資料庫的交易」。
-- 之後由 poller 撈 PENDING 送 Kafka、標 SENT。
CREATE TABLE IF NOT EXISTS wallet_outbox (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    topic       VARCHAR(100) NOT NULL,                    -- wallet.credit / wallet.debit
    kafka_key   VARCHAR(100) NULL,                        -- 用 playerId，保證同玩家事件同 partition 有序
    payload     TEXT         NOT NULL,                    -- JSON 事件內容
    status      VARCHAR(20)  COLLATE utf8mb4_bin NOT NULL DEFAULT 'PENDING',
    retry_count INT          NOT NULL DEFAULT 0,          -- 投遞失敗累加，供觀測/告警
    created_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    sent_at     DATETIME(6)  NULL,
    CONSTRAINT pk_wallet_outbox         PRIMARY KEY (id),
    CONSTRAINT chk_wallet_outbox_status CHECK (status IN ('PENDING', 'SENT')),
    -- poller 的撈取條件就是 (status, created_at)，所以索引照這個順序建。
    KEY idx_wallet_outbox_status_created (status, created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- payload 用 TEXT 而不是 MySQL 的 JSON 型別：outbox 從不查詢 payload 內容，
-- 只是原封不動搬進 Kafka。用 JSON 型別會讓 MySQL 做解析與正規化
-- （欄位重排、空白移除），送出去的 bytes 就不等於寫進去的 bytes，
-- 契約測試比對 payload 時會出現「看起來一樣但不相等」的假失敗。

-- ⚠️ outbox 是單向成長的表：poller 投遞成功只把 status 標 SENT、**從不刪除**。
-- 每筆下注/派彩/贈禮都寫一列。清理排程要在 Phase A 內補上，規則是
-- **只刪 SENT**（PENDING 無論多舊都不能刪，刪掉就是無聲丟失事件），
-- 保留期對齊消費端去重標記的 TTL（7 天）。

-- +goose Down

-- ⚠️ 這會**刪掉全部帳務資料**。它存在是因為 down 是 migration 工具的契約
-- （少了它，「這個版本能不能退回去」只能靠猜），不是因為它該被執行。
-- 正式環境的回退手段是備份還原，不是 `goose down`。
-- 順序與建表相反：outbox → transactions → wallets。
DROP TABLE IF EXISTS wallet_outbox;
DROP TABLE IF EXISTS wallet_transactions;
DROP TABLE IF EXISTS wallets;
