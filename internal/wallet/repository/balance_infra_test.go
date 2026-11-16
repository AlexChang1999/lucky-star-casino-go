//go:build infra

package repository

import (
	"errors"
	"testing"
)

// TestBalanceReturnsTotalNotAvailable 釘住破產救助用的是**總餘額**：
// 凍結的錢不可以被扣掉。否則玩家把錢凍結在未結算的下注上就能壓低餘額騙補助。
func TestBalanceReturnsTotalNotAvailable(t *testing.T) {
	env := newTestEnv(t)
	const player = 201
	if err := env.repo.CreateWallet(env.ctx, player); err != nil {
		t.Fatal(err)
	}
	if err := env.db.WithContext(env.ctx).
		Exec("UPDATE wallets SET balance = 500, frozen_amount = 450 WHERE player_id = ?", player).Error; err != nil {
		t.Fatal(err)
	}

	got, err := env.repo.Balance(env.ctx, player)
	if err != nil {
		t.Fatal(err)
	}
	if got != 500 {
		t.Errorf("Balance = %d, want 500（總餘額，不是可用的 50）", got)
	}
}

func TestBalanceWalletNotFound(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.repo.Balance(env.ctx, 999_999); !errors.Is(err, ErrWalletNotFound) {
		t.Errorf("err = %v, want ErrWalletNotFound", err)
	}
}
