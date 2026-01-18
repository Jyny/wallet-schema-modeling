package repo

import (
	"context"
	"fmt"
	"walletmodeling/model"
	"walletmodeling/repo/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type AccWalletRepo struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewAccWalletRepo(pool *pgxpool.Pool) *AccWalletRepo {
	return &AccWalletRepo{
		db:      pool,
		queries: db.New(pool),
	}
}

// GetWalletBalance retrieves the balance of a wallet by its ID.
func (s *AccWalletRepo) GetWalletBalance(ctx context.Context, walletID int) (*model.BalanceWallet, error) {
	wallet, err := s.queries.GetAccWalletForUpdate(ctx, int32(walletID))
	if err != nil {
		return nil, fmt.Errorf("failed to get wallet balance: %w", err)
	}

	amount, err := numericToDecimal(wallet.Amount)
	if err != nil {
		return nil, fmt.Errorf("failed to convert amount: %w", err)
	}

	return &model.BalanceWallet{
		WalletID:  int(wallet.WalletID),
		Amount:    amount,
		CreatedAt: wallet.CreatedAt.Time,
		UpdatedAt: wallet.UpdatedAt.Time,
	}, nil
}

// UpdateWalletBalance updates the balance of a wallet by applying a delta.
func (s *AccWalletRepo) UpdateWalletBalance(ctx context.Context, walletID int, delta decimal.Decimal) (*model.BalanceWallet, error) {
	wallet, err := s.queries.UpdateAccWalletBalance(ctx, db.UpdateAccWalletBalanceParams{
		WalletID: int32(walletID),
		Amount:   decimalToNumeric(delta),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update wallet balance: %w", err)
	}

	amount, err := numericToDecimal(wallet.Amount)
	if err != nil {
		return nil, fmt.Errorf("failed to convert amount: %w", err)
	}

	return &model.BalanceWallet{
		WalletID:  int(wallet.WalletID),
		Amount:    amount,
		CreatedAt: wallet.CreatedAt.Time,
		UpdatedAt: wallet.UpdatedAt.Time,
	}, nil
}
