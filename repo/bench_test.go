package repo

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// common business scenarios under high concurrency, the same load against
// both models: `make go.bench`. every benchmark runs benchWorkers goroutines
// over the shared pool and reports, besides ns/op (wall time per operation
// across all workers), ops/s, the p50 and p99 latency of one operation and
// the share of operations the utxo model gave up on with ErrContended.

// benchWorkers is the number of concurrent clients, BENCH_WORKERS overrides it
var benchWorkers = envInts("BENCH_WORKERS", 64)[0]

// envInts reads a comma separated list of integers from the environment.
func envInts(name string, def ...int) []int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	var ints []int
	for _, s := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			panic(fmt.Sprintf("%s: %q is not a list of positive integers", name, v))
		}
		ints = append(ints, n)
	}
	return ints
}

// benchOp is one business operation by one client.
type benchOp func(ctx context.Context, rng *rand.Rand) error

// warmupOps is how many untimed operations each worker runs before a
// measurement: enough to open every pool connection and prepare its
// statements, which would otherwise land in the first measured run
const warmupOps = 4

// load runs op b.N times from benchWorkers goroutines.
func load(b *testing.B, op benchOp) {
	b.Helper()
	ctx := context.Background()
	var next, contended atomic.Int64
	latencies := make([][]time.Duration, benchWorkers)
	var wg sync.WaitGroup

	for w := range benchWorkers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 0))
			for range warmupOps {
				if err := op(ctx, rng); err != nil && !errors.Is(err, ErrContended) {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	b.ResetTimer()
	start := time.Now()
	for w := range benchWorkers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), uint64(b.N)))
			for next.Add(1) <= int64(b.N) {
				began := time.Now()
				err := op(ctx, rng)
				latencies[w] = append(latencies[w], time.Since(began))
				switch {
				case err == nil:
				case errors.Is(err, ErrContended):
					contended.Add(1)
				default:
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	b.StopTimer()

	all := slices.Concat(latencies...)
	slices.Sort(all)
	percentile := func(p float64) float64 {
		return float64(all[int(float64(len(all)-1)*p)]) / float64(time.Millisecond)
	}
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/s")
	b.ReportMetric(percentile(0.50), "p50-ms")
	b.ReportMetric(percentile(0.99), "p99-ms")
	b.ReportMetric(float64(contended.Load())/float64(b.N), "contended/op")
}

var one = decimal.NewFromInt(1)

// the scenarios pick wallets uniformly from 1..wallets

func randomWallet(rng *rand.Rand, wallets int) int64 {
	return 1 + rng.Int64N(int64(wallets))
}

// balance inquiry
func opBalance(r WalletRepo, wallets int) benchOp {
	return func(ctx context.Context, rng *rand.Rand) error {
		_, err := r.GetBalance(ctx, randomWallet(rng, wallets), model.USD)
		return err
	}
}

// deposit
func opDeposit(r WalletRepo, wallets int) benchOp {
	return func(ctx context.Context, rng *rand.Rand) error {
		_, err := r.Credit(ctx, randomWallet(rng, wallets), model.USD, one)
		return err
	}
}

// withdrawal
func opWithdraw(r WalletRepo, wallets int) benchOp {
	return func(ctx context.Context, rng *rand.Rand) error {
		_, err := r.Debit(ctx, randomWallet(rng, wallets), model.USD, one)
		return err
	}
}

// payment: hold, then capture by settling the hold
func opPayment(r WalletRepo, wallets int) benchOp {
	return func(ctx context.Context, rng *rand.Rand) error {
		hold, err := r.Hold(ctx, randomWallet(rng, wallets), model.USD, one)
		if err != nil {
			return err
		}
		_, err = r.SettleHold(ctx, hold.ID)
		return err
	}
}

// cancelled payment: hold, then release
func opHoldRelease(r WalletRepo, wallets int) benchOp {
	return func(ctx context.Context, rng *rand.Rand) error {
		hold, err := r.Hold(ctx, randomWallet(rng, wallets), model.USD, one)
		if err != nil {
			return err
		}
		_, err = r.ReleaseHold(ctx, hold.ID)
		return err
	}
}

// transfer between two random wallets
func opTransfer(r WalletRepo, wallets int) benchOp {
	return func(ctx context.Context, rng *rand.Rand) error {
		from := randomWallet(rng, wallets)
		to := 1 + (from+rng.Int64N(int64(wallets)-1))%int64(wallets)
		return transfer(ctx, r, from, to, model.USD, one)
	}
}

// a read-heavy mix of the above: 50% balance, 20% payment, 15% transfer, 10%
// deposit, 5% withdraw
func opMixed(r WalletRepo, wallets int) benchOp {
	balance, payment, xfer := opBalance(r, wallets), opPayment(r, wallets), opTransfer(r, wallets)
	deposit, withdraw := opDeposit(r, wallets), opWithdraw(r, wallets)
	return func(ctx context.Context, rng *rand.Rand) error {
		switch n := rng.IntN(100); {
		case n < 50:
			return balance(ctx, rng)
		case n < 70:
			return payment(ctx, rng)
		case n < 85:
			return xfer(ctx, rng)
		case n < 95:
			return deposit(ctx, rng)
		default:
			return withdraw(ctx, rng)
		}
	}
}

// benchSeed is the data every scenario starts from: wallets holding 4
// outputs, each large enough that no run spends one down
func benchSeed(wallets int) seedSpec {
	return seedSpec{wallets: wallets, cur: model.USD, outputs: 4, amount: "1000000000"}
}

// reseeder returns a function replacing the data of both models with s the
// first time it is called: the sub-benchmarks sharing it, one per model, run
// on the same seed, their own tables apart, and a level that -bench filters
// out is never loaded.
func reseeder(s seedSpec) func(b *testing.B) {
	seeded := false
	return func(b *testing.B) {
		b.Helper()
		if seeded {
			return
		}
		cleanDB(b)
		seed(b, s)
		seeded = true
	}
}

// benchRepos is repos in random order: within a round both models run on the
// same seed one after the other, so a fixed order would hand one of them
// whatever drifts over the round, such as autovacuum or checkpoints. across
// rounds the order evens out.
func benchRepos() []namedRepo {
	r := repos()
	rand.Shuffle(len(r), func(i, j int) { r[i], r[j] = r[j], r[i] })
	return r
}

// each business scenario with the load spread over 1000 wallets
func BenchmarkScenario(b *testing.B) {
	const wallets = 1000
	scenarios := []struct {
		name string
		op   func(r WalletRepo, wallets int) benchOp
	}{
		{"balance", opBalance},
		{"deposit", opDeposit},
		{"withdraw", opWithdraw},
		{"payment", opPayment},
		{"hold-release", opHoldRelease},
		{"transfer", opTransfer},
		{"mixed", opMixed},
	}
	setup(b)
	for _, s := range scenarios {
		reseed := reseeder(benchSeed(wallets))
		for _, impl := range benchRepos() {
			b.Run("scenario="+s.name+"/model="+impl.name, func(b *testing.B) {
				reseed(b)
				load(b, s.op(impl.repo, wallets))
			})
		}
	}
}

// every client pays from the same wallet, a merchant or treasury account: the
// account model queues on its row, the utxo model on how many outputs there
// are to pick from
func BenchmarkHotWallet(b *testing.B) {
	setup(b)
	for _, outputs := range []int{1, 64, 1024} {
		reseed := reseeder(seedSpec{wallets: 1, cur: model.USD, outputs: outputs, amount: "1000000000"})
		for _, impl := range benchRepos() {
			b.Run(fmt.Sprintf("outputs=%d/model=%s", outputs, impl.name), func(b *testing.B) {
				reseed(b)
				load(b, opPayment(impl.repo, 1))
			})
		}
	}
}

// cardinality growth: the mixed scenario, same concurrency, on ever more
// data: `make go.bench.scale`. a model scales when its numbers stay
// flat from one level to the next.

func benchCardinality(b *testing.B, label string, levels []int, spec func(level int) seedSpec) {
	setup(b)
	for _, level := range levels {
		s := spec(level)
		reseed := reseeder(s)
		for _, impl := range benchRepos() {
			b.Run(fmt.Sprintf("%s=%d/model=%s", label, level, impl.name), func(b *testing.B) {
				reseed(b)
				load(b, opMixed(impl.repo, s.wallets))
				reportRows(b)
			})
		}
	}
}

// reportRows reports how many rows the wallet tables of both models hold, in
// millions, as the benchmark ends.
func reportRows(b *testing.B) {
	b.Helper()
	var rows int64
	err := testPool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM account) + (SELECT count(*) FROM account_hold)
			+ (SELECT count(*) FROM account_entry)
			+ (SELECT count(*) FROM utxo) + (SELECT count(*) FROM utxo_tx)`).Scan(&rows)
	require.NoError(b, err)
	b.ReportMetric(float64(rows)/1e6, "Mrows")
}

// more wallets: CARDINALITY_WALLETS overrides the levels
func BenchmarkCardinalityWallets(b *testing.B) {
	levels := envInts("CARDINALITY_WALLETS", 1_000, 10_000, 100_000, 1_000_000)
	benchCardinality(b, "wallets", levels, benchSeed)
}

// more unspent outputs per wallet, the fragmentation the utxo model reads and
// selects from: CARDINALITY_OUTPUTS overrides the levels
func BenchmarkCardinalityOutputs(b *testing.B) {
	levels := envInts("CARDINALITY_OUTPUTS", 1, 10, 100, 1_000)
	benchCardinality(b, "outputs", levels, func(outputs int) seedSpec {
		s := benchSeed(1000)
		s.outputs = outputs
		return s
	})
}

// more closed history per wallet, entries and spent outputs no balance
// depends on any more: CARDINALITY_HISTORY overrides the levels
func BenchmarkCardinalityHistory(b *testing.B) {
	levels := envInts("CARDINALITY_HISTORY", 1, 100, 1_000)
	benchCardinality(b, "history", levels, func(history int) seedSpec {
		s := benchSeed(1000)
		s.history = history
		return s
	})
}
