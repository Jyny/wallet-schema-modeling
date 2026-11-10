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
// Each write is one statement updating all three in autocommit, so the row
// is locked for that one round trip only.
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

// Credit, Debit and Hold validate the amount against its currency, a read of
// reference data that needs no transaction with the write, then write in one
// statement, atomic on its own: the row is locked for that statement only.

func (r *AccountWalletRepo) Credit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error) {
	if err := validateAmount(ctx, r.queries, cur, amount); err != nil {
		return model.Balance{}, fmt.Errorf("credit: %w", err)
	}
	row, err := r.queries.AccountCredit(ctx, db.AccountCreditParams{WalletID: walletID, Currency: string(cur), Amount: amount})
	if err != nil {
		return model.Balance{}, fmt.Errorf("credit: %w", translate(err))
	}
	return toBalance(walletID, balanceRow(row)), nil
}

func (r *AccountWalletRepo) Debit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error) {
	if err := validateAmount(ctx, r.queries, cur, amount); err != nil {
		return model.Balance{}, fmt.Errorf("debit: %w", err)
	}
	row, err := r.queries.AccountDebit(ctx, db.AccountDebitParams{WalletID: walletID, Currency: string(cur), Amount: amount})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInsufficientFunds
	}
	if err != nil {
		return model.Balance{}, fmt.Errorf("debit: %w", translate(err))
	}
	return toBalance(walletID, balanceRow(row)), nil
}

func (r *AccountWalletRepo) Hold(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Hold, error) {
	if err := validateAmount(ctx, r.queries, cur, amount); err != nil {
		return model.Hold{}, fmt.Errorf("hold: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return model.Hold{}, fmt.Errorf("hold: %w", err)
	}
	_, err = r.queries.AccountHold(ctx, db.AccountHoldParams{WalletID: walletID, Currency: string(cur), Amount: amount, HoldID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInsufficientFunds
	}
	if err != nil {
		return model.Hold{}, fmt.Errorf("hold: %w", translate(err))
	}
	return model.Hold{ID: id, WalletID: walletID, Currency: cur, Amount: amount}, nil
}

func (r *AccountWalletRepo) ReleaseHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error) {
	row, err := r.queries.AccountReleaseHold(ctx, holdID)
	bal, err := closedHold(holdID, row.WalletID, balanceRow{row.Currency, row.Available, row.Held}, err)
	if err != nil {
		return model.Balance{}, fmt.Errorf("release hold: %w", err)
	}
	return bal, nil
}

func (r *AccountWalletRepo) SettleHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error) {
	row, err := r.queries.AccountSettleHold(ctx, holdID)
	bal, err := closedHold(holdID, row.WalletID, balanceRow{row.Currency, row.Available, row.Held}, err)
	if err != nil {
		return model.Balance{}, fmt.Errorf("settle hold: %w", err)
	}
	return bal, nil
}

// closedHold maps the result of a release or settle. it needs no
// transaction of its own: the statement closes the hold, updates the account
// and writes the entry at once, and finds no rows when the hold is unknown or
// closed, also when a concurrent close of it committed first
func closedHold(holdID uuid.UUID, walletID int64, row balanceRow, err error) (model.Balance, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Balance{}, fmt.Errorf("%w: %s", ErrHoldNotFound, holdID)
	}
	if err != nil {
		return model.Balance{}, translate(err)
	}
	return toBalance(walletID, row), nil
}
