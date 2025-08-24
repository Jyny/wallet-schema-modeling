package repo

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBalanceWalletGetWalletBalance(t *testing.T) {
	db := setupTestDB(t)
	repo := NewBalanceWalletRepo(db)

	wallet, err := repo.GetWalletBalance(context.Background(), 1)
	require.NoError(t, err)
	expectedBalance := decimal.RequireFromString("3847283.345238472800")
	assert.True(t, wallet.Amount.Equal(expectedBalance))

	_, err = repo.GetWalletBalance(context.Background(), 999)
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestBalanceWalletUpdateWalletBalance(t *testing.T) {
	db := setupTestDB(t)
	repo := NewBalanceWalletRepo(db)

	delta := decimal.RequireFromString("100.123456789012")
	_, err := repo.UpdateWalletBalance(context.Background(), 1, delta)
	require.NoError(t, err)

	updatedBalance, err := repo.GetWalletBalance(context.Background(), 1)
	require.NoError(t, err)
	expectedBalance := decimal.RequireFromString("3847383.468695261812")
	assert.True(t, updatedBalance.Amount.Equal(expectedBalance))

	_, err = repo.UpdateWalletBalance(context.Background(), 1, decimal.RequireFromString("-100.123456789012"))
	require.NoError(t, err)

	_, err = repo.UpdateWalletBalance(context.Background(), 999, delta)
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}
