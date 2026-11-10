package repo

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// several wallets at once, run against both models: an account is a (wallet,
// currency), so operations on one must never touch another

type accountKey struct {
	wallet int64
	cur    model.Currency
}

// operations on one wallet leave another untouched: balances, holds closed by
// id, and coin selection, which must not pick the other wallet's outputs
func TestWalletIsolation(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		a, b := newWalletID(), newWalletID()

		_, err := r.Credit(ctx, a, model.USD, dec("100"))
		require.NoError(t, err)
		_, err = r.Credit(ctx, b, model.USD, dec("5"))
		require.NoError(t, err)

		// b cannot spend a's funds
		_, err = r.Debit(ctx, b, model.USD, dec("6"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
		_, err = r.Hold(ctx, b, model.USD, dec("6"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)

		h, err := r.Hold(ctx, a, model.USD, dec("30"))
		require.NoError(t, err)
		assertReads(t, r, a, model.USD, "70", "30")
		assertReads(t, r, b, model.USD, "5", "0")

		// closing a hold by id acts on its own wallet, whoever asks
		bal, err := r.SettleHold(ctx, h.ID)
		require.NoError(t, err)
		assert.Equal(t, a, bal.WalletID)
		assertBalance(t, bal, "70", "0")
		assertReads(t, r, b, model.USD, "5", "0")

		b2, err := r.Debit(ctx, b, model.USD, dec("5"))
		require.NoError(t, err)
		assert.Equal(t, b, b2.WalletID)
		assertBalance(t, b2, "0", "0")
		assertReads(t, r, a, model.USD, "70", "0")

		// a wallet never credited reads as zero, unaffected by the others
		bs, err := r.ListBalances(ctx, newWalletID())
		require.NoError(t, err)
		for _, x := range bs {
			assertBalance(t, x, "0", "0")
		}
		for _, w := range []int64{a, b} {
			assertInvariants(t, pool, r, w)
		}
	})
}

// workers run random operations on random accounts of several wallets and two
// currencies. every success is accounted for on its own account, the only
// acceptable failure is insufficient funds, and each account's final balance
// and each wallet's invariants must match
func TestConcurrentMultiWallet(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		const wallets = 6
		currencies := []model.Currency{model.USDT, model.BTC}
		units := map[model.Currency]int32{model.USDT: -6, model.BTC: -8}

		var ids []int64
		want := map[accountKey]*ledger{}
		for range wallets {
			w := newWalletID()
			ids = append(ids, w)
			for _, cur := range currencies {
				_, err := r.Credit(ctx, w, cur, dec("100"))
				require.NoError(t, err)
				want[accountKey{w, cur}] = &ledger{available: dec("100")}
			}
		}

		const workers, steps = 16, 40
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				rng := rand.New(rand.NewPCG(uint64(i), 13))
				var holds []model.Hold // each worker closes only its own holds
				for range steps {
					k := accountKey{ids[rng.IntN(wallets)], currencies[rng.IntN(len(currencies))]}
					l := want[k]
					amount := decimal.New(rng.Int64N(20_000_000)+1, units[k.cur]) // up to 20 USDT, 0.2 BTC
					var err error
					switch op := rng.IntN(6); {
					case op == 0:
						if _, err = r.Credit(ctx, k.wallet, k.cur, amount); err == nil {
							l.add(amount, decimal.Zero)
						}
					case op <= 2:
						if _, err = r.Debit(ctx, k.wallet, k.cur, amount); err == nil {
							l.add(amount.Neg(), decimal.Zero)
						}
					case op == 3:
						var h model.Hold
						if h, err = r.Hold(ctx, k.wallet, k.cur, amount); err == nil {
							l.add(amount.Neg(), amount)
							holds = append(holds, h)
						}
					case len(holds) > 0:
						h := holds[0]
						holds = holds[1:]
						hl := want[accountKey{h.WalletID, h.Currency}]
						var b model.Balance
						if op == 4 {
							if b, err = r.ReleaseHold(ctx, h.ID); err == nil {
								hl.add(h.Amount, h.Amount.Neg())
							}
						} else {
							if b, err = r.SettleHold(ctx, h.ID); err == nil {
								hl.add(decimal.Zero, h.Amount.Neg())
							}
						}
						if assert.NoError(t, err, "closing own hold") {
							assert.Equal(t, h.WalletID, b.WalletID, "closed on its own wallet")
							assert.Equal(t, h.Currency, b.Currency, "closed on its own currency")
						}
					}
					if err != nil {
						assert.ErrorIs(t, err, ErrInsufficientFunds)
					}
				}
			})
		}
		wg.Wait()

		for _, w := range ids {
			bs, err := r.ListBalances(ctx, w)
			require.NoError(t, err)
			for _, b := range bs {
				l, ok := want[accountKey{w, b.Currency}]
				if !ok {
					assertBalance(t, b, "0", "0")
					continue
				}
				assertBalance(t, b, l.available.String(), l.held.String())
			}
			assertInvariants(t, pool, r, w)
		}
	})
}
