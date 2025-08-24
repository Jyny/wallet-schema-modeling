package repo

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUTXOWalletGetWalletBalance(t *testing.T) {
	db := setupTestDB(t)
	repo := NewUTXOWalletRepo(db)

	wallet, err := repo.GetWalletBalance(context.Background(), 1)
	require.NoError(t, err)
	expectedBalance := decimal.RequireFromString("3847283.345238472800")
	assert.True(t, wallet.Amount.Equal(expectedBalance))

	_, err = repo.GetWalletBalance(context.Background(), 999)
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestFindUnspentGTETarget(t *testing.T) {
	db := setupTestDB(t)
	repo := NewUTXOWalletRepo(db)

	walletID := 1
	target := decimal.RequireFromString("3847283.345238472800")
	utxos, total, err := repo.FindUnspentGTETarget(context.Background(), walletID, target)
	require.NoError(t, err)
	assert.NotEmpty(t, utxos)
	assert.True(t, total.GreaterThanOrEqual(target))

	walletID = 1
	target = decimal.RequireFromString("4000000.00")
	utxos, _, err = repo.FindUnspentGTETarget(context.Background(), walletID, target)
	assert.ErrorIs(t, err, ErrInsufficientFunds)
	assert.Empty(t, utxos)

	walletID = 999
	target = decimal.RequireFromString("0.0")
	utxos, _, err = repo.FindUnspentGTETarget(context.Background(), walletID, target)
	assert.NoError(t, err)
	assert.Empty(t, utxos)
}

func TestUpdateWalletBalance(t *testing.T) {
	db := setupTestDB(t)
	repo := NewUTXOWalletRepo(db)

	walletID := 1
	delta := decimal.RequireFromString("1000.123456789")
	err := repo.UpdateWalletBalance(context.Background(), walletID, delta)
	require.NoError(t, err)

	wallet, err := repo.GetWalletBalance(context.Background(), walletID)
	require.NoError(t, err)

	expectedBalance := decimal.RequireFromString("3848283.468695261800") // 3847283.345238472800 + 1000.123456789
	assert.True(t, wallet.Amount.Equal(expectedBalance))

	delta = decimal.RequireFromString("-1000.123456789")
	err = repo.UpdateWalletBalance(context.Background(), walletID, delta)
	require.NoError(t, err)

	wallet, err = repo.GetWalletBalance(context.Background(), walletID)
	require.NoError(t, err)
	expectedBalance = decimal.RequireFromString("3847283.345238472800") // back to original
	assert.True(t, wallet.Amount.Equal(expectedBalance))

	delta = decimal.RequireFromString("-5000000.00")
	err = repo.UpdateWalletBalance(context.Background(), walletID, delta)
	assert.ErrorIs(t, err, ErrInsufficientFunds)

	delta = decimal.RequireFromString("-1.0")
	err = repo.UpdateWalletBalance(context.Background(), 999, delta)
	assert.ErrorIs(t, err, ErrInsufficientFunds)
}
