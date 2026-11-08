package repo

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// the hot account under network latency, where the utxo model is faster.
// the account model holds the row lock from its UPDATE until COMMIT returns,
// across the entry INSERT and the COMMIT round trips, so concurrent writes of
// one account serialize on about 2 round trips each. the utxo model holds
// locks just as long, but on disjoint outputs, so writes overlap. on a local
// database a round trip is ~0.1ms and the gap is small; across a network
// (~1ms between zones) it dominates.

// slowConn delays every write by latency, standing in for a network round
// trip: pgx sends each query, and BEGIN and COMMIT, as one write
type slowConn struct {
	net.Conn
	latency time.Duration
}

func (c *slowConn) Write(b []byte) (int, error) {
	time.Sleep(c.latency)
	return c.Conn.Write(b)
}

// setupLatencyDB is setupTestDB with latency added to every round trip and
// a pool of maxConns connections
func setupLatencyDB(t testing.TB, latency time.Duration, maxConns int32) *pgxpool.Pool {
	t.Helper()
	pool := latencyPool(t, setupTestDB(t).Config(), latency, maxConns)
	t.Cleanup(pool.Close)
	return pool
}

// latencyPool opens a pool of maxConns connections from cfg, adding latency
// to every round trip. the caller closes it
func latencyPool(t testing.TB, cfg *pgxpool.Config, latency time.Duration, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg.MaxConns = maxConns
	if latency > 0 {
		cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := new(net.Dialer).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &slowConn{Conn: c, latency: latency}, nil
		}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	return pool
}

// concurrent debits of one account, funded as many outputs, take less wall
// time in the utxo model than in the account model
func TestHotAccountUnderLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const (
		latency = time.Millisecond
		workers = 16
		debits  = 10 // per worker
		outputs = 64
	)
	pool := setupLatencyDB(t, latency, workers)
	ctx := context.Background()

	run := func(t *testing.T, r WalletRepo) time.Duration {
		w := newWalletID()
		for range outputs {
			_, err := r.Credit(ctx, w, model.USD, dec("1000"))
			require.NoError(t, err)
		}
		// warm every connection so dialing is not timed
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() { require.NoError(t, pool.Ping(ctx)) })
		}
		wg.Wait()

		start := time.Now()
		for range workers {
			wg.Go(func() {
				for range debits {
					_, err := r.Debit(ctx, w, model.USD, dec("1"))
					require.NoError(t, err)
				}
			})
		}
		wg.Wait()
		elapsed := time.Since(start)

		b, err := r.GetBalance(ctx, w, model.USD)
		require.NoError(t, err)
		want := decimal.NewFromInt(outputs*1000 - workers*debits)
		require.True(t, b.Available.Equal(want), "available: want %s, got %s", want, b.Available)
		assertInvariants(t, pool, r, w)
		t.Logf("%d debits by %d workers, %s per round trip: %s, %s per debit",
			workers*debits, workers, latency, elapsed, elapsed/(workers*debits))
		return elapsed
	}

	var account, utxo time.Duration
	t.Run("account", func(t *testing.T) { account = run(t, NewAccountWalletRepo(pool)) })
	t.Run("utxo", func(t *testing.T) { utxo = run(t, NewUTXOWalletRepo(pool)) })
	t.Logf("utxo / account: %.2fx", float64(utxo)/float64(account))
	require.Less(t, utxo, account, "utxo model should outpace the account model on a hot account")
}
