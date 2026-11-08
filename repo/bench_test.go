package repo

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"walletmodeling/model"

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
