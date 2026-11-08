package repo

import (
	"context"
	"errors"
	"fmt"
	"walletmodeling/model"
	"walletmodeling/repo/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// AccountWalletRepo models a balance as one account row per (wallet, currency)
// holding available and held. Every operation is a single guarded UPDATE of
// that row, which also serializes concurrent operations on it. account_hold
// tracks each hold, account_entry keeps the history the row itself overwrites.
type AccountWalletRepo struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

var _ WalletRepo = (*AccountWalletRepo)(nil)

func NewAccountWalletRepo(pool *pgxpool.Pool) *AccountWalletRepo {
	return &AccountWalletRepo{
		pool:    pool,
		queries: db.New(pool),
	}
}

func (r *AccountWalletRepo) GetBalance(ctx context.Context, walletID int64, cur model.Currency) (model.Balance, error) {
	if err := checkCurrencyCode(cur); err != nil {
		return model.Balance{}, fmt.Errorf("get balance: %w", err)
	}
	row, err := r.queries.AccountGetBalance(ctx, db.AccountGetBalanceParams{WalletID: walletID, Currency: string(cur)})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Balance{}, fmt.Errorf("%w: %s", ErrUnknownCurrency, cur)
	}
	if err != nil {
		return model.Balance{}, fmt.Errorf("get balance: %w", err)
	}
	return toBalance(walletID, balanceRow(row)), nil
}

func (r *AccountWalletRepo) ListBalances(ctx context.Context, walletID int64) ([]model.Balance, error) {
	rows, err := r.queries.AccountListBalances(ctx, walletID)
	if err != nil {
		return nil, fmt.Errorf("list balances: %w", err)
	}
	balances := make([]model.Balance, len(rows))
	for i, row := range rows {
		balances[i] = toBalance(walletID, balanceRow(row))
	}
	return balances, nil
}

func (r *AccountWalletRepo) Credit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error) {
	var bal model.Balance
	err := r.inTx(ctx, func(q *db.Queries) error {
		if err := validateAmount(ctx, q, cur, amount); err != nil {
			return err
		}
		row, err := q.AccountCredit(ctx, db.AccountCreditParams{WalletID: walletID, Currency: string(cur), Amount: amount})
		if err != nil {
			return err
		}
		bal = toBalance(walletID, balanceRow(row))
		return insertEntry(ctx, q, walletID, cur, kindCredit, amount, decimal.Zero, nil)
	})
	if err != nil {
		return model.Balance{}, fmt.Errorf("credit: %w", err)
	}
	return bal, nil
}

func (r *AccountWalletRepo) Debit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error) {
	var bal model.Balance
	err := r.inTx(ctx, func(q *db.Queries) error {
		if err := validateAmount(ctx, q, cur, amount); err != nil {
			return err
		}
		row, err := q.AccountDebit(ctx, db.AccountDebitParams{WalletID: walletID, Currency: string(cur), Amount: amount})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInsufficientFunds
		}
		if err != nil {
			return err
		}
		bal = toBalance(walletID, balanceRow(row))
		return insertEntry(ctx, q, walletID, cur, kindDebit, amount.Neg(), decimal.Zero, nil)
	})
	if err != nil {
		return model.Balance{}, fmt.Errorf("debit: %w", err)
	}
	return bal, nil
}

func (r *AccountWalletRepo) Hold(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Hold, error) {
	hold := model.Hold{WalletID: walletID, Currency: cur, Amount: amount}
	err := r.inTx(ctx, func(q *db.Queries) error {
		if err := validateAmount(ctx, q, cur, amount); err != nil {
			return err
		}
		_, err := q.AccountHold(ctx, db.AccountHoldParams{WalletID: walletID, Currency: string(cur), Amount: amount})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInsufficientFunds
		}
		if err != nil {
			return err
		}
		if hold.ID, err = uuid.NewV7(); err != nil {
			return err
		}
		if err := q.AccountInsertHold(ctx, db.AccountInsertHoldParams{
			HoldID:   hold.ID,
			WalletID: walletID,
			Currency: string(cur),
			Amount:   amount,
		}); err != nil {
			return err
		}
		return insertEntry(ctx, q, walletID, cur, kindHold, amount.Neg(), amount, &hold.ID)
	})
	if err != nil {
		return model.Hold{}, fmt.Errorf("hold: %w", err)
	}
	return hold, nil
}

func (r *AccountWalletRepo) ReleaseHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error) {
	var bal model.Balance
	err := r.inTx(ctx, func(q *db.Queries) error {
		hold, err := closeAccountHold(ctx, q, holdID, "released")
		if err != nil {
			return err
		}
		row, err := q.AccountRelease(ctx, db.AccountReleaseParams{WalletID: hold.WalletID, Currency: hold.Currency, Amount: hold.Amount})
		if err != nil {
			return err
		}
		bal = toBalance(hold.WalletID, balanceRow(row))
		return insertEntry(ctx, q, hold.WalletID, model.Currency(hold.Currency), kindRelease, hold.Amount, hold.Amount.Neg(), &holdID)
	})
	if err != nil {
		return model.Balance{}, fmt.Errorf("release hold: %w", err)
	}
	return bal, nil
}

func (r *AccountWalletRepo) SettleHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error) {
	var bal model.Balance
	err := r.inTx(ctx, func(q *db.Queries) error {
		hold, err := closeAccountHold(ctx, q, holdID, "settled")
		if err != nil {
			return err
		}
		row, err := q.AccountSettle(ctx, db.AccountSettleParams{WalletID: hold.WalletID, Currency: hold.Currency, Amount: hold.Amount})
		if err != nil {
			return err
		}
		bal = toBalance(hold.WalletID, balanceRow(row))
		return insertEntry(ctx, q, hold.WalletID, model.Currency(hold.Currency), kindSettle, decimal.Zero, hold.Amount.Neg(), &holdID)
	})
	if err != nil {
		return model.Balance{}, fmt.Errorf("settle hold: %w", err)
	}
	return bal, nil
}

func (r *AccountWalletRepo) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return translate(pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(r.queries.WithTx(tx))
	}))
}

// closeAccountHold marks an active hold released or settled. the conditional UPDATE
// makes a concurrent second close of the same hold find nothing.
func closeAccountHold(ctx context.Context, q *db.Queries, holdID uuid.UUID, status string) (db.AccountCloseHoldRow, error) {
	hold, err := q.AccountCloseHold(ctx, db.AccountCloseHoldParams{HoldID: holdID, Status: status})
	if errors.Is(err, pgx.ErrNoRows) {
		return hold, fmt.Errorf("%w: %s", ErrHoldNotFound, holdID)
	}
	return hold, err
}

func insertEntry(ctx context.Context, q *db.Queries, walletID int64, cur model.Currency, kind string, availableDelta, heldDelta decimal.Decimal, holdID *uuid.UUID) error {
	return q.AccountInsertEntry(ctx, db.AccountInsertEntryParams{
		WalletID:       walletID,
		Currency:       string(cur),
		Kind:           kind,
		AvailableDelta: availableDelta,
		HeldDelta:      heldDelta,
		HoldID:         nullUUID(holdID),
	})
}
