-- ============================================================================
-- 00002 — diamond_wallets：玩家鑽石錢包
--
-- 對齊 Java 的 DiamondWalletService / DiamondWallet entity。與 wallets 平行、
-- 由同一個 member.registered 事件建立（internal/wallet/memberconsume）。
--
-- ⚠️ NO TRANSACTION 與 00001 同理（AGENTS.md 地雷 #31）：MySQL 的 DDL 會隱式
-- commit，這裡明寫是為了讓「沒有原子性」變成看得到的事實。
--
-- ⚠️ 這裡刻意**不加**那個只有 00001 才准用的存在性防護子句——00001 的檔頭
-- 已經說明那是 baseline 專屬的例外（要在 initdb.d 時代就建好表的既有 volume
-- 上安全跑過一次）。之後的 migration 加了它只會讓欄位定義漂移被靜靜跳過，
-- 看不出來（由 TestMigrationFilesAreWellFormed 釘住）。
-- ============================================================================

-- +goose NO TRANSACTION
-- +goose Up

-- ── diamond_wallets：玩家鑽石錢包（member.registered 建立，與 wallets 平行）──
-- ⚠️ 刻意沒有 frozen_amount 欄位：對齊 Java 的 DiamondWalletService.createDiamondWallet
-- （只有 playerId / balance / version），鑽石沒有凍結金額這個概念。
CREATE TABLE diamond_wallets (
    player_id  BIGINT      NOT NULL,
    balance    BIGINT      NOT NULL DEFAULT 0,   -- 鑽石餘額，整數、無小數
    version    BIGINT      NOT NULL DEFAULT 0,   -- 樂觀鎖版本號（目前建立時寫死 0，尚無鑽石異動路徑）
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CONSTRAINT pk_diamond_wallets          PRIMARY KEY (player_id),
    CONSTRAINT chk_diamond_wallets_balance CHECK (balance >= 0)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- 沒有字串欄位參與相等性判定或列舉約束（不像 wallet_transactions 的
-- type/sub_type/idempotency_key），所以不需要 COLLATE utf8mb4_bin（地雷 #30）。

-- +goose Down

-- ⚠️ 這會刪掉全部鑽石錢包資料，理由與 00001 的 Down 相同：down 是 migration
-- 工具的契約，不是建議的正式環境回退手段。
DROP TABLE IF EXISTS diamond_wallets;
