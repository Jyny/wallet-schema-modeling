package repo

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"walletmodeling/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// behaviour every WalletRepo must share, run against both models

func TestGetBalance(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		b, err := r.GetBalance(ctx, w, model.BTC)
		require.NoError(t, err)
		assert.Equal(t, w, b.WalletID)
		assert.Equal(t, model.BTC, b.Currency)
		assertBalance(t, b, "0", "0")

		_, err = r.GetBalance(ctx, w, "DOGE")
		assert.ErrorIs(t, err, ErrUnknownCurrency)
	})
}

func TestListBalances(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.USDT, dec("12.5"))
		require.NoError(t, err)
		_, err = r.Hold(ctx, w, model.USDT, dec("2.5"))
		require.NoError(t, err)

		bs, err := r.ListBalances(ctx, w)
		require.NoError(t, err)
		require.Len(t, bs, 4)
		got := map[model.Currency]model.Balance{}
		for _, b := range bs {
			assert.Equal(t, w, b.WalletID)
			got[b.Currency] = b
		}
		assertBalance(t, got[model.USDT], "10", "2.5")
		assertBalance(t, got[model.USD], "0", "0")
		assertBalance(t, got[model.BTC], "0", "0")
		assertBalance(t, got[model.ETH], "0", "0")
	})
}

func TestCreditDebit(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		b, err := r.Credit(ctx, w, model.ETH, dec("1.000000000000000001"))
		require.NoError(t, err)
		assertBalance(t, b, "1.000000000000000001", "0")

		b, err = r.Credit(ctx, w, model.ETH, dec("2"))
		require.NoError(t, err)
		assertBalance(t, b, "3.000000000000000001", "0")

		b, err = r.Debit(ctx, w, model.ETH, dec("0.000000000000000001"))
		require.NoError(t, err)
		assertBalance(t, b, "3", "0")

		// exact spend down to zero
		b, err = r.Debit(ctx, w, model.ETH, dec("3"))
		require.NoError(t, err)
		assertBalance(t, b, "0", "0")

		// other currencies of the wallet are untouched
		b, err = r.GetBalance(ctx, w, model.BTC)
		require.NoError(t, err)
		assertBalance(t, b, "0", "0")
	})
}

func TestDebitInsufficientFunds(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		// no history at all
		_, err := r.Debit(ctx, w, model.USD, dec("0.01"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)

		_, err = r.Credit(ctx, w, model.USD, dec("100"))
		require.NoError(t, err)
		_, err = r.Debit(ctx, w, model.USD, dec("100.01"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)

		// held funds are not spendable
		_, err = r.Hold(ctx, w, model.USD, dec("60"))
		require.NoError(t, err)
		_, err = r.Debit(ctx, w, model.USD, dec("40.01"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)

		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "40", "60")
	})
}

func TestInvalidAmount(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for _, tc := range []struct {
			cur    model.Currency
			amount string
		}{
			{model.USD, "0"},
			{model.USD, "-1"},
			{model.USD, "0.001"},                 // USD scale 2
			{model.USDT, "0.0000001"},            // USDT scale 6
			{model.BTC, "0.000000001"},           // BTC scale 8
			{model.ETH, "0.0000000000000000001"}, // ETH scale 18
			{model.USD, "100000000000000000000"}, // numeric(38,18) holds 20 integer digits
		} {
			_, err := r.Credit(ctx, w, tc.cur, dec(tc.amount))
			assert.ErrorIs(t, err, ErrInvalidAmount, "credit %s %s", tc.amount, tc.cur)
			_, err = r.Debit(ctx, w, tc.cur, dec(tc.amount))
			assert.ErrorIs(t, err, ErrInvalidAmount, "debit %s %s", tc.amount, tc.cur)
			_, err = r.Hold(ctx, w, tc.cur, dec(tc.amount))
			assert.ErrorIs(t, err, ErrInvalidAmount, "hold %s %s", tc.amount, tc.cur)
		}

		// trailing zeros within the scale are fine
		_, err := r.Credit(ctx, w, model.USD, dec("1.500"))
		assert.NoError(t, err)

		// a credit pushing the balance out of range fails and changes nothing
		_, err = r.Credit(ctx, w, model.USD, dec("99999999999999999999"))
		assert.ErrorIs(t, err, ErrInvalidAmount)
		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "1.5", "0")

		_, err = r.Credit(ctx, w, "DOGE", dec("1"))
		assert.ErrorIs(t, err, ErrUnknownCurrency)
	})
}

// held, like available, stays within numeric(38,18), and a rejected hold
// leaves the balance readable and unchanged
func TestHoldOverflow(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()
		big := dec("90000000000000000000")

		_, err := r.Credit(ctx, w, model.USD, big)
		require.NoError(t, err)
		_, err = r.Hold(ctx, w, model.USD, big)
		require.NoError(t, err)
		_, err = r.Credit(ctx, w, model.USD, big)
		require.NoError(t, err)

		_, err = r.Hold(ctx, w, model.USD, big)
		assert.ErrorIs(t, err, ErrInvalidAmount)

		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "90000000000000000000", "90000000000000000000")
		_, err = r.ListBalances(ctx, w)
		require.NoError(t, err)
	})
}

func TestHoldRelease(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.BTC, dec("1"))
		require.NoError(t, err)

		h, err := r.Hold(ctx, w, model.BTC, dec("0.3"))
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, h.ID)
		assert.Equal(t, w, h.WalletID)
		assert.Equal(t, model.BTC, h.Currency)
		assert.True(t, h.Amount.Equal(dec("0.3")))

		b, err := r.GetBalance(ctx, w, model.BTC)
		require.NoError(t, err)
		assertBalance(t, b, "0.7", "0.3")

		_, err = r.Hold(ctx, w, model.BTC, dec("0.70000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)

		b, err = r.ReleaseHold(ctx, h.ID)
		require.NoError(t, err)
		assertBalance(t, b, "1", "0")

		// a hold closes once
		_, err = r.ReleaseHold(ctx, h.ID)
		assert.ErrorIs(t, err, ErrHoldNotFound)
		_, err = r.SettleHold(ctx, h.ID)
		assert.ErrorIs(t, err, ErrHoldNotFound)
	})
}

func TestHoldSettle(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.USDT, dec("100"))
		require.NoError(t, err)
		h1, err := r.Hold(ctx, w, model.USDT, dec("30"))
		require.NoError(t, err)
		h2, err := r.Hold(ctx, w, model.USDT, dec("20.000001"))
		require.NoError(t, err)

		b, err := r.SettleHold(ctx, h1.ID)
		require.NoError(t, err)
		assertBalance(t, b, "49.999999", "20.000001")

		b, err = r.ReleaseHold(ctx, h2.ID)
		require.NoError(t, err)
		assertBalance(t, b, "70", "0")

		_, err = r.SettleHold(ctx, h1.ID)
		assert.ErrorIs(t, err, ErrHoldNotFound)
		_, err = r.SettleHold(ctx, uuid.Must(uuid.NewV7()))
		assert.ErrorIs(t, err, ErrHoldNotFound)
	})
}

// concurrent debits never lose or invent money, and all succeed while funds
// suffice: the account model serializes on the account row, the utxo model spends
// disjoint outputs and retries when every output is locked by others.
func TestConcurrentDebits(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		const outputs, workers = 20, 20
		for range outputs {
			_, err := r.Credit(ctx, w, model.USD, dec("10"))
			require.NoError(t, err)
		}

		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				_, err := r.Debit(ctx, w, model.USD, dec("7.5"))
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "50", "0")
	})
}

// many concurrent debits and holds contending for a single output: in the utxo
// model each one finds it locked or already spent, its change not yet visible,
// and must not mistake that for insufficient funds
func TestContendedSingleOutput(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.USD, dec("100"))
		require.NoError(t, err)

		var wg sync.WaitGroup
		for i := range 30 {
			wg.Go(func() {
				var err error
				if i%3 == 0 {
					_, err = r.Hold(ctx, w, model.USD, dec("2"))
				} else {
					_, err = r.Debit(ctx, w, model.USD, dec("1"))
				}
				assert.NoError(t, err)
			})
		}
		wg.Wait()

		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "60", "20")

		// once funds really run out, contention still ends in insufficient funds
		var ok, insufficient atomic.Int64
		for range 20 {
			wg.Go(func() {
				_, err := r.Debit(ctx, w, model.USD, dec("7"))
				if err == nil {
					ok.Add(1)
					return
				}
				assert.ErrorIs(t, err, ErrInsufficientFunds)
				insufficient.Add(1)
			})
		}
		wg.Wait()
		assert.Equal(t, int64(8), ok.Load())
		assert.Equal(t, int64(12), insufficient.Load())
		b, err = r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "4", "20")
	})
}

// racing release and settle of one hold: exactly one wins
func TestConcurrentCloseHold(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.ETH, dec("5"))
		require.NoError(t, err)
		h, err := r.Hold(ctx, w, model.ETH, dec("2"))
		require.NoError(t, err)

		var released, settled, lost atomic.Int64
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Go(func() {
				var err error
				if i%2 == 0 {
					if _, err = r.ReleaseHold(ctx, h.ID); err == nil {
						released.Add(1)
					}
				} else {
					if _, err = r.SettleHold(ctx, h.ID); err == nil {
						settled.Add(1)
					}
				}
				if err != nil {
					assert.ErrorIs(t, err, ErrHoldNotFound)
					lost.Add(1)
				}
			})
		}
		wg.Wait()

		require.Equal(t, int64(1), released.Load()+settled.Load())
		require.Equal(t, int64(9), lost.Load())
		b, err := r.GetBalance(ctx, w, model.ETH)
		require.NoError(t, err)
		if released.Load() == 1 {
			assertBalance(t, b, "5", "0")
		} else {
			assertBalance(t, b, "3", "0")
		}
	})
}
