package repo

import (
	"context"
	"math/rand/v2"
	"os"
	"testing"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestDB connects to DATABASE_URL, a database migrated with
// `make migrate.apply`. The tests are skipped when it is unreachable, unless
// TEST_REQUIRE_DB is set (as `make go.test` does), where they fail instead.
func setupTestDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://user:passwd@localhost:5432/default?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		if os.Getenv("TEST_REQUIRE_DB") != "" {
			t.Fatalf("database unreachable, run `make infra.up migrate.apply`: %v", err)
		}
		t.Skipf("database unreachable, run `make infra.up migrate.apply`: %v", err)
	}
	return pool
}

// newWalletID returns a wallet unused by other tests and by the seed data,
// so tests are independent and can run against a seeded database.
func newWalletID() int64 {
	return 1<<40 + rand.Int64N(1<<40)
}

func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func assertBalance(t *testing.T, b model.Balance, available, held string) {
	t.Helper()
	assert.True(t, b.Available.Equal(dec(available)), "available: want %s, got %s", available, b.Available)
	assert.True(t, b.Held.Equal(dec(held)), "held: want %s, got %s", held, b.Held)
}

// models runs fn as a subtest against each WalletRepo implementation.
func models(t *testing.T, fn func(t *testing.T, pool *pgxpool.Pool, r WalletRepo)) {
	pool := setupTestDB(t)
	impls := []struct {
		name string
		repo WalletRepo
	}{
		{"account", NewAccountWalletRepo(pool)},
		{"utxo", NewUTXOWalletRepo(pool)},
	}
	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			fn(t, pool, impl.repo)
		})
	}
}
