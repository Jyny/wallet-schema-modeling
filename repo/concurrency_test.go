package repo

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// correctness under high concurrency, each scenario run against both models
// with more goroutines than the pool has connections

// goroutines of a concurrent scenario
const testWorkers = 64

// testContext bounds a concurrent scenario, so a deadlock fails the test
// instead of hanging it.
func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// fundings runs fn with 100 USD in wallet 1, once as a single credit and once
// as 50: in the utxo model concurrent spends of one output chain through each
// other's change, while many outputs let them pick disjoint ones.
func fundings(t *testing.T, fn func(t *testing.T, r WalletRepo)) {
	for _, outputs := range []int64{1, 50} {
		t.Run(fmt.Sprintf("outputs=%d", outputs), func(t *testing.T) {
			models(t, func(t *testing.T, r WalletRepo) {
				for range outputs {
					fund(t, r, 1, model.USD, decimal.NewFromInt(100/outputs).String())
				}
				fn(t, r)
			})
		})
	}
}

// 200 debits of 1 race for 100: exactly 100 win, the balance ends at zero
func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	fundings(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		errs := parallel(200, func(int) error {
			return retryContended(func() error {
				_, err := r.Debit(ctx, 1, model.USD, dec("1"))
				return err
			})
		})
		ok, insufficient := countErrors(t, errs, ErrInsufficientFunds)
		assert.Equal(t, 100, ok)
		assert.Equal(t, 100, insufficient)
		assertBalance(t, r, 1, model.USD, "0", "0")
	})
}

// 150 holds of 1 race for 100, then the winners are settled and released
// concurrently
func TestConcurrentHoldsNeverOverdraw(t *testing.T) {
	fundings(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		holds := make([]model.Hold, 150)
		errs := parallel(len(holds), func(i int) error {
			return retryContended(func() (err error) {
				holds[i], err = r.Hold(ctx, 1, model.USD, dec("1"))
				return err
			})
		})
		ok, insufficient := countErrors(t, errs, ErrInsufficientFunds)
		require.Equal(t, 100, ok)
		assert.Equal(t, 50, insufficient)
		assertBalance(t, r, 1, model.USD, "0", "100")

		var open []model.Hold
		for i, h := range holds {
			if errs[i] == nil {
				open = append(open, h)
			}
		}
		errs = parallel(len(open), func(i int) (err error) {
			if i%2 == 0 {
				_, err = r.SettleHold(ctx, open[i].ID)
			} else {
				_, err = r.ReleaseHold(ctx, open[i].ID)
			}
			return err
		})
		ok, _ = countErrors(t, errs, nil)
		assert.Equal(t, 100, ok)
		assertBalance(t, r, 1, model.USD, "50", "0")
	})
}

// releases and settles race to close one hold: exactly one wins, and the
// balance is the winner's
func TestConcurrentCloseOfOneHold(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		fund(t, r, 1, model.USD, "100")
		for round := range 10 {
			hold, err := r.Hold(ctx, 1, model.USD, dec("10"))
			require.NoError(t, err)

			var released atomic.Bool
			errs := parallel(testWorkers, func(i int) (err error) {
				if i%2 == 0 {
					_, err = r.SettleHold(ctx, hold.ID)
					return err
				}
				if _, err = r.ReleaseHold(ctx, hold.ID); err == nil {
					released.Store(true)
				}
				return err
			})
			ok, notFound := countErrors(t, errs, ErrHoldNotFound)
			require.Equal(t, 1, ok, "round %d", round)
			require.Equal(t, testWorkers-1, notFound, "round %d", round)

			want := "90"
			if released.Load() {
				want = "100"
			}
			assertBalance(t, r, 1, model.USD, want, "0")
			// back to 100 for the next round
			if !released.Load() {
				fund(t, r, 1, model.USD, "10")
			}
		}
	})
}

// concurrent first credits of a wallet: the account model races to create
// the row
func TestConcurrentFirstCredits(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		for wallet := int64(1); wallet <= 10; wallet++ {
			errs := parallel(testWorkers, func(int) error {
				_, err := r.Credit(ctx, wallet, model.USD, dec("0.01"))
				return err
			})
			ok, _ := countErrors(t, errs, nil)
			require.Equal(t, testWorkers, ok)
			assertBalance(t, r, wallet, model.USD, "0.64", "0")
		}
	})
}

// random transfers among a few wallets, many of them failing for lack of
// funds: money is neither created nor lost, and no hold stays open
func TestConcurrentTransfersConserveTotal(t *testing.T) {
	const wallets = 8
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		for w := int64(1); w <= wallets; w++ {
			fund(t, r, w, model.USD, "100")
		}

		var done atomic.Int64
		errs := parallel(testWorkers, func(i int) error {
			rng := rand.New(rand.NewPCG(uint64(i), 1))
			for range 20 {
				from := 1 + rng.Int64N(wallets)
				to := 1 + (from+rng.Int64N(wallets-1))%wallets
				amount := decimal.New(1+rng.Int64N(8000), -2)
				err := retryContended(func() error { return transfer(ctx, r, from, to, model.USD, amount) })
				switch {
				case err == nil:
					done.Add(1)
				case !errors.Is(err, ErrInsufficientFunds):
					return err
				}
			}
			return nil
		})
		countErrors(t, errs, nil)
		require.NotZero(t, done.Load(), "no transfer went through")

		total := decimal.Zero
		for w := int64(1); w <= wallets; w++ {
			b, err := r.GetBalance(ctx, w, model.USD)
			require.NoError(t, err)
			assert.True(t, b.Held.IsZero(), "wallet %d still holds %s", w, b.Held)
			assert.False(t, b.Available.IsNegative(), "wallet %d available %s", w, b.Available)
			total = total.Add(b.Available)
		}
		assert.True(t, total.Equal(dec("800")), "total: want 800, got %s", total)
	})
}

// every kind of operation at random across wallets and currencies: the final
// balances equal the sum of what the successful operations reported
func TestConcurrentMixedWorkloadMatchesLedger(t *testing.T) {
	type key struct {
		wallet int64
		cur    model.Currency
	}
	const wallets = 4
	currencies := []model.Currency{model.USD, model.BTC}
	scales := map[model.Currency]int32{model.USD: 2, model.BTC: 8}

	models(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		var mu sync.Mutex
		available := map[key]decimal.Decimal{}
		held := map[key]decimal.Decimal{}
		record := func(k key, availableDelta, heldDelta decimal.Decimal) {
			mu.Lock()
			defer mu.Unlock()
			available[k] = available[k].Add(availableDelta)
			held[k] = held[k].Add(heldDelta)
		}
		for w := int64(1); w <= wallets; w++ {
			for _, cur := range currencies {
				fund(t, r, w, cur, "50")
				record(key{w, cur}, dec("50"), decimal.Zero)
			}
		}

		errs := parallel(testWorkers, func(i int) error {
			rng := rand.New(rand.NewPCG(uint64(i), 2))
			var open []model.Hold
			for range 40 {
				k := key{1 + rng.Int64N(wallets), currencies[rng.IntN(len(currencies))]}
				amount := decimal.New(1+rng.Int64N(3000), -scales[k.cur])
				var err error
				switch kind := rng.IntN(5); {
				case kind == 0:
					if _, err = r.Credit(ctx, k.wallet, k.cur, amount); err == nil {
						record(k, amount, decimal.Zero)
					}
				case kind == 1:
					err = retryContended(func() error {
						_, err := r.Debit(ctx, k.wallet, k.cur, amount)
						return err
					})
					if err == nil {
						record(k, amount.Neg(), decimal.Zero)
					}
				case kind == 2:
					var h model.Hold
					err = retryContended(func() (err error) {
						h, err = r.Hold(ctx, k.wallet, k.cur, amount)
						return err
					})
					if err == nil {
						open = append(open, h)
						record(k, amount.Neg(), amount)
					}
				case len(open) == 0:
				default:
					h := open[len(open)-1]
					open = open[:len(open)-1]
					hk := key{h.WalletID, h.Currency}
					if kind == 3 {
						if _, err = r.ReleaseHold(ctx, h.ID); err == nil {
							record(hk, h.Amount, h.Amount.Neg())
						}
					} else if _, err = r.SettleHold(ctx, h.ID); err == nil {
						record(hk, decimal.Zero, h.Amount.Neg())
					}
				}
				if err != nil && !errors.Is(err, ErrInsufficientFunds) {
					return err
				}
			}
			return nil
		})
		countErrors(t, errs, nil)

		for k := range available {
			assertBalance(t, r, k.wallet, k.cur, available[k].String(), held[k].String())
		}
	})
}

// a balance read is one snapshot: while holds open and close, available plus
// held never strays from the total
func TestConcurrentReadsSeeWholeOperations(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := testContext(t)
		for range 16 {
			fund(t, r, 1, model.USD, "10")
		}
		total := dec("160")

		var writers sync.WaitGroup
		var writing atomic.Bool
		writing.Store(true)
		for range testWorkers / 2 {
			writers.Go(func() {
				for range 20 {
					var h model.Hold
					err := retryContended(func() (err error) {
						h, err = r.Hold(ctx, 1, model.USD, dec("7"))
						return err
					})
					if errors.Is(err, ErrInsufficientFunds) {
						continue
					}
					if !assert.NoError(t, err) {
						return
					}
					_, err = r.ReleaseHold(ctx, h.ID)
					assert.NoError(t, err)
				}
			})
		}
		go func() { writers.Wait(); writing.Store(false) }()
		errs := parallel(testWorkers/2, func(int) error {
			for writing.Load() {
				b, err := r.GetBalance(ctx, 1, model.USD)
				if err != nil {
					return err
				}
				if !b.Total().Equal(total) {
					return fmt.Errorf("read available %s + held %s, want total %s", b.Available, b.Held, total)
				}
				balances, err := r.ListBalances(ctx, 1)
				if err != nil {
					return err
				}
				for _, b := range balances {
					if b.Currency == model.USD && !b.Total().Equal(total) {
						return fmt.Errorf("listed available %s + held %s, want total %s", b.Available, b.Held, total)
					}
				}
			}
			return nil
		})
		countErrors(t, errs, nil)
		writers.Wait()
		assertBalance(t, r, 1, model.USD, "160", "0")
	})
}
