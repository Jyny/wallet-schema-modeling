package repo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// benchmarks of the same workloads against both models: `make go.bench`

type benchRepo struct {
	name string
	repo WalletRepo
}

func benchRepos(b *testing.B) (*pgxpool.Pool, []benchRepo) {
	pool := setupTestDB(b)
	return pool, []benchRepo{
		{"account", NewAccountWalletRepo(pool)},
		{"utxo", NewUTXOWalletRepo(pool)},
	}
}

// fundedWallet is a fresh wallet holding outputs credits of amount each
func fundedWallet(b *testing.B, r WalletRepo, outputs int, amount string) int64 {
	b.Helper()
	ctx := context.Background()
	w := newWalletID()
	for range outputs {
		_, err := r.Credit(ctx, w, model.USD, dec(amount))
		require.NoError(b, err)
	}
	return w
}

// reading a balance. the account model reads one row whatever the history,
// the utxo model sums the unspent outputs
func BenchmarkGetBalance(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, outputs := range []int{1, 100, 1000} {
		for _, r := range repos {
			w := fundedWallet(b, r.repo, outputs, "1")
			b.Run(fmt.Sprintf("outputs=%d/%s", outputs, r.name), func(b *testing.B) {
				for b.Loop() {
					_, err := r.repo.GetBalance(ctx, w, model.USD)
					require.NoError(b, err)
				}
			})
		}
	}
}

func BenchmarkListBalances(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, r := range repos {
		w := fundedWallet(b, r.repo, 100, "1")
		b.Run(r.name, func(b *testing.B) {
			for b.Loop() {
				_, err := r.repo.ListBalances(ctx, w)
				require.NoError(b, err)
			}
		})
	}
}

func BenchmarkCredit(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, r := range repos {
		w := newWalletID()
		b.Run(r.name, func(b *testing.B) {
			for b.Loop() {
				_, err := r.repo.Credit(ctx, w, model.USD, dec("1"))
				require.NoError(b, err)
			}
		})
	}
}

// debiting from one large output: the utxo model spends it and creates change
func BenchmarkDebit(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, r := range repos {
		w := fundedWallet(b, r.repo, 1, "1000000000000")
		b.Run(r.name, func(b *testing.B) {
			for b.Loop() {
				_, err := r.repo.Debit(ctx, w, model.USD, dec("1"))
				require.NoError(b, err)
			}
		})
	}
}

// a debit that must consume many small outputs, the utxo model's worst case
func BenchmarkDebitFragmented(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, inputs := range []int{10, 100} {
		for _, r := range repos {
			b.Run(fmt.Sprintf("inputs=%d/%s", inputs, r.name), func(b *testing.B) {
				w := newWalletID()
				amount := decimal.NewFromInt(int64(inputs))
				for b.Loop() {
					b.StopTimer()
					for range inputs {
						_, err := r.repo.Credit(ctx, w, model.USD, dec("1"))
						require.NoError(b, err)
					}
					b.StartTimer()
					_, err := r.repo.Debit(ctx, w, model.USD, amount)
					require.NoError(b, err)
				}
			})
		}
	}
}

// one hold and its release per op
func BenchmarkHoldRelease(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, r := range repos {
		w := fundedWallet(b, r.repo, 1, "1000000000000")
		b.Run(r.name, func(b *testing.B) {
			for b.Loop() {
				h, err := r.repo.Hold(ctx, w, model.USD, dec("1"))
				require.NoError(b, err)
				_, err = r.repo.ReleaseHold(ctx, h.ID)
				require.NoError(b, err)
			}
		})
	}
}

// parallel debits of one account, the hot-row case: the account model
// serializes on its row, the utxo model spends disjoint outputs, so it scales
// with the number of outputs it starts from. failures (ErrContended) are
// reported, not fatal
func BenchmarkParallelDebitSameAccount(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, outputs := range []int{1, 64} {
		for _, r := range repos {
			w := fundedWallet(b, r.repo, outputs, "1000000000")
			b.Run(fmt.Sprintf("outputs=%d/%s", outputs, r.name), func(b *testing.B) {
				var failed atomic.Int64
				b.SetParallelism(2) // goroutines: 2 x GOMAXPROCS
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, err := r.repo.Debit(ctx, w, model.USD, dec("1"))
						if errors.Is(err, ErrContended) {
							failed.Add(1)
							continue
						}
						require.NoError(b, err)
					}
				})
				b.ReportMetric(float64(failed.Load())/float64(b.N), "contended/op")
			})
		}
	}
}

// parallel debits each on its own account: no contention, the baseline
func BenchmarkParallelDebitDistinctAccounts(b *testing.B) {
	_, repos := benchRepos(b)
	ctx := context.Background()
	for _, r := range repos {
		b.Run(r.name, func(b *testing.B) {
			b.SetParallelism(2)
			b.RunParallel(func(pb *testing.PB) {
				w := newWalletID()
				_, err := r.repo.Credit(ctx, w, model.USD, dec("1000000000000"))
				require.NoError(b, err)
				for pb.Next() {
					_, err := r.repo.Debit(ctx, w, model.USD, dec("1"))
					require.NoError(b, err)
				}
			})
		})
	}
}

// hot account benchmarks: parallel operations on one account, where the
// account model serializes on its row and the utxo model does not. each
// sweeps one dimension around a baseline of 1ms per round trip (about a
// cross-zone hop), 16 workers and 64 outputs:
//   - rtt: delay added to every round trip, lengthening the account row lock,
//     held from the UPDATE until COMMIT returns
//   - workers: concurrent operations, each with its own connection
//   - outputs: outputs the wallet is funded with (the account model has one row
//     whatever the funding)
// setup runs on an undelayed pool

type hotCase struct {
	rtt     time.Duration
	workers int
	outputs int
	lock    time.Duration // BenchmarkHotAccountDebitSlowTx only
}

var hotBase = hotCase{rtt: time.Millisecond, workers: 16, outputs: 64}

func (c hotCase) String() string {
	s := fmt.Sprintf("rtt=%s/workers=%d/outputs=%d", c.rtt, c.workers, c.outputs)
	if c.lock > 0 {
		s += fmt.Sprintf("/lock=%s", c.lock)
	}
	return s
}

// sweep returns the baseline varied in each given way, without duplicates
func sweep(base hotCase, vary ...func(hotCase) []hotCase) []hotCase {
	var cases []hotCase
	seen := map[hotCase]bool{}
	for _, v := range vary {
		for _, c := range v(base) {
			if !seen[c] {
				seen[c] = true
				cases = append(cases, c)
			}
		}
	}
	return cases
}

func varyRTT(c hotCase) []hotCase {
	var cs []hotCase
	for _, d := range []time.Duration{0, 500 * time.Microsecond, time.Millisecond, 2 * time.Millisecond} {
		c.rtt = d
		cs = append(cs, c)
	}
	return cs
}

func varyWorkers(c hotCase) []hotCase {
	var cs []hotCase
	for _, n := range []int{1, 4, 16, 64} {
		c.workers = n
		cs = append(cs, c)
	}
	return cs
}

func varyOutputs(c hotCase) []hotCase {
	var cs []hotCase
	for _, n := range []int{1, 4, 16, 64, 256} {
		c.outputs = n
		cs = append(cs, c)
	}
	return cs
}

func varyLock(c hotCase) []hotCase {
	var cs []hotCase
	for _, d := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond} {
		c.lock = d
		cs = append(cs, c)
	}
	return cs
}

// runHot runs op from c.workers goroutines against each model, on a wallet
// funded with c.outputs outputs. prepare, when set, runs after funding with
// an undelayed pool and returns a func to undo it. failures with ErrContended
// are reported, not fatal
func runHot(b *testing.B, cases []hotCase,
	prepare func(pool *pgxpool.Pool, c hotCase, model string, walletID int64) (stop func()),
	op func(r WalletRepo, walletID int64) error,
) {
	pool := setupTestDB(b)
	for _, c := range cases {
		slow := latencyPool(b, pool.Config(), c.rtt, int32(c.workers))
		repos := []hotRepo{
			{"account", NewAccountWalletRepo(pool), NewAccountWalletRepo(slow)},
			{"utxo", NewUTXOWalletRepo(pool), NewUTXOWalletRepo(slow)},
		}
		for _, r := range repos {
			b.Run(fmt.Sprintf("%s/%s", c, r.name), func(b *testing.B) {
				w := newWalletID()
				if c.outputs > 0 {
					w = fundedWallet(b, r.setup, c.outputs, "1000000000")
				}
				if prepare != nil {
					defer prepare(pool, c, r.name, w)()
				}
				warm(b, slow, c.workers)

				var next, failed atomic.Int64
				var wg sync.WaitGroup
				b.ResetTimer()
				for range c.workers {
					wg.Go(func() {
						for next.Add(1) <= int64(b.N) {
							err := op(r.repo, w)
							if errors.Is(err, ErrContended) {
								failed.Add(1)
								continue
							}
							if err != nil {
								b.Error(err)
								return
							}
						}
					})
				}
				wg.Wait()
				b.StopTimer()
				b.ReportMetric(float64(failed.Load())/float64(b.N), "contended/op")
			})
		}
		slow.Close()
	}
}

type hotRepo struct {
	name  string
	setup WalletRepo // undelayed, same database
	repo  WalletRepo
}

// warm opens n connections, so dialing is not timed
func warm(b *testing.B, pool *pgxpool.Pool, n int) {
	ctx := context.Background()
	conns := make([]*pgxpool.Conn, n)
	for i := range conns {
		c, err := pool.Acquire(ctx)
		require.NoError(b, err)
		conns[i] = c
	}
	for _, c := range conns {
		c.Release()
	}
}

// debits of one account
func BenchmarkHotAccountDebit(b *testing.B) {
	ctx := context.Background()
	runHot(b, sweep(hotBase, varyRTT, varyWorkers, varyOutputs), nil, func(r WalletRepo, w int64) error {
		_, err := r.Debit(ctx, w, model.USD, dec("1"))
		return err
	})
}

// credits of one account, e.g. a merchant receiving payments: the account
// model updates its row, the utxo model only inserts an output and takes no
// lock at all. the wallet starts empty
func BenchmarkHotAccountCredit(b *testing.B) {
	ctx := context.Background()
	base := hotBase
	base.outputs = 0
	runHot(b, sweep(base, varyRTT, varyWorkers), nil, func(r WalletRepo, w int64) error {
		_, err := r.Credit(ctx, w, model.USD, dec("1"))
		return err
	})
}

// a hold then its settle, e.g. authorize and capture a payment: the account
// model updates its row twice, the utxo model's settle locks only the held
// output
func BenchmarkHotAccountHoldSettle(b *testing.B) {
	ctx := context.Background()
	runHot(b, sweep(hotBase, varyRTT, varyWorkers), nil, func(r WalletRepo, w int64) error {
		h, err := r.Hold(ctx, w, model.USD, dec("1"))
		if err != nil {
			return err
		}
		_, err = r.SettleHold(ctx, h.ID)
		return err
	})
}

// debits while another session keeps locking the funds, c.lock on and c.lock
// off, e.g. a slow job reading them FOR UPDATE: the account model waits for
// the row, the utxo model spends around the locked output
func BenchmarkHotAccountDebitSlowTx(b *testing.B) {
	ctx := context.Background()
	locks := map[string]string{
		"account": `SELECT 1 FROM account WHERE wallet_id = $1 AND currency = 'USD' FOR UPDATE`,
		"utxo": `SELECT 1 FROM utxo WHERE utxo_id = (
			SELECT utxo_id FROM utxo WHERE wallet_id = $1 AND spent_at IS NULL AND hold_id IS NULL
			ORDER BY utxo_id LIMIT 1
		) FOR UPDATE`,
	}
	base := hotBase
	base.lock = 5 * time.Millisecond
	cases := sweep(base, varyLock, func(c hotCase) []hotCase {
		c.rtt = 0
		return []hotCase{c}
	})
	prepare := func(pool *pgxpool.Pool, c hotCase, model string, w int64) func() {
		lockCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for lockCtx.Err() == nil {
				_ = pgx.BeginFunc(lockCtx, pool, func(tx pgx.Tx) error {
					if _, err := tx.Exec(lockCtx, locks[model], w); err != nil {
						return err
					}
					time.Sleep(c.lock)
					return nil
				})
				time.Sleep(c.lock)
			}
		}()
		return func() { stop(); <-done }
	}
	runHot(b, cases, prepare, func(r WalletRepo, w int64) error {
		_, err := r.Debit(ctx, w, model.USD, dec("1"))
		return err
	})
}
