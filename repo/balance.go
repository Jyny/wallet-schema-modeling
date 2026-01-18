package repo

import (
	"context"
	"fmt"
	"walletmodeling/model"
	"walletmodeling/repo/db"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type BalanceWalletRepo struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewBalanceWalletRepo(pool *pgxpool.Pool) *BalanceWalletRepo {
	return &BalanceWalletRepo{
		db:      pool,
		queries: db.New(pool),
	}
}

// GetWalletBalance retrieves the balance of a wallet by its ID.
func (s *BalanceWalletRepo) GetWalletBalance(ctx context.Context, walletID int) (*model.BalanceWallet, error) {
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
func (s *BalanceWalletRepo) UpdateWalletBalance(ctx context.Context, walletID int, delta decimal.Decimal) (*model.BalanceWallet, error) {
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

func decimalToNumeric(d decimal.Decimal) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(d.String())
	return n
}

func numericToDecimal(n pgtype.Numeric) (decimal.Decimal, error) {
	if !n.Valid {
		return decimal.Zero, nil
	}
	return decimal.NewFromBigInt(n.Int, n.Exp), nil
}
