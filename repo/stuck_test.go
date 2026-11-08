package repo

import (
	"context"
	"sync"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scenarios with a stuck transaction: another session holds a lock on the
// wallet's funds and does not finish. the models differ here by design: the
// account model has one row to wait for, the utxo model can spend around it.

// lockStuck opens a transaction holding the lock the given query takes and
// keeps it open until the test ends
func lockStuck(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	tag, err := tx.Exec(ctx, sql, args...)
	require.NoError(t, err)
	require.Positive(t, tag.RowsAffected(), "nothing locked")
}

// the account model waits for the stuck row and gives up when the context
// does, leaving no partial state
func TestStuckLockAccount(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	r := NewAccountWalletRepo(pool)
	w := newWalletID()

	_, err := r.Credit(ctx, w, model.USD, dec("100"))
	require.NoError(t, err)
	lockStuck(t, pool, `SELECT 1 FROM account WHERE wallet_id = $1 AND currency = 'USD' FOR UPDATE`, w)

	tctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err = r.Debit(tctx, w, model.USD, dec("1"))
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// reads do not take locks
	b, err := r.GetBalance(ctx, w, model.USD)
	require.NoError(t, err)
	assertBalance(t, b, "100", "0")
	assertAccountInvariants(t, pool, w)
}

// the utxo model spends other outputs while one is stuck
func TestStuckLockUTXO(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	r := NewUTXOWalletRepo(pool)
	w := newWalletID()

	_, err := r.Credit(ctx, w, model.USD, dec("100")) // oldest output, gets stuck
	require.NoError(t, err)
	_, err = r.Credit(ctx, w, model.USD, dec("100"))
	require.NoError(t, err)
	lockStuck(t, pool, `
		SELECT 1 FROM utxo WHERE utxo_id = (
			SELECT utxo_id FROM utxo WHERE wallet_id = $1 AND spent_at IS NULL
			ORDER BY utxo_id LIMIT 1
		) FOR UPDATE`, w)

	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := r.Debit(tctx, w, model.USD, dec("1"))
	require.NoError(t, err)
	assertBalance(t, b, "199", "0")

	// more than the unlocked output holds: the funds exist, so it keeps
	// retrying rather than failing with insufficient funds
	tctx2, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel2()
	_, err = r.Debit(tctx2, w, model.USD, dec("150"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, ErrInsufficientFunds)

	// without a deadline it gives up after its retries, telling the caller to
	// retry rather than that the funds are missing
	start := time.Now()
	_, err = r.Debit(ctx, w, model.USD, dec("150"))
	require.ErrorIs(t, err, ErrContended)
	assert.Less(t, time.Since(start), 3*time.Second)

	assertUTXOInvariants(t, pool, w)
}

// a stuck output must not stall contended spends that can be served by the
// others: they retry around it instead of waiting for it
func TestStuckLockUTXOUnderContention(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	r := NewUTXOWalletRepo(pool)
	w := newWalletID()

	_, err := r.Credit(ctx, w, model.USD, dec("100")) // gets stuck
	require.NoError(t, err)
	_, err = r.Credit(ctx, w, model.USD, dec("100")) // the one everybody contends for
	require.NoError(t, err)
	lockStuck(t, pool, `
		SELECT 1 FROM utxo WHERE utxo_id = (
			SELECT utxo_id FROM utxo WHERE wallet_id = $1 AND spent_at IS NULL
			ORDER BY utxo_id LIMIT 1
		) FOR UPDATE`, w)

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := r.Debit(tctx, w, model.USD, dec("1"))
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	b, err := r.GetBalance(ctx, w, model.USD)
	require.NoError(t, err)
	assertBalance(t, b, "180", "0")
	assertUTXOInvariants(t, pool, w)
}

// a canceled operation leaves nothing behind, in either model
func TestCanceledContext(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()
		_, err := r.Credit(ctx, w, model.BTC, dec("1"))
		require.NoError(t, err)

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = r.Credit(canceled, w, model.BTC, dec("1"))
		assert.ErrorIs(t, err, context.Canceled)
		_, err = r.Debit(canceled, w, model.BTC, dec("1"))
		assert.ErrorIs(t, err, context.Canceled)
		_, err = r.Hold(canceled, w, model.BTC, dec("1"))
		assert.ErrorIs(t, err, context.Canceled)

		b, err := r.GetBalance(ctx, w, model.BTC)
		require.NoError(t, err)
		assertBalance(t, b, "1", "0")
		assertInvariants(t, pool, r, w)
	})
}
