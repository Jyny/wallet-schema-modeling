package repo

import (
	"accmodeling/model"
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type BalanceWalletRepo struct {
	db *pgxpool.Pool
}

func NewBalanceWalletRepo(db *pgxpool.Pool) *BalanceWalletRepo {
	return &BalanceWalletRepo{db: db}
}

// GetWalletBalance retrieves the balance of a wallet by its ID.
func (s *BalanceWalletRepo) GetWalletBalance(ctx context.Context, walletID int) (*model.BalanceWallet, error) {
	var wallet model.BalanceWallet

	query := `SELECT wallet_id, amount, created_at, updated_at FROM acc_wallet WHERE wallet_id = $1 FOR UPDATE`
	err := s.db.QueryRow(ctx, query, walletID).Scan(
		&wallet.WalletID,
		&wallet.Amount,
		&wallet.CreatedAt,
		&wallet.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet balance: %w", err)
	}

	return &wallet, nil
}

// UpdateWalletBalance updates the balance of a wallet by applying a delta.
func (s *BalanceWalletRepo) UpdateWalletBalance(ctx context.Context, walletID int, delta decimal.Decimal) (*model.BalanceWallet, error) {
	query := `
        UPDATE acc_wallet
        SET amount = amount + $1
        WHERE wallet_id = $2
		AND amount + $1 >= 0
        RETURNING *
    `

	var wallet model.BalanceWallet
	err := s.db.QueryRow(ctx, query, delta, walletID).Scan(
		&wallet.WalletID,
		&wallet.Amount,
		&wallet.CreatedAt,
		&wallet.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to update wallet balance: %w", err)
	}

	return &wallet, nil
}
