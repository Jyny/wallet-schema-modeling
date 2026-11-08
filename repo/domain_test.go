package repo

import (
	"context"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// domain edge cases, run against both models

// the largest amount each currency accepts round-trips exactly
func TestMaxAmountPerCurrency(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for cur, max := range map[model.Currency]string{
			model.USD:  "99999999999999999999.99",
			model.USDT: "99999999999999999999.999999",
			model.BTC:  "99999999999999999999.99999999",
			model.ETH:  "99999999999999999999.999999999999999999",
		} {
			b, err := r.Credit(ctx, w, cur, dec(max))
			require.NoError(t, err, cur)
			assertBalance(t, b, max, "0")

			// one smallest unit more no longer fits
			_, err = r.Credit(ctx, w, cur, decimal.New(1, -18))
			assert.ErrorIs(t, err, ErrInvalidAmount, cur)

			h, err := r.Hold(ctx, w, cur, dec(max))
			require.NoError(t, err, cur)
			b, err = r.SettleHold(ctx, h.ID)
			require.NoError(t, err, cur)
			assertBalance(t, b, "0", "0")
		}
		assertInvariants(t, pool, r, w)
	})
}

// spending exactly everything leaves zero and no change output
func TestSpendExactlyAll(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for _, a := range []string{"0.1", "0.2", "0.3"} {
			_, err := r.Credit(ctx, w, model.BTC, dec(a))
			require.NoError(t, err)
		}
		b, err := r.Debit(ctx, w, model.BTC, dec("0.6"))
		require.NoError(t, err)
		assertBalance(t, b, "0", "0")

		_, err = r.Debit(ctx, w, model.BTC, dec("0.00000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)

		// hold everything, then nothing is spendable until it is released
		_, err = r.Credit(ctx, w, model.BTC, dec("1"))
		require.NoError(t, err)
		h, err := r.Hold(ctx, w, model.BTC, dec("1"))
		require.NoError(t, err)
		_, err = r.Debit(ctx, w, model.BTC, dec("0.00000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
		_, err = r.Hold(ctx, w, model.BTC, dec("0.00000001"))
		assert.ErrorIs(t, err, ErrInsufficientFunds)
		b, err = r.ReleaseHold(ctx, h.ID)
		require.NoError(t, err)
		assertBalance(t, b, "1", "0")

		if _, ok := r.(*UTXOWalletRepo); ok {
			var n int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM utxo WHERE wallet_id = $1 AND spent_at IS NULL`, w).Scan(&n))
			assert.Equal(t, 1, n, "no zero or change outputs left over")
		}
		assertInvariants(t, pool, r, w)
	})
}

// a debit drawing on hundreds of tiny outputs
func TestManyDustOutputs(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		const n = 300
		for range n {
			_, err := r.Credit(ctx, w, model.USD, dec("0.01"))
			require.NoError(t, err)
		}
		start := time.Now()
		b, err := r.Debit(ctx, w, model.USD, dec("2.99"))
		require.NoError(t, err)
		t.Logf("debit over %d outputs took %s", n, time.Since(start))
		assertBalance(t, b, "0.01", "0")
		assertInvariants(t, pool, r, w)
	})
}

// sub-unit precision is kept exactly: no float arithmetic anywhere
func TestExactArithmetic(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for range 10 {
			_, err := r.Credit(ctx, w, model.ETH, dec("0.1"))
			require.NoError(t, err)
		}
		b, err := r.Debit(ctx, w, model.ETH, dec("0.3"))
		require.NoError(t, err)
		assertBalance(t, b, "0.7", "0")

		_, err = r.Credit(ctx, w, model.ETH, dec("0.000000000000000001"))
		require.NoError(t, err)
		b, err = r.Debit(ctx, w, model.ETH, dec("0.7"))
		require.NoError(t, err)
		assertBalance(t, b, "0.000000000000000001", "0")
	})
}

// currency codes are exact: no case folding, no blanks
func TestCurrencyCodes(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for _, cur := range []model.Currency{"usd", "", " USD", "USD ", "BTC\x00"} {
			_, err := r.GetBalance(ctx, w, cur)
			assert.ErrorIs(t, err, ErrUnknownCurrency, "%q", cur)
			_, err = r.Credit(ctx, w, cur, dec("1"))
			assert.ErrorIs(t, err, ErrUnknownCurrency, "%q", cur)
		}
	})
}

// amount forms that are valid despite their spelling, and the zero value
func TestAmountForms(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		for _, a := range []string{"1E+3", "1.000000000", "0001.50"} {
			_, err := r.Credit(ctx, w, model.USD, dec(a))
			assert.NoError(t, err, a)
		}
		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		assertBalance(t, b, "1002.5", "0")

		_, err = r.Credit(ctx, w, model.USD, decimal.Decimal{})
		assert.ErrorIs(t, err, ErrInvalidAmount)
		_, err = r.Debit(ctx, w, model.USD, dec("-0"))
		assert.ErrorIs(t, err, ErrInvalidAmount)
	})
}

// hold ids only close holds they were issued for
func TestHoldIDs(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	account, utxo := NewAccountWalletRepo(pool), NewUTXOWalletRepo(pool)
	w := newWalletID()

	for _, r := range []WalletRepo{account, utxo} {
		_, err := r.Credit(ctx, w, model.USD, dec("10"))
		require.NoError(t, err)
	}
	ha, err := account.Hold(ctx, w, model.USD, dec("1"))
	require.NoError(t, err)
	hu, err := utxo.Hold(ctx, w, model.USD, dec("1"))
	require.NoError(t, err)

	// a hold of one model is unknown to the other
	_, err = account.ReleaseHold(ctx, hu.ID)
	assert.ErrorIs(t, err, ErrHoldNotFound)
	_, err = utxo.ReleaseHold(ctx, ha.ID)
	assert.ErrorIs(t, err, ErrHoldNotFound)

	for _, r := range []WalletRepo{account, utxo} {
		_, err = r.SettleHold(ctx, uuid.Nil)
		assert.ErrorIs(t, err, ErrHoldNotFound)
	}
}

// releasing back into a full available balance would overflow it: refused,
// and the hold stays open and can still be settled
func TestReleaseOverflow(t *testing.T) {
	models(t, func(t *testing.T, pool *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()
		big := dec("90000000000000000000")

		_, err := r.Credit(ctx, w, model.USD, big)
		require.NoError(t, err)
		h, err := r.Hold(ctx, w, model.USD, big)
		require.NoError(t, err)
		_, err = r.Credit(ctx, w, model.USD, big)
		require.NoError(t, err)

		_, err = r.ReleaseHold(ctx, h.ID)
		assert.ErrorIs(t, err, ErrInvalidAmount)

		b, err := r.SettleHold(ctx, h.ID)
		require.NoError(t, err)
		assertBalance(t, b, "90000000000000000000", "0")
		assertInvariants(t, pool, r, w)
	})
}

// a wallet whose only funds are held lists them, and nothing else changes
func TestOnlyHeldFunds(t *testing.T) {
	models(t, func(t *testing.T, _ *pgxpool.Pool, r WalletRepo) {
		ctx := context.Background()
		w := newWalletID()

		_, err := r.Credit(ctx, w, model.USDT, dec("5"))
		require.NoError(t, err)
		_, err = r.Hold(ctx, w, model.USDT, dec("5"))
		require.NoError(t, err)

		bs, err := r.ListBalances(ctx, w)
		require.NoError(t, err)
		for _, b := range bs {
			if b.Currency == model.USDT {
				assertBalance(t, b, "0", "5")
				assert.True(t, b.Total().Equal(dec("5")))
			} else {
				assertBalance(t, b, "0", "0")
			}
		}
	})
}
