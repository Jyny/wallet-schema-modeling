package repo

import (
	"context"
	"math/rand/v2"
	"testing"
	"walletmodeling/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// the same random operations applied to both models yield the same balances
func TestModelsEquivalent(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	account, utxo := NewAccountWalletRepo(pool), NewUTXOWalletRepo(pool)
	w := newWalletID()
	rng := rand.New(rand.NewPCG(1, 2))

	scale := map[model.Currency]int32{model.USD: 2, model.USDT: 6, model.BTC: 8, model.ETH: 18}
	currencies := []model.Currency{model.USD, model.USDT, model.BTC, model.ETH}
	type holdPair struct{ account, utxo uuid.UUID }
	var holds []holdPair

	for step := range 300 {
		cur := currencies[rng.IntN(len(currencies))]
		amount := decimal.New(rng.Int64N(1_000_000)+1, -scale[cur])
		var errA, errU error
		switch op := rng.IntN(5); {
		case op <= 1:
			_, errA = account.Credit(ctx, w, cur, amount)
			_, errU = utxo.Credit(ctx, w, cur, amount)
		case op == 2:
			_, errA = account.Debit(ctx, w, cur, amount)
			_, errU = utxo.Debit(ctx, w, cur, amount)
		case op == 3:
			var ha, hu model.Hold
			ha, errA = account.Hold(ctx, w, cur, amount)
			hu, errU = utxo.Hold(ctx, w, cur, amount)
			if errA == nil && errU == nil {
				holds = append(holds, holdPair{ha.ID, hu.ID})
			}
		case len(holds) > 0:
			i := rng.IntN(len(holds))
			h := holds[i]
			holds = append(holds[:i], holds[i+1:]...)
			if rng.IntN(2) == 0 {
				_, errA = account.ReleaseHold(ctx, h.account)
				_, errU = utxo.ReleaseHold(ctx, h.utxo)
			} else {
				_, errA = account.SettleHold(ctx, h.account)
				_, errU = utxo.SettleHold(ctx, h.utxo)
			}
		}
		require.Equal(t, errA == nil, errU == nil, "step %d: account %v, utxo %v", step, errA, errU)
		if errA != nil {
			assert.ErrorIs(t, errA, ErrInsufficientFunds)
			assert.ErrorIs(t, errU, ErrInsufficientFunds)
		}

		ba, err := account.ListBalances(ctx, w)
		require.NoError(t, err)
		bu, err := utxo.ListBalances(ctx, w)
		require.NoError(t, err)
		for i := range ba {
			require.True(t, ba[i].Available.Equal(bu[i].Available), "step %d %s available: account %s, utxo %s", step, ba[i].Currency, ba[i].Available, bu[i].Available)
			require.True(t, ba[i].Held.Equal(bu[i].Held), "step %d %s held: account %s, utxo %s", step, ba[i].Currency, ba[i].Held, bu[i].Held)
		}
	}
	assertAccountInvariants(t, pool, w)
	assertUTXOInvariants(t, pool, w)
}

func TestAccountEntries(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	r := NewAccountWalletRepo(pool)
	w := newWalletID()

	_, err := r.Credit(ctx, w, model.USD, dec("100"))
	require.NoError(t, err)
	_, err = r.Debit(ctx, w, model.USD, dec("10"))
	require.NoError(t, err)
	h, err := r.Hold(ctx, w, model.USD, dec("30"))
	require.NoError(t, err)
	_, err = r.SettleHold(ctx, h.ID)
	require.NoError(t, err)
	// a failed operation leaves no trace
	_, err = r.Debit(ctx, w, model.USD, dec("1000"))
	require.ErrorIs(t, err, ErrInsufficientFunds)

	var kinds []string
	rows, err := pool.Query(ctx, `SELECT kind FROM account_entry WHERE wallet_id = $1 ORDER BY entry_id`, w)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		kinds = append(kinds, k)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"credit", "debit", "hold", "settle"}, kinds)

	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM account_hold WHERE hold_id = $1`, h.ID).Scan(&status))
	assert.Equal(t, "settled", status)

	assertAccountInvariants(t, pool, w)
}

func TestUTXOOutputs(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	r := NewUTXOWalletRepo(pool)
	w := newWalletID()

	for _, a := range []string{"0.5", "0.25", "1"} {
		_, err := r.Credit(ctx, w, model.BTC, dec(a))
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"0.5", "0.25", "1"}, unspentOutputs(t, pool, w))

	// oldest first: spends 0.5 + 0.25, returns 0.15 change
	_, err := r.Debit(ctx, w, model.BTC, dec("0.6"))
	require.NoError(t, err)
	assert.Equal(t, []string{"1", "0.15"}, unspentOutputs(t, pool, w))

	// spends 1, returns 0.6 change, locks 0.4 in one held output
	h, err := r.Hold(ctx, w, model.BTC, dec("0.4"))
	require.NoError(t, err)
	assert.Equal(t, []string{"0.15", "0.6", "0.4 held"}, unspentOutputs(t, pool, w))

	// release recreates the held output as available
	_, err = r.ReleaseHold(ctx, h.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"0.15", "0.6", "0.4"}, unspentOutputs(t, pool, w))

	var spent int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM utxo WHERE wallet_id = $1 AND spent_at IS NOT NULL`, w).Scan(&spent))
	assert.Equal(t, 4, spent)

	assertUTXOInvariants(t, pool, w)
}

// unspentOutputs lists the wallet's unspent BTC outputs in creation order
func unspentOutputs(t *testing.T, pool *pgxpool.Pool, walletID int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT amount, hold_id IS NOT NULL FROM utxo
		WHERE wallet_id = $1 AND currency = 'BTC' AND spent_at IS NULL
		ORDER BY utxo_id`, walletID)
	require.NoError(t, err)
	var outs []string
	for rows.Next() {
		var amount decimal.Decimal
		var held bool
		require.NoError(t, rows.Scan(&amount, &held))
		s := amount.String()
		if held {
			s += " held"
		}
		outs = append(outs, s)
	}
	require.NoError(t, rows.Err())
	return outs
}

// the account row equals the sum of its entries, and held equals its active holds
func assertAccountInvariants(t *testing.T, pool *pgxpool.Pool, walletID int64) {
	t.Helper()
	var bad int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM account AS b
		WHERE b.wallet_id = $1 AND (
			b.available <> (SELECT COALESCE(SUM(available_delta), 0) FROM account_entry AS j
				WHERE j.wallet_id = b.wallet_id AND j.currency = b.currency)
			OR b.held <> (SELECT COALESCE(SUM(held_delta), 0) FROM account_entry AS j
				WHERE j.wallet_id = b.wallet_id AND j.currency = b.currency)
			OR b.held <> (SELECT COALESCE(SUM(amount), 0) FROM account_hold AS h
				WHERE h.wallet_id = b.wallet_id AND h.currency = b.currency AND h.status = 'active')
		)`, walletID).Scan(&bad))
	assert.Zero(t, bad, "accounts disagreeing with their entries or holds")
}

// every tx conserves value: inputs spent = outputs created + amount debited
// or settled out of the wallet
func assertUTXOInvariants(t *testing.T, pool *pgxpool.Pool, walletID int64) {
	t.Helper()
	var bad int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM utxo_tx AS tx
		WHERE tx.wallet_id = $1
		AND COALESCE((SELECT SUM(amount) FROM utxo WHERE spent_tx = tx.tx_id), 0)
			+ CASE WHEN tx.kind = 'credit' THEN tx.amount ELSE 0 END
		<> COALESCE((SELECT SUM(amount) FROM utxo WHERE created_tx = tx.tx_id), 0)
			+ CASE WHEN tx.kind IN ('debit', 'settle') THEN tx.amount ELSE 0 END`,
		walletID).Scan(&bad))
	assert.Zero(t, bad, "utxo txs not conserving value")
}
