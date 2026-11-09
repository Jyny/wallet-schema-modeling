package repo

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
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
	stats   *connStats
}

func (c *slowConn) Write(b []byte) (int, error) {
	start := time.Now()
	time.Sleep(c.latency)
	if c.stats != nil {
		c.stats.slept.Add(int64(time.Since(start)))
	}
	return c.Conn.Write(b)
}

// connStats tells where the time of operations on a latencyPool goes, in
// nanoseconds summed over its connections: slept is the latency injected into
// round trips, inStmt the time inside statements, BEGIN and COMMIT included,
// from the call until the result is read. inStmt - slept is the time spent in
// the database: executing, and waiting for locks
type connStats struct {
	slept, inStmt atomic.Int64
}

func (s *connStats) reset() {
	s.slept.Store(0)
	s.inStmt.Store(0)
}

type stmtStartKey struct{}

func (s *connStats) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, stmtStartKey{}, time.Now())
}

func (s *connStats) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if start, ok := ctx.Value(stmtStartKey{}).(time.Time); ok {
		s.inStmt.Add(int64(time.Since(start)))
	}
}

// setupLatencyDB is setupTestDB with latency added to every round trip and
// a pool of maxConns connections
func setupLatencyDB(t testing.TB, latency time.Duration, maxConns int32) *pgxpool.Pool {
	t.Helper()
	pool := latencyPool(t, setupTestDB(t).Config(), latency, maxConns, nil)
	t.Cleanup(pool.Close)
	return pool
}

// latencyPool opens a pool of maxConns connections from cfg, adding latency
// to every round trip and, when stats is not nil, recording where the time
// goes. the caller closes it
func latencyPool(t testing.TB, cfg *pgxpool.Config, latency time.Duration, maxConns int32, stats *connStats) *pgxpool.Pool {
	t.Helper()
	cfg.MaxConns = maxConns
	if stats != nil {
		cfg.ConnConfig.Tracer = stats
	}
	if latency > 0 {
		cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := new(net.Dialer).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &slowConn{Conn: c, latency: latency, stats: stats}, nil
		}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	return pool
}

// hotDebits funds a wallet with outputs credits on the undelayed pool, then
// runs workers x debits concurrent debits on slow and returns their wall time
func hotDebits(t *testing.T, pool, slow *pgxpool.Pool, newRepo func(*pgxpool.Pool) WalletRepo, workers, debits, outputs int) time.Duration {
	t.Helper()
	ctx := context.Background()
	w := fundedWallet(t, newRepo(pool), outputs, "1000")
	warm(t, slow, workers)
	r := newRepo(slow)

	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Go(func() {
			for range debits {
				_, err := r.Debit(ctx, w, model.USD, dec("1"))
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)

	b, err := r.GetBalance(ctx, w, model.USD)
	require.NoError(t, err)
	want := decimal.NewFromInt(int64(outputs*1000 - workers*debits))
	require.True(t, b.Available.Equal(want), "available: want %s, got %s", want, b.Available)
	assertInvariants(t, pool, r, w)
	return elapsed
}

func newAccountRepo(p *pgxpool.Pool) WalletRepo { return NewAccountWalletRepo(p) }
func newUTXORepo(p *pgxpool.Pool) WalletRepo    { return NewUTXOWalletRepo(p) }

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
	pool := setupTestDB(t)
	slow := setupLatencyDB(t, latency, workers)

	var account, utxo time.Duration
	t.Run("account", func(t *testing.T) { account = hotDebits(t, pool, slow, newAccountRepo, workers, debits, outputs) })
	t.Run("utxo", func(t *testing.T) { utxo = hotDebits(t, pool, slow, newUTXORepo, workers, debits, outputs) })
	t.Logf("%d debits by %d workers, %s per round trip: account %s, utxo %s, utxo / account %.2fx",
		workers*debits, workers, latency, account, utxo, float64(utxo)/float64(account))
	require.Less(t, utxo, account, "utxo model should outpace the account model on a hot account")
}

// how far the gap goes: with enough workers the account model is capped by
// its lock span, which grows with the rtt, while the utxo model is capped by
// the server or by workers / time of one op. so the speedup grows with the
// rtt, toward about workers x (account lock round trips / utxo op round trips),
// ~N/4 here. 64 workers measure ~8x at 1ms and ~17x at 5ms; the bounds below
// leave room for a slower machine
func TestHotAccountPeakSpeedup(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const (
		workers = 64
		debits  = 5 // per worker
		outputs = 128
	)
	pool := setupTestDB(t)

	speedup := map[time.Duration]float64{}
	for _, latency := range []time.Duration{time.Millisecond, 5 * time.Millisecond} {
		t.Run(fmt.Sprintf("rtt=%s", latency), func(t *testing.T) {
			slow := setupLatencyDB(t, latency, workers)
			account := hotDebits(t, pool, slow, newAccountRepo, workers, debits, outputs)
			utxo := hotDebits(t, pool, slow, newUTXORepo, workers, debits, outputs)
			speedup[latency] = float64(account) / float64(utxo)
			t.Logf("%d debits by %d workers: account %s, utxo %s, utxo %.1fx faster",
				workers*debits, workers, account, utxo, speedup[latency])
		})
	}
	require.Greater(t, speedup[time.Millisecond], 3.0, "1ms rtt")
	require.Greater(t, speedup[5*time.Millisecond], 8.0, "5ms rtt")
	require.Greater(t, speedup[5*time.Millisecond], speedup[time.Millisecond], "the gap should grow with the rtt")
}
