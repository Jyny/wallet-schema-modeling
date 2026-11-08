package repo

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// concurrency scenarios, run against both models

// ledger tracks what the successful operations of a scenario must add up to
type ledger struct {
	mu        sync.Mutex
	available decimal.Decimal
	held      decimal.Decimal
}

func (l *ledger) add(available, held decimal.Decimal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.available = l.available.Add(available)
	l.held = l.held.Add(held)
}

func assertInvariants(t *testing.T, pool *pgxpool.Pool, r WalletRepo, walletID int64) {
	t.Helper()
	switch r.(type) {
	case *AccountWalletRepo:
		assertAccountInvariants(t, pool, walletID)
	case *UTXOWalletRepo:
		assertUTXOInvariants(t, pool, walletID)
	}
}

// workers run random operations on one account. every success is accounted
// for, the only acceptable failure is insufficient funds, and the final
// balance and the model's internal invariants must match
func TestConcurrentMixedWorkload(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()
		var want ledger

		_, err := r.Credit(ctx, w, model.USDT, dec("1000"))
		require.NoError(t, err)
		want.add(dec("1000"), decimal.Zero)

		const workers, steps = 16, 40
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				rng := rand.New(rand.NewPCG(uint64(i), 7))
				var holds []model.Hold // each worker closes only its own holds
				for range steps {
					amount := decimal.New(rng.Int64N(50_000_000)+1, -6) // up to 50 USDT
					var err error
					switch op := rng.IntN(6); {
					case op == 0:
						if _, err = r.Credit(ctx, w, model.USDT, amount); err == nil {
							want.add(amount, decimal.Zero)
						}
					case op <= 2:
						if _, err = r.Debit(ctx, w, model.USDT, amount); err == nil {
							want.add(amount.Neg(), decimal.Zero)
						}
					case op == 3:
						var h model.Hold
						if h, err = r.Hold(ctx, w, model.USDT, amount); err == nil {
							want.add(amount.Neg(), amount)
							holds = append(holds, h)
						}
					case len(holds) > 0:
						h := holds[0]
						holds = holds[1:]
						if op == 4 {
							if _, err = r.ReleaseHold(ctx, h.ID); err == nil {
								want.add(h.Amount, h.Amount.Neg())
							}
						} else {
							if _, err = r.SettleHold(ctx, h.ID); err == nil {
								want.add(decimal.Zero, h.Amount.Neg())
							}
						}
						assert.NoError(t, err, "closing own hold")
					}
					if err != nil {
						assert.ErrorIs(t, err, ErrInsufficientFunds)
					}
				}
			})
		}
		wg.Wait()

		b, err := r.GetBalance(ctx, w, model.USDT)
		require.NoError(t, err)
		assertBalance(t, b, want.available.String(), want.held.String())
		assertInvariants(t, pool, r, w)
	})
}

// funds for exactly half of the holds, spread over several outputs: exactly
// that many succeed, no overdraft, no false insufficient funds
func TestConcurrentHoldsExactFunds(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for _, a := range []string{"30", "25", "20", "15", "10"} { // 100
			_, err := r.Credit(ctx, w, model.BTC, dec(a))
			require.NoError(t, err)
		}

		var ok, insufficient atomic.Int64
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				_, err := r.Hold(ctx, w, model.BTC, dec("10"))
				if err == nil {
					ok.Add(1)
					return
				}
				assert.ErrorIs(t, err, ErrInsufficientFunds)
				insufficient.Add(1)
			})
		}
		wg.Wait()

		assert.Equal(t, int64(10), ok.Load())
		assert.Equal(t, int64(10), insufficient.Load())
		b, err := r.GetBalance(ctx, w, model.BTC)
		require.NoError(t, err)
		assertBalance(t, b, "0", "100")
		assertInvariants(t, pool, r, w)
	})
}

// holds and releases never change the total, so no read may see it change:
// a read must never observe a half-applied operation
func TestConcurrentReadsConsistent(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for range 5 {
			_, err := r.Credit(ctx, w, model.ETH, dec("20"))
			require.NoError(t, err)
		}

		var done atomic.Bool
		var writers, readers sync.WaitGroup
		for range 4 {
			writers.Go(func() {
				for range 25 {
					h, err := r.Hold(ctx, w, model.ETH, dec("3.5"))
					if !assert.NoError(t, err) {
						return
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
					b, err := r.GetBalance(ctx, w, model.ETH)
					if !assert.NoError(t, err) {
						return
					}
					assert.True(t, b.Total().Equal(dec("100")), "total %s (available %s, held %s)", b.Total(), b.Available, b.Held)
					reads.Add(1)
				}
			})
		}
		writers.Wait()
		done.Store(true)
		readers.Wait()
		t.Logf("%d consistent reads", reads.Load())

		b, err := r.GetBalance(ctx, w, model.ETH)
		require.NoError(t, err)
		assertBalance(t, b, "100", "0")
	})
}

// concurrent credits to an account that does not exist yet
func TestConcurrentFirstCredit(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				_, err := r.Credit(ctx, w, model.USD, dec("1.25"))
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "25", "0")
		assertInvariants(t, pool, r, w)
	})
}

// two credits racing past the balance bound. whatever the outcome, the
// balance stays readable and equals the credits that succeeded
func TestConcurrentOverflow(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()
		big := dec("60000000000000000000")

		var ok atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 2 {
			wg.Go(func() {
				<-start
				_, err := r.Credit(ctx, w, model.USD, big)
				if err == nil {
					ok.Add(1)
					return
				}
				assert.ErrorIs(t, err, ErrInvalidAmount)
			})
		}
		close(start)
		wg.Wait()

		require.Positive(t, ok.Load())
		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err, "balance must stay readable")
		assert.True(t, b.Available.Equal(big.Mul(decimal.NewFromInt(ok.Load()))), "available %s after %d credits", b.Available, ok.Load())
		_, err = r.ListBalances(ctx, w)
		require.NoError(t, err)
		t.Logf("%d of 2 credits succeeded", ok.Load())

		// coin selection must not overflow either: its running sum reaches
		// 1.2e20 picking both outputs
		if ok.Load() == 2 {
			b, err = r.Debit(ctx, w, model.USD, dec("90000000000000000000"))
			require.NoError(t, err)
			assertBalance(t, b, "30000000000000000000", "0")
		}
	})
}

// contention on one currency must not block or fail another of the same wallet
func TestConcurrentCurrenciesIsolated(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.USD, dec("1"))
		require.NoError(t, err)
		_, err = r.Credit(ctx, w, model.BTC, dec("10"))
		require.NoError(t, err)

		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() {
				if i%2 == 0 {
					// USD is starved: all but one of these fail
					_, err := r.Debit(ctx, w, model.USD, dec("1"))
					if err != nil {
						assert.ErrorIs(t, err, ErrInsufficientFunds)
					}
					return
				}
				_, err := r.Debit(ctx, w, model.BTC, dec("0.5"))
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		bs, err := r.ListBalances(ctx, w)
		require.NoError(t, err)
		for _, b := range bs {
			switch b.Currency {
			case model.USD:
				assertBalance(t, b, "0", "0")
			case model.BTC:
				assertBalance(t, b, "5", "0")
			}
		}
	})
}
