-- ============================================================================
-- 00003 — gift_transfers：贈禮的狀態單（兩步冪等 + 補償，docs/ADR-005）
--
-- 贈禮是「送出方 debit」與「接收方 credit」兩筆各自獨立的交易。兩步之間行程可能
-- 死掉，所以**先寫這張單、再動錢**：任何時刻只要看這一列的 status，就知道下一步
-- 該做什麼（扣款、入帳、退款），而每一步都帶固定的冪等鍵，重做不會重複記帳。
--
-- ⚠️ 這張表是本專案自己的設計，Java 版沒有對應物：Java 把雙向分錄放在單一
-- PostgreSQL 交易裡（GiftTransferService）。分歧的理由與代價寫在 docs/ADR-005。
--
-- ⚠️ NO TRANSACTION 與 00001 / 00002 同理（AGENTS.md 地雷 #31）。
-- ============================================================================

-- +goose NO TRANSACTION
-- +goose Up

CREATE TABLE gift_transfers (
    id              BIGINT      NOT NULL AUTO_INCREMENT,
    -- 呼叫端帶來的冪等鍵（上限 80，對齊 Java GiftRequest 的 @Size(max = 80)）。
    -- 三筆帳務分錄的鍵都由它衍生：`{key}:gift:debit` / `:gift:credit` / `:gift:refund`，
    -- 80 + 12 剛好不超過 wallet_transactions.idempotency_key 的 VARCHAR(100)。
    -- ⚠️ utf8mb4_bin：它參與 UNIQUE 相等性判定（地雷 #30）。
    idempotency_key VARCHAR(80) COLLATE utf8mb4_bin NOT NULL,
    sender_id       BIGINT      NOT NULL,
    receiver_id     BIGINT      NOT NULL,
    amount          BIGINT      NOT NULL,   -- 星幣，整數最小單位
    -- PENDING   已立單，尚未確認扣款
    -- DEBITED   送出方已扣款，等待入帳（失敗時依 next_attempt_at 重試）
    -- COMPLETED 接收方已入帳（終態）
    -- REFUNDING 入帳放棄，正在把錢退回送出方（重試到成功為止）
    -- REFUNDED  已退回送出方（終態）
    -- ⚠️ utf8mb4_bin：它參與 CHECK ... IN 列舉約束（地雷 #30）。
    status          VARCHAR(20) COLLATE utf8mb4_bin NOT NULL,
    -- 目前這個階段已經失敗幾次；換階段時歸零。
    attempts        INT         NOT NULL DEFAULT 0,
    -- 背景重試最早可以再碰這一列的時間（UTC）。終態為 NULL。
    next_attempt_at DATETIME(6) NULL,
    last_error      VARCHAR(255) NULL,
    -- 以下四個欄位在對應的步驟完成時才有值，用來重建冪等重送的回應。
    debit_transaction_id   BIGINT NULL,
    credit_transaction_id  BIGINT NULL,
    sender_balance_after   BIGINT NULL,
    receiver_balance_after BIGINT NULL,
    created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    CONSTRAINT pk_gift_transfers                 PRIMARY KEY (id),
    CONSTRAINT uk_gift_transfers_idempotency_key UNIQUE (idempotency_key),
    CONSTRAINT chk_gift_transfers_amount         CHECK (amount > 0),
    CONSTRAINT chk_gift_transfers_parties        CHECK (sender_id <> receiver_id),
    CONSTRAINT chk_gift_transfers_status         CHECK (status IN (
        'PENDING', 'DEBITED', 'COMPLETED', 'REFUNDING', 'REFUNDED'
    )),
    -- 背景重試的撈取條件：status IN (未完成的三種) AND next_attempt_at <= now。
    KEY idx_gift_transfers_status_next (status, next_attempt_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- +goose Down

-- ⚠️ 這會刪掉全部贈禮狀態單，包含還沒入帳或還沒退款的那些——那些錢會卡在
-- 「送出方已扣、接收方未入」。down 是 migration 工具的契約，不是正式環境的回退手段。
DROP TABLE IF EXISTS gift_transfers;
