package repo

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
	"walletmodeling/model"
	"walletmodeling/repo/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// UTXOWalletRepo models a balance as the sum of unspent outputs. Outputs are
// never changed, only spent: every operation is a utxo_tx that spends some
// outputs and creates new ones. A hold is an output locked by its hold_id.
type UTXOWalletRepo struct {
	pool    *pgxpool.Pool
	queries *db.Queries
}

var _ WalletRepo = (*UTXOWalletRepo)(nil)

// retries of a spend whose funds exist but are locked by other transactions:
// jittered exponential backoff, up to about a second in all
const (
	maxSpendAttempts = 50
	minSpendBackoff  = time.Millisecond
	maxSpendBackoff  = 32 * time.Millisecond
)

func NewUTXOWalletRepo(pool *pgxpool.Pool) *UTXOWalletRepo {
	return &UTXOWalletRepo{
		pool:    pool,
		queries: db.New(pool),
	}
}

func (r *UTXOWalletRepo) GetBalance(ctx context.Context, walletID int64, cur model.Currency) (model.Balance, error) {
	if err := checkCurrencyCode(cur); err != nil {
		return model.Balance{}, fmt.Errorf("get balance: %w", err)
	}
	bal, err := getUTXOBalance(ctx, r.queries, walletID, cur)
	if err != nil {
		return model.Balance{}, fmt.Errorf("get balance: %w", err)
	}
	return bal, nil
}

func (r *UTXOWalletRepo) ListBalances(ctx context.Context, walletID int64) ([]model.Balance, error) {
	rows, err := r.queries.UTXOListBalances(ctx, walletID)
	if err != nil {
		return nil, fmt.Errorf("list balances: %w", err)
	}
	balances := make([]model.Balance, len(rows))
	for i, row := range rows {
		balances[i] = toBalance(walletID, balanceRow(row))
	}
	return balances, nil
}

// Credit creates one output of amount.
func (r *UTXOWalletRepo) Credit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error) {
	var bal model.Balance
	err := r.inTx(ctx, func(q *db.Queries) error {
		if err := validateAmount(ctx, q, cur, amount); err != nil {
			return err
		}
		txID, err := insertTx(ctx, q, walletID, cur, kindCredit, amount, nil)
		if err != nil {
			return err
		}
		if err := insertOutput(ctx, q, txID, walletID, cur, amount, nil); err != nil {
			return err
		}
		if bal, err = getUTXOBalance(ctx, q, walletID, cur); err != nil {
			return err
		}
		return checkBound(bal)
	})
	if err != nil {
		return model.Balance{}, fmt.Errorf("credit: %w", err)
	}
	return bal, nil
}

// Debit spends available outputs covering amount and creates an output for
// the change.
func (r *UTXOWalletRepo) Debit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error) {
	var bal model.Balance
	err := r.spendInTx(ctx, func(q *db.Queries) error {
		if err := validateAmount(ctx, q, cur, amount); err != nil {
			return err
		}
		txID, err := insertTx(ctx, q, walletID, cur, kindDebit, amount, nil)
		if err != nil {
			return err
		}
		if err := spendAvailable(ctx, q, txID, walletID, cur, amount); err != nil {
			return err
		}
		bal, err = getUTXOBalance(ctx, q, walletID, cur)
		return err
	})
	if err != nil {
		return model.Balance{}, fmt.Errorf("debit: %w", err)
	}
	return bal, nil
}

// Hold spends available outputs covering amount, creates one output of amount
// locked by the hold, and an output for the change.
func (r *UTXOWalletRepo) Hold(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Hold, error) {
	hold := model.Hold{WalletID: walletID, Currency: cur, Amount: amount}
	err := r.spendInTx(ctx, func(q *db.Queries) error {
		if err := validateAmount(ctx, q, cur, amount); err != nil {
			return err
		}
		var err error
		if hold.ID, err = uuid.NewV7(); err != nil {
			return err
		}
		txID, err := insertTx(ctx, q, walletID, cur, kindHold, amount, &hold.ID)
		if err != nil {
			return err
		}
		if err := spendAvailable(ctx, q, txID, walletID, cur, amount); err != nil {
			return err
		}
		if err := insertOutput(ctx, q, txID, walletID, cur, amount, &hold.ID); err != nil {
			return err
		}
		bal, err := getUTXOBalance(ctx, q, walletID, cur)
		if err != nil {
			return err
		}
		return checkBound(bal)
	})
	if err != nil {
		return model.Hold{}, fmt.Errorf("hold: %w", err)
	}
	return hold, nil
}

// ReleaseHold spends the held output and recreates it as available.
func (r *UTXOWalletRepo) ReleaseHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error) {
	bal, err := r.closeHold(ctx, holdID, kindRelease)
	if err != nil {
		return model.Balance{}, fmt.Errorf("release hold: %w", err)
	}
	return bal, nil
}

// SettleHold spends the held output without creating a new one.
func (r *UTXOWalletRepo) SettleHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error) {
	bal, err := r.closeHold(ctx, holdID, kindSettle)
	if err != nil {
		return model.Balance{}, fmt.Errorf("settle hold: %w", err)
	}
	return bal, nil
}

func (r *UTXOWalletRepo) closeHold(ctx context.Context, holdID uuid.UUID, kind string) (model.Balance, error) {
	var bal model.Balance
	err := r.inTx(ctx, func(q *db.Queries) error {
		// locks the output: a concurrent close of the same hold waits here,
		// then finds it spent
		held, err := q.UTXOGetHeldForUpdate(ctx, uuid.NullUUID{UUID: holdID, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrHoldNotFound, holdID)
		}
		if err != nil {
			return err
		}
		cur := model.Currency(held.Currency)
		txID, err := insertTx(ctx, q, held.WalletID, cur, kind, held.Amount, &holdID)
		if err != nil {
			return err
		}
		if err := spend(ctx, q, txID, []uuid.UUID{held.UtxoID}); err != nil {
			return err
		}
		if kind == kindRelease {
			if err := insertOutput(ctx, q, txID, held.WalletID, cur, held.Amount, nil); err != nil {
				return err
			}
		}
		if bal, err = getUTXOBalance(ctx, q, held.WalletID, cur); err != nil {
			return err
		}
		return checkBound(bal)
	})
	return bal, err
}

func (r *UTXOWalletRepo) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return translate(pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(r.queries.WithTx(tx))
	}))
}

// spendInTx runs an operation that selects outputs to spend. selection skips
// outputs locked by other transactions; when that leaves it short while the
// funds exist, it backs off and retries in a fresh transaction, whose snapshot
// sees what the competitors committed meanwhile, such as their change. it
// never waits on a lock, so an unrelated long transaction holding one output
// cannot stall spends the others can serve.
func (r *UTXOWalletRepo) spendInTx(ctx context.Context, fn func(q *db.Queries) error) error {
	for attempt := 0; ; attempt++ {
		err := r.inTx(ctx, fn)
		if !errors.Is(err, ErrContended) || attempt+1 == maxSpendAttempts {
			return err
		}
		ceiling := min(maxSpendBackoff, minSpendBackoff<<min(attempt, 10))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rand.N(ceiling) + 1):
		}
	}
}

func getUTXOBalance(ctx context.Context, q *db.Queries, walletID int64, cur model.Currency) (model.Balance, error) {
	row, err := q.UTXOGetBalance(ctx, db.UTXOGetBalanceParams{WalletID: walletID, Currency: string(cur)})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Balance{}, fmt.Errorf("%w: %s", ErrUnknownCurrency, cur)
	}
	if err != nil {
		return model.Balance{}, err
	}
	return toBalance(walletID, balanceRow(row)), nil
}

// spendAvailable selects and spends available outputs covering amount, then
// returns the excess to the wallet as a change output.
func spendAvailable(ctx context.Context, q *db.Queries, txID uuid.UUID, walletID int64, cur model.Currency, amount decimal.Decimal) error {
	inputs, err := q.UTXOSelectInputs(ctx, db.UTXOSelectInputsParams{WalletID: walletID, Currency: string(cur), Target: amount})
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(inputs))
	total := decimal.Zero
	for i, in := range inputs {
		ids[i] = in.UtxoID
		total = total.Add(in.Amount)
	}
	if total.LessThan(amount) {
		// the selection only sees outputs committed before it started and
		// skipped those locked by other transactions. a fresh look tells
		// whether the funds are there at all
		available, err := q.UTXOAvailable(ctx, db.UTXOAvailableParams{WalletID: walletID, Currency: string(cur)})
		if err != nil {
			return err
		}
		if available.LessThan(amount) {
			return ErrInsufficientFunds
		}
		return ErrContended
	}
	if err := spend(ctx, q, txID, ids); err != nil {
		return err
	}
	if change := total.Sub(amount); change.IsPositive() {
		return insertOutput(ctx, q, txID, walletID, cur, change, nil)
	}
	return nil
}

func spend(ctx context.Context, q *db.Queries, txID uuid.UUID, ids []uuid.UUID) error {
	n, err := q.UTXOSpend(ctx, db.UTXOSpendParams{SpentTx: uuid.NullUUID{UUID: txID, Valid: true}, UtxoIds: ids})
	if err != nil {
		return err
	}
	// inputs are locked by this transaction, so all of them must still be unspent
	if n != int64(len(ids)) {
		return fmt.Errorf("spent %d of %d outputs", n, len(ids))
	}
	return nil
}

func insertTx(ctx context.Context, q *db.Queries, walletID int64, cur model.Currency, kind string, amount decimal.Decimal, holdID *uuid.UUID) (uuid.UUID, error) {
	txID, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	return txID, q.UTXOInsertTx(ctx, db.UTXOInsertTxParams{
		TxID:     txID,
		WalletID: walletID,
		Currency: string(cur),
		Kind:     kind,
		Amount:   amount,
		HoldID:   nullUUID(holdID),
	})
}

func insertOutput(ctx context.Context, q *db.Queries, txID uuid.UUID, walletID int64, cur model.Currency, amount decimal.Decimal, holdID *uuid.UUID) error {
	return q.UTXOInsertOutput(ctx, db.UTXOInsertOutputParams{
		WalletID:  walletID,
		Currency:  string(cur),
		Amount:    amount,
		HoldID:    nullUUID(holdID),
		CreatedTx: txID,
	})
}

func nullUUID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}
