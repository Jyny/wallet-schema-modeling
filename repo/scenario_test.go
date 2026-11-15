package repo

import (
	"context"
	"math/rand/v2"
	"testing"
	"walletmodeling/model"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// business scenarios, each run against both models

func TestNewWalletReadsZero(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		assertBalance(t, r, 1, model.USD, "0", "0")

		balances, err := r.ListBalances(context.Background(), 1)
		require.NoError(t, err)
		var currencies []model.Currency
		for _, b := range balances {
			currencies = append(currencies, b.Currency)
			assert.True(t, b.Total().IsZero(), "%s: %s", b.Currency, b.Total())
		}
		assert.Equal(t, []model.Currency{model.BTC, model.ETH, model.USD, model.USDT}, currencies)
	})
}

func TestDepositAndWithdraw(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		bal, err := r.Credit(ctx, 1, model.USD, dec("100.00"))
		require.NoError(t, err)
		assert.True(t, bal.Available.Equal(dec("100")), "credit returns the new balance, got %s", bal.Available)
		fund(t, r, 1, model.USD, "50.50")

		// spans both deposits: the utxo model spends two outputs and keeps the change
		bal, err = r.Debit(ctx, 1, model.USD, dec("120.25"))
		require.NoError(t, err)
		assert.True(t, bal.Available.Equal(dec("30.25")), "debit returns the new balance, got %s", bal.Available)

		_, err = r.Debit(ctx, 1, model.USD, dec("30.26"))
		require.ErrorIs(t, err, ErrInsufficientFunds)
		assertBalance(t, r, 1, model.USD, "30.25", "0")

		_, err = r.Debit(ctx, 1, model.USD, dec("30.25"))
		require.NoError(t, err)
		assertBalance(t, r, 1, model.USD, "0", "0")
	})
}

// hold then capture: the held amount leaves the wallet
func TestHoldThenSettle(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")

		hold, err := r.Hold(ctx, 1, model.USD, dec("30"))
		require.NoError(t, err)
		assert.Equal(t, model.Hold{ID: hold.ID, WalletID: 1, Currency: model.USD, Amount: dec("30")}, hold)
		assertBalance(t, r, 1, model.USD, "70", "30")

		bal, err := r.SettleHold(ctx, hold.ID)
		require.NoError(t, err)
		assert.True(t, bal.Total().Equal(dec("70")), "total after settle: %s", bal.Total())
		assertBalance(t, r, 1, model.USD, "70", "0")
	})
}

// hold then cancel: the held amount returns to available
func TestHoldThenRelease(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")

		hold, err := r.Hold(ctx, 1, model.USD, dec("30"))
		require.NoError(t, err)
		_, err = r.ReleaseHold(ctx, hold.ID)
		require.NoError(t, err)
		assertBalance(t, r, 1, model.USD, "100", "0")

		// released funds are spendable again, in full
		_, err = r.Debit(ctx, 1, model.USD, dec("100"))
		require.NoError(t, err)
	})
}

func TestHeldFundsAreNotSpendable(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")
		_, err := r.Hold(ctx, 1, model.USD, dec("80"))
		require.NoError(t, err)

		_, err = r.Debit(ctx, 1, model.USD, dec("20.01"))
		require.ErrorIs(t, err, ErrInsufficientFunds)
		_, err = r.Hold(ctx, 1, model.USD, dec("20.01"))
		require.ErrorIs(t, err, ErrInsufficientFunds)
		assertBalance(t, r, 1, model.USD, "20", "80")

		_, err = r.Hold(ctx, 1, model.USD, dec("20"))
		require.NoError(t, err)
		assertBalance(t, r, 1, model.USD, "0", "100")
	})
}

func TestSeveralHoldsCloseIndependently(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")
		var holds []model.Hold
		for _, amount := range []string{"10", "20", "30"} {
			h, err := r.Hold(ctx, 1, model.USD, dec(amount))
			require.NoError(t, err)
			holds = append(holds, h)
		}
		assertBalance(t, r, 1, model.USD, "40", "60")

		_, err := r.SettleHold(ctx, holds[1].ID)
		require.NoError(t, err)
		assertBalance(t, r, 1, model.USD, "40", "40")
		_, err = r.ReleaseHold(ctx, holds[0].ID)
		require.NoError(t, err)
		assertBalance(t, r, 1, model.USD, "50", "30")
		_, err = r.SettleHold(ctx, holds[2].ID)
		require.NoError(t, err)
		assertBalance(t, r, 1, model.USD, "50", "0")
	})
}

func TestHoldClosesOnlyOnce(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")
		settled, err := r.Hold(ctx, 1, model.USD, dec("10"))
		require.NoError(t, err)
		released, err := r.Hold(ctx, 1, model.USD, dec("20"))
		require.NoError(t, err)
		_, err = r.SettleHold(ctx, settled.ID)
		require.NoError(t, err)
		_, err = r.ReleaseHold(ctx, released.ID)
		require.NoError(t, err)

		for _, id := range []uuid.UUID{settled.ID, released.ID, uuid.New()} {
			_, err = r.SettleHold(ctx, id)
			require.ErrorIs(t, err, ErrHoldNotFound)
			_, err = r.ReleaseHold(ctx, id)
			require.ErrorIs(t, err, ErrHoldNotFound)
		}
		assertBalance(t, r, 1, model.USD, "90", "0")
	})
}

func TestTransfer(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")

		require.NoError(t, transfer(ctx, r, 1, 2, model.USD, dec("40")))
		assertBalance(t, r, 1, model.USD, "60", "0")
		assertBalance(t, r, 2, model.USD, "40", "0")

		// the receiver can pass it on
		require.NoError(t, transfer(ctx, r, 2, 3, model.USD, dec("40")))
		assertBalance(t, r, 2, model.USD, "0", "0")
		assertBalance(t, r, 3, model.USD, "40", "0")
	})
}

func TestTransferInsufficientFundsChangesNothing(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		fund(t, r, 1, model.USD, "100")

		err := transfer(context.Background(), r, 1, 2, model.USD, dec("100.01"))
		require.ErrorIs(t, err, ErrInsufficientFunds)
		assertBalance(t, r, 1, model.USD, "100", "0")
		assertBalance(t, r, 2, model.USD, "0", "0")
	})
}

// the receiver cannot take the amount (its balance would pass the bound), so
// the sender's hold is released
func TestTransferRejectedByReceiverIsReleased(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		fund(t, r, 1, model.USD, "100")
		fund(t, r, 2, model.USD, "99999999999999999999.99")

		err := transfer(context.Background(), r, 1, 2, model.USD, dec("0.01"))
		require.ErrorIs(t, err, ErrInvalidAmount)
		assertBalance(t, r, 1, model.USD, "100", "0")
		assertBalance(t, r, 2, model.USD, "99999999999999999999.99", "0")
	})
}

func TestCurrenciesAreIndependent(t *testing.T) {
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")
		fund(t, r, 1, model.BTC, "0.5")
		// one wei, the smallest amount any currency carries
		fund(t, r, 1, model.ETH, "0.000000000000000001")

		_, err := r.Debit(ctx, 1, model.BTC, dec("0.50000001"))
		require.ErrorIs(t, err, ErrInsufficientFunds, "usd must not cover a btc debit")
		_, err = r.Hold(ctx, 1, model.BTC, dec("0.2"))
		require.NoError(t, err)
		_, err = r.Debit(ctx, 1, model.USD, dec("99.99"))
		require.NoError(t, err)

		balances, err := r.ListBalances(ctx, 1)
		require.NoError(t, err)
		want := map[model.Currency][2]string{
			model.BTC:  {"0.3", "0.2"},
			model.ETH:  {"0.000000000000000001", "0"},
			model.USD:  {"0.01", "0"},
			model.USDT: {"0", "0"},
		}
		require.Len(t, balances, len(want))
		for _, b := range balances {
			assert.True(t, b.Available.Equal(dec(want[b.Currency][0])), "%s available: %s", b.Currency, b.Available)
			assert.True(t, b.Held.Equal(dec(want[b.Currency][1])), "%s held: %s", b.Currency, b.Held)
		}
		assertBalance(t, r, 2, model.USD, "0", "0")
	})
}

func TestInvalidRequestsChangeNothing(t *testing.T) {
	cases := []struct {
		name   string
		cur    model.Currency
		amount string
		want   error
	}{
		{"zero", model.USD, "0", ErrInvalidAmount},
		{"negative", model.USD, "-1", ErrInvalidAmount},
		{"finer than the currency scale", model.USD, "0.001", ErrInvalidAmount},
		{"finer than satoshi", model.BTC, "0.000000001", ErrInvalidAmount},
		{"at the bound", model.USD, "100000000000000000000", ErrInvalidAmount},
		{"unknown currency", "XXX", "1", ErrUnknownCurrency},
		{"malformed currency", "usd", "1", ErrUnknownCurrency},
	}
	models(t, func(t *testing.T, r WalletRepo) {
		ctx := context.Background()
		fund(t, r, 1, model.USD, "100")
		fund(t, r, 1, model.BTC, "1")
		for _, c := range cases {
			_, err := r.Credit(ctx, 1, c.cur, dec(c.amount))
			assert.ErrorIs(t, err, c.want, "credit %s", c.name)
			_, err = r.Debit(ctx, 1, c.cur, dec(c.amount))
			assert.ErrorIs(t, err, c.want, "debit %s", c.name)
			_, err = r.Hold(ctx, 1, c.cur, dec(c.amount))
			assert.ErrorIs(t, err, c.want, "hold %s", c.name)
		}
		_, err := r.GetBalance(ctx, 1, "XXX")
		assert.ErrorIs(t, err, ErrUnknownCurrency)
		assertBalance(t, r, 1, model.USD, "100", "0")
		assertBalance(t, r, 1, model.BTC, "1", "0")
	})
}

// the same random operations applied to both models must succeed or fail
// alike and leave equal balances after every step
func TestModelsAgree(t *testing.T) {
	setup(t)
	ctx := context.Background()
	impls := repos()
	rng := rand.New(rand.NewPCG(1, 2))
	scales := map[model.Currency]int32{model.USD: 2, model.BTC: 8}
	currencies := []model.Currency{model.USD, model.BTC}
	// open holds, the same one in each model
	var open [][]uuid.UUID

	for step := range 400 {
		wallet := 1 + rng.Int64N(2)
		cur := currencies[rng.IntN(len(currencies))]
		amount := decimal.New(1+rng.Int64N(5000), -scales[cur])
		kind := rng.IntN(5)
		closing := rng.IntN(max(len(open), 1))

		errs := make([]error, len(impls))
		holds := make([]uuid.UUID, len(impls))
		for i, impl := range impls {
			switch {
			case kind == 0:
				// credits weigh more than debits so that most spends succeed
				_, errs[i] = impl.repo.Credit(ctx, wallet, cur, amount.Mul(decimal.NewFromInt(3)))
			case kind == 1:
				_, errs[i] = impl.repo.Debit(ctx, wallet, cur, amount)
			case kind == 2:
				var h model.Hold
				h, errs[i] = impl.repo.Hold(ctx, wallet, cur, amount)
				holds[i] = h.ID
			case len(open) == 0:
			case kind == 3:
				_, errs[i] = impl.repo.ReleaseHold(ctx, open[closing][i])
			default:
				_, errs[i] = impl.repo.SettleHold(ctx, open[closing][i])
			}
		}
		require.Equal(t, errs[0] == nil, errs[1] == nil, "step %d: account %v, utxo %v", step, errs[0], errs[1])
		if errs[0] != nil {
			require.ErrorIs(t, errs[0], ErrInsufficientFunds, "step %d", step)
			require.ErrorIs(t, errs[1], ErrInsufficientFunds, "step %d", step)
		}
		switch {
		case kind == 2 && errs[0] == nil:
			open = append(open, holds)
		case kind > 2 && len(open) > 0:
			open = append(open[:closing], open[closing+1:]...)
		}

		for w := int64(1); w <= 2; w++ {
			a, err := impls[0].repo.ListBalances(ctx, w)
			require.NoError(t, err)
			u, err := impls[1].repo.ListBalances(ctx, w)
			require.NoError(t, err)
			require.Len(t, u, len(a))
			for i := range a {
				require.True(t, a[i].Available.Equal(u[i].Available) && a[i].Held.Equal(u[i].Held),
					"step %d wallet %d: account %+v, utxo %+v", step, w, a[i], u[i])
			}
		}
	}
	audit(t)
}
