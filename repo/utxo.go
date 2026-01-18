package repo

import (
	"context"
	"time"
	"walletmodeling/model"
	"walletmodeling/repo/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	ulid "github.com/oklog/ulid/v2"
	"github.com/shopspring/decimal"
)

type UTXOWalletRepo struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

func NewUTXOWalletRepo(pool *pgxpool.Pool) *UTXOWalletRepo {
	return &UTXOWalletRepo{
		db:      pool,
		queries: db.New(pool),
	}
}

func (s *UTXOWalletRepo) GetWalletBalance(ctx context.Context, walletID int) (*model.BalanceWallet, error) {
	row, err := s.queries.GetUTXOWalletBalanceForUpdate(ctx, int32(walletID))
	if err != nil {
		return nil, err
	}

	amount, err := numericToDecimal(row.TotalAmount)
	if err != nil {
		return nil, err
	}

	return &model.BalanceWallet{
		WalletID: int(row.WalletID),
		Amount:   amount,
	}, nil
}

func (s *UTXOWalletRepo) UpdateWalletBalance(ctx context.Context, walletID int, delta decimal.Decimal) error {
	if delta.IsNegative() {
		utxos, total, err := s.FindUnspentGTETarget(ctx, walletID, delta.Abs())
		if err != nil {
			return err
		}

		txIDs := make([]uuid.UUID, len(utxos))
		for i, utxo := range utxos {
			txIDs[i] = utxo.TxID
		}

		if err := s.queries.MarkUTXOsAsSpent(ctx, txIDs); err != nil {
			return err
		}

		delta = total.Add(delta)
	}

	if !delta.IsZero() {
		err := s.queries.InsertUTXO(ctx, db.InsertUTXOParams{
			TxID:     uuid.UUID(ulid.Make()),
			WalletID: int32(walletID),
			Amount:   decimalToNumeric(delta),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *UTXOWalletRepo) FindUnspentGTETarget(ctx context.Context, walletID int, target decimal.Decimal) ([]model.UTXOWallet, decimal.Decimal, error) {
	rows, err := s.queries.FindUnspentUTXOsGTETarget(ctx, db.FindUnspentUTXOsGTETargetParams{
		WalletID: int32(walletID),
		Target:   decimalToNumeric(target),
	})
	if err != nil {
		return []model.UTXOWallet{}, decimal.Zero, err
	}

	var utxos []model.UTXOWallet
	total := decimal.Zero
	for _, row := range rows {
		amount, err := numericToDecimal(row.Amount)
		if err != nil {
			return []model.UTXOWallet{}, decimal.Zero, err
		}

		var spentAt *time.Time
		if row.SpentAt.Valid {
			spentAt = &row.SpentAt.Time
		}

		utxo := model.UTXOWallet{
			TxID:      row.TxID,
			WalletID:  int(row.WalletID),
			Amount:    amount,
			CreatedAt: row.CreatedAt.Time,
			SpentAt:   spentAt,
		}
		total = total.Add(amount)
		utxos = append(utxos, utxo)
	}

	if !total.GreaterThanOrEqual(target) {
		return []model.UTXOWallet{}, decimal.Zero, ErrInsufficientFunds
	}

	return utxos, total, nil
}
