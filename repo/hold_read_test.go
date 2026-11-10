package repo

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// how reads (GetBalance, ListBalances) and holds interact, run against both
// models

// assertReads checks that GetBalance and the ListBalances row of cur agree
// with each other and with the expected balance
func assertReads(t *testing.T, r WalletRepo, walletID int64, cur model.Currency, available, held string) {
	t.Helper()
	ctx := context.Background()
	b, err := r.GetBalance(ctx, walletID, cur)
	require.NoError(t, err)
	assertBalance(t, b, available, held)

	bs, err := r.ListBalances(ctx, walletID)
	require.NoError(t, err)
	for _, lb := range bs {
		if lb.Currency == cur {
			assertBalance(t, lb, available, held)
			return
		}
	}
	t.Fatalf("%s missing from ListBalances", cur)
}

// every step of overlapping holds shows up in both reads and in the balance
// the operation returns; failed operations leave the reads unchanged, held
// funds cannot be spent, and holds of one currency leave another untouched
func TestHoldLifecycleReads(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.USDT, dec("100"))
		require.NoError(t, err)
		_, err = r.Credit(ctx, w, model.BTC, dec("1"))
		require.NoError(t, err)

		h1, err := r.Hold(ctx, w, model.USDT, dec("10"))
		require.NoError(t, err)
		assertReads(t, r, w, model.USDT, "90", "10")
		h2, err := r.Hold(ctx, w, model.USDT, dec("20"))
		require.NoError(t, err)
		h3, err := r.Hold(ctx, w, model.USDT, dec("30"))
		require.NoError(t, err)
		assertReads(t, r, w, model.USDT, "40", "60")
		assertReads(t, r, w, model.BTC, "1", "0")

		// held funds are not available: only the 40 left can be spent
		_, err = r.Hold(ctx, w, model.USDT, dec("40.000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
		_, err = r.Debit(ctx, w, model.USDT, dec("40.000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
		assertReads(t, r, w, model.USDT, "40", "60")

		b, err := r.Debit(ctx, w, model.USDT, dec("40"))
		require.NoError(t, err)
		assertBalance(t, b, "0", "60")
		assertReads(t, r, w, model.USDT, "0", "60")

		// only held funds left
		_, err = r.Hold(ctx, w, model.USDT, dec("0.000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
		assertReads(t, r, w, model.USDT, "0", "60")

		b, err = r.SettleHold(ctx, h2.ID)
		require.NoError(t, err)
		assertBalance(t, b, "0", "40")
		assertReads(t, r, w, model.USDT, "0", "40")

		b, err = r.ReleaseHold(ctx, h1.ID)
		require.NoError(t, err)
		assertBalance(t, b, "10", "30")
		assertReads(t, r, w, model.USDT, "10", "30")

		// released funds are spendable again
		b, err = r.Debit(ctx, w, model.USDT, dec("10"))
		require.NoError(t, err)
		assertBalance(t, b, "0", "30")

		b, err = r.ReleaseHold(ctx, h3.ID)
		require.NoError(t, err)
		assertBalance(t, b, "30", "0")
		_, err = r.SettleHold(ctx, h3.ID)
		assert.ErrorIs(t, err, ErrHoldNotFound)
		assertReads(t, r, w, model.USDT, "30", "0")
		assertReads(t, r, w, model.BTC, "1", "0")
		assertInvariants(t, pool, r, w)
	})
}

// a release or settle stuck in flight holds the hold's locks. reads do not
// wait for them and still see the hold; closing the same hold waits; a new
// hold waits for the account row but spends around the locked output in utxo
func TestReadsDuringStuckHoldClose(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.BTC, dec("1"))
		require.NoError(t, err)
		h, err := r.Hold(ctx, w, model.BTC, dec("0.3"))
		require.NoError(t, err)

		// the locks a close takes: the hold and the account row, or the held output
		switch r.(type) {
		case *AccountWalletRepo:
			lockStuck(t, pool, `
				SELECT 1 FROM account_hold AS h
				JOIN account AS a USING (wallet_id, currency)
				WHERE h.hold_id = $1 FOR UPDATE`, h.ID)
		case *UTXOWalletRepo:
			lockStuck(t, pool, `SELECT 1 FROM utxo WHERE hold_id = $1 FOR UPDATE`, h.ID)
		}

		rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer rcancel()
		b, err := r.GetBalance(rctx, w, model.BTC)
		require.NoError(t, err, "reads must not wait for locks")
		assertBalance(t, b, "0.7", "0.3")
		_, err = r.ListBalances(rctx, w)
		require.NoError(t, err, "reads must not wait for locks")

		cctx, ccancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer ccancel()
		_, err = r.ReleaseHold(cctx, h.ID)
		require.ErrorIs(t, err, context.DeadlineExceeded)

		hctx, hcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer hcancel()
		_, err = r.Hold(hctx, w, model.BTC, dec("0.2"))
		switch r.(type) {
		case *AccountWalletRepo:
			require.ErrorIs(t, err, context.DeadlineExceeded)
			assertReads(t, r, w, model.BTC, "0.7", "0.3")
		case *UTXOWalletRepo:
			require.NoError(t, err)
			assertReads(t, r, w, model.BTC, "0.5", "0.5")
		}
		assertInvariants(t, pool, r, w)
	})
}

// workers hold then settle or release while readers poll. every read is a
// state between whole operations: held is a whole number of open holds,
// total is the funding less a whole number of settles, and to each reader the
// total never goes back up
func TestConcurrentReadsDuringHoldSettle(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()
		unit := dec("1.25")

		for range 8 {
			_, err := r.Credit(ctx, w, model.USD, dec("12.5"))
			require.NoError(t, err)
		}

		const workers, steps = 4, 20
		var done atomic.Bool
		var writers, readers sync.WaitGroup
		for range workers {
			writers.Go(func() {
				for i := range steps {
					h, err := r.Hold(ctx, w, model.USD, unit)
					if !assert.NoError(t, err) {
						return
					}
					if i%2 == 0 {
						_, err = r.SettleHold(ctx, h.ID)
					} else {
						_, err = r.ReleaseHold(ctx, h.ID)
					}
					assert.NoError(t, err)
				}
			})
		}
		maxHeld := unit.Mul(decimal.NewFromInt(workers))
		var reads atomic.Int64
		for range 4 {
			readers.Go(func() {
				last := dec("100")
				for !done.Load() {
					b, err := r.GetBalance(ctx, w, model.USD)
					if !assert.NoError(t, err) {
						return
					}
					total := b.Total()
					assert.False(t, b.Available.IsNegative(), "available %s", b.Available)
					assert.False(t, b.Held.IsNegative(), "held %s", b.Held)
					assert.True(t, b.Held.LessThanOrEqual(maxHeld), "held %s above %s", b.Held, maxHeld)
					assert.True(t, b.Held.Mod(unit).IsZero(), "held %s not whole holds", b.Held)
					assert.True(t, dec("100").Sub(total).Mod(unit).IsZero(), "total %s not whole settles", total)
					assert.True(t, total.LessThanOrEqual(last), "total went up from %s to %s", last, total)
					last = total
					reads.Add(1)
				}
			})
		}
		writers.Wait()
		done.Store(true)
		readers.Wait()
		t.Logf("%d consistent reads", reads.Load())

		// half of the holds settled: 40 x 1.25
		assertReads(t, r, w, model.USD, "50", "0")
		assertInvariants(t, pool, r, w)
	})
}

// holds and releases on two currencies at once: each ListBalances row is
// consistent on its own, totals never move, and untouched currencies stay zero
func TestConcurrentListBalancesDuringHolds(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for range 5 {
			_, err := r.Credit(ctx, w, model.USDT, dec("20"))
			require.NoError(t, err)
		}
		for range 4 {
			_, err := r.Credit(ctx, w, model.BTC, dec("0.25"))
			require.NoError(t, err)
		}
		totals := map[model.Currency]decimal.Decimal{
			model.USDT: dec("100"),
			model.BTC:  dec("1"),
			model.USD:  decimal.Zero,
			model.ETH:  decimal.Zero,
		}
		amounts := map[model.Currency]decimal.Decimal{
			model.USDT: dec("7.5"),
			model.BTC:  dec("0.1"),
		}

		var done atomic.Bool
		var writers, readers sync.WaitGroup
		for i := range 4 {
			cur := []model.Currency{model.USDT, model.BTC}[i%2]
			writers.Go(func() {
				rng := rand.New(rand.NewPCG(uint64(i), 11))
				for range 20 {
					h, err := r.Hold(ctx, w, cur, amounts[cur])
					if !assert.NoError(t, err) {
						return
					}
					if rng.IntN(2) == 0 {
						// reads in the middle of an open hold
						_, err = r.GetBalance(ctx, w, cur)
						assert.NoError(t, err)
					}
					_, err = r.ReleaseHold(ctx, h.ID)
					assert.NoError(t, err)
				}
			})
		}
		var reads atomic.Int64
		for range 4 {
			readers.Go(func() {
				for !done.Load() {
					bs, err := r.ListBalances(ctx, w)
					if !assert.NoError(t, err) || !assert.Len(t, bs, len(totals)) {
						return
					}
					for _, b := range bs {
						assert.True(t, b.Total().Equal(totals[b.Currency]), "%s total %s (available %s, held %s)",
							b.Currency, b.Total(), b.Available, b.Held)
						assert.False(t, b.Available.IsNegative(), "%s available %s", b.Currency, b.Available)
						assert.False(t, b.Held.IsNegative(), "%s held %s", b.Currency, b.Held)
						if a, ok := amounts[b.Currency]; ok {
							// two writers per currency, one open hold each at most
							assert.True(t, b.Held.LessThanOrEqual(a.Mul(decimal.NewFromInt(2))), "%s held %s", b.Currency, b.Held)
						}
					}
					reads.Add(1)
				}
			})
		}
		writers.Wait()
		done.Store(true)
		readers.Wait()
		t.Logf("%d consistent reads", reads.Load())

		assertReads(t, r, w, model.USDT, "100", "0")
		assertReads(t, r, w, model.BTC, "1", "0")
		assertInvariants(t, pool, r, w)
	})
}
