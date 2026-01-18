package repo

import (
	"context"
	"walletmodeling/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	ulid "github.com/oklog/ulid/v2"
	"github.com/shopspring/decimal"
)

type UTXOWalletRepo struct {
	db *pgxpool.Pool
}

func NewUTXOWalletRepo(db *pgxpool.Pool) *UTXOWalletRepo {
	return &UTXOWalletRepo{db: db}
}

func (s *UTXOWalletRepo) GetWalletBalance(ctx context.Context, walletID int) (*model.BalanceWallet, error) {
	query := `
		WITH locked AS (
			SELECT *
			FROM utxo_wallet
			WHERE wallet_id = $1
			AND spent_at IS NULL
			FOR UPDATE
		)
		SELECT
			wallet_id,
			SUM(amount)     AS total_amount,
			MAX(created_at) AS last_created_at,
			MAX(created_at) AS last_updated_at
		FROM locked
		GROUP BY wallet_id;
	`
	row := s.db.QueryRow(ctx, query, walletID)

	var balance model.BalanceWallet
	if err := row.Scan(&balance.WalletID, &balance.Amount, &balance.CreatedAt, &balance.UpdatedAt); err != nil {
		return nil, err
	}

	return &balance, nil
}

func (s *UTXOWalletRepo) UpdateWalletBalance(ctx context.Context, walletID int, delta decimal.Decimal) error {
	if delta.IsNegative() {
		utxos, total, err := s.FindUnspentGTETarget(ctx, walletID, delta.Abs())
		if err != nil {
			return err
		}

		txIDs := make([]interface{}, len(utxos))
		for i, utxo := range utxos {
			txIDs[i] = utxo.TxID
		}
		query := `
				UPDATE utxo_wallet
				SET spent_at = now()
				WHERE tx_id = ANY($1)
			`
		_, err = s.db.Exec(ctx, query, txIDs)
		if err != nil {
			return err
		}

		delta = total.Add(delta)
	}

	if !delta.IsZero() {
		_, err := s.db.Exec(ctx, `
		INSERT INTO utxo_wallet (tx_id, wallet_id, amount, created_at)
		VALUES ($1, $2, $3, now())
	`, uuid.UUID(ulid.Make()), walletID, delta)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *UTXOWalletRepo) FindUnspentGTETarget(ctx context.Context, walletID int, target decimal.Decimal) ([]model.UTXOWallet, decimal.Decimal, error) {
	query := `
		WITH RECURSIVE candidate AS (
			SELECT u.tx_id, u.wallet_id, u.amount, u.created_at, u.spent_at,
				u.amount::NUMERIC(20,12) AS acc
			FROM (
				SELECT tx_id, wallet_id, amount, created_at, spent_at
				FROM utxo_wallet
				WHERE wallet_id = $1
				AND spent_at IS NULL
				ORDER BY tx_id
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			) AS u

			UNION ALL

			SELECT u.tx_id, u.wallet_id, u.amount, u.created_at, u.spent_at,
				(c.acc + u.amount)::NUMERIC(20,12) AS acc
			FROM candidate AS c
			JOIN LATERAL (
				SELECT tx_id, wallet_id, amount, created_at, spent_at
				FROM utxo_wallet
				WHERE wallet_id = c.wallet_id
				AND spent_at IS NULL
				AND tx_id > c.tx_id
				ORDER BY tx_id
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			) AS u ON true
			WHERE c.acc < $2::NUMERIC(20,12)
		)
		SELECT tx_id, wallet_id, amount, created_at, spent_at
		FROM candidate
	`

	rows, err := s.db.Query(ctx, query, walletID, target)
	if err != nil {
		return []model.UTXOWallet{}, decimal.Zero, err
	}
	defer rows.Close()

	var utxos []model.UTXOWallet
	total := decimal.Zero
	for rows.Next() {
		var utxo model.UTXOWallet
		if err := rows.Scan(&utxo.TxID, &utxo.WalletID, &utxo.Amount, &utxo.CreatedAt, &utxo.SpentAt); err != nil {
			return []model.UTXOWallet{}, decimal.Zero, err
		}
		total = total.Add(utxo.Amount)
		utxos = append(utxos, utxo)
	}
	if err := rows.Err(); err != nil {
		return []model.UTXOWallet{}, decimal.Zero, err
	}

	if !total.GreaterThanOrEqual(target) {
		return []model.UTXOWallet{}, decimal.Zero, ErrInsufficientFunds
	}

	return utxos, total, nil
}
