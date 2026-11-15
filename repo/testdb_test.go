package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"walletmodeling/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// the tests own a schema of their own inside DATABASE_URL: TestMain builds it
// from migration/ and drops it afterwards, every test starts from empty wallet
// tables (setup), so whatever else the database holds is never touched
const (
	testSchema     = "wallet_test"
	testPoolConns  = 32
	defaultTestDSN = "postgres://user:passwd@localhost:5432/default?sslmode=disable"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests skips the package when the database is unreachable, unless
// TEST_REQUIRE_DB is set (as `make go.test` does), where it fails instead.
func runTests(m *testing.M) int {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		if os.Getenv("TEST_REQUIRE_DB") != "" {
			fmt.Fprintf(os.Stderr, "database unreachable, run `make infra.up`: %v\n", err)
			return 1
		}
		fmt.Printf("skip: database unreachable, run `make infra.up`: %v\n", err)
		return 0
	}
	defer admin.Close(ctx)

	if err := createTestSchema(ctx, admin); err != nil {
		fmt.Fprintf(os.Stderr, "create schema %s: %v\n", testSchema, err)
		return 1
	}
	defer admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+testSchema+" CASCADE")

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg.MaxConns = testPoolConns
	cfg.ConnConfig.RuntimeParams["search_path"] = testSchema
	if testPool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer testPool.Close()

	return m.Run()
}

// createTestSchema replays migration/ into a fresh testSchema, replacing one a
// killed run left behind. the session advisory lock, held until admin closes,
// makes a second `go test` on the same database wait instead of sharing it.
func createTestSchema(ctx context.Context, admin *pgx.Conn) error {
	files, err := filepath.Glob("../migration/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	script := "SELECT pg_advisory_lock(hashtext('" + testSchema + "'));" +
		"DROP SCHEMA IF EXISTS " + testSchema + " CASCADE;" +
		"CREATE SCHEMA " + testSchema + ";" +
		"SET search_path TO " + testSchema + ";"
	if _, err := admin.PgConn().Exec(ctx, script).ReadAll(); err != nil {
		return err
	}
	for _, file := range files {
		sql, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if _, err := admin.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
	return nil
}

// setup hands out the pool with the wallet tables of both models empty, and
// empties them again when the test or benchmark ends.
func setup(t testing.TB) *pgxpool.Pool {
	t.Helper()
	cleanDB(t)
	t.Cleanup(func() { cleanDB(t) })
	return testPool
}

// cleanDB removes all wallet data of both models. currency is reference data
// and stays.
func cleanDB(t testing.TB) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `TRUNCATE account_entry, account_hold, account, utxo, utxo_tx`)
	require.NoError(t, err)
}

// seedSpec describes wallets to bulk load into both models, in the state the
// repos would have left them in.
type seedSpec struct {
	wallets int // wallet ids 1..wallets
	cur     model.Currency
	outputs int    // credits per wallet: unspent outputs in the utxo model
	amount  string // of each credit
	// closed credit + debit pairs per wallet: entries and spent outputs that
	// no longer count towards a balance
	history int
}

// seed bulk loads s with set-based SQL, as going through the repos costs a
// transaction per row, then vacuums so plans and index-only scans behave as on
// a settled database. wallets are loaded in batches: one statement over
// millions of rows spends most of its time in queued foreign key checks.
func seed(t testing.TB, s seedSpec) {
	t.Helper()
	ctx := context.Background()
	const batchRows = 200_000
	batch := max(1, batchRows/max(s.outputs, s.history, 1))
	for first := 1; first <= s.wallets; first += batch {
		last := min(first+batch-1, s.wallets)
		for _, stmt := range seedStatements {
			n := s.outputs
			if stmt.history {
				n = s.history
			}
			_, err := testPool.Exec(ctx, stmt.sql, first, last, string(s.cur), dec(s.amount), n)
			require.NoError(t, err)
		}
	}
	_, err := testPool.Exec(ctx, `VACUUM (ANALYZE) account, account_entry, utxo, utxo_tx`)
	require.NoError(t, err)
}

// seedStatements load wallets $1..$2 with $5 credits of $4 in currency $3,
// or, the history ones, with $5 credits that a debit has spent again
var seedStatements = []struct {
	history bool
	sql     string
}{
	{false, `
		INSERT INTO account (wallet_id, currency, available)
		SELECT w, $3::text, $4::numeric * $5::int
		FROM generate_series($1::bigint, $2::bigint) AS w`},
	{false, `
		INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta)
		SELECT w, $3::text, 'credit', $4::numeric, 0
		FROM generate_series($1::bigint, $2::bigint) AS w, generate_series(1, $5::int)`},
	{true, `
		INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta)
		SELECT w, $3::text, k.kind, k.sign * $4::numeric, 0
		FROM generate_series($1::bigint, $2::bigint) AS w, generate_series(1, $5::int),
			(VALUES ('credit', 1), ('debit', -1)) AS k (kind, sign)`},
	{false, `
		WITH ids AS MATERIALIZED (
			SELECT w AS wallet_id, uuidv7() AS tx_id
			FROM generate_series($1::bigint, $2::bigint) AS w, generate_series(1, $5::int)
		), tx AS (
			INSERT INTO utxo_tx (tx_id, wallet_id, currency, kind, amount)
			SELECT tx_id, wallet_id, $3::text, 'credit', $4::numeric FROM ids
		)
		INSERT INTO utxo (wallet_id, currency, amount, created_tx)
		SELECT wallet_id, $3::text, $4::numeric, tx_id FROM ids`},
	{true, `
		WITH ids AS MATERIALIZED (
			SELECT w AS wallet_id, uuidv7() AS credit_tx, uuidv7() AS debit_tx
			FROM generate_series($1::bigint, $2::bigint) AS w, generate_series(1, $5::int)
		), tx AS (
			INSERT INTO utxo_tx (tx_id, wallet_id, currency, kind, amount)
			SELECT credit_tx, wallet_id, $3::text, 'credit', $4::numeric FROM ids
			UNION ALL
			SELECT debit_tx, wallet_id, $3::text, 'debit', $4::numeric FROM ids
		)
		INSERT INTO utxo (wallet_id, currency, amount, created_tx, spent_tx, spent_at)
		SELECT wallet_id, $3::text, $4::numeric, credit_tx, debit_tx, now() FROM ids`},
}

// audit checks, over everything in the database, the invariants each model
// must keep between its tables. call it only when no operation is in flight.
func audit(t testing.TB) {
	t.Helper()
	checks := []struct {
		name string
		sql  string
	}{
		{"account rows equal the sum of their entries", `
			SELECT count(*) FROM account AS a
			LEFT JOIN (
				SELECT wallet_id, currency, SUM(available_delta) AS available, SUM(held_delta) AS held
				FROM account_entry GROUP BY wallet_id, currency
			) AS e USING (wallet_id, currency)
			WHERE a.available <> COALESCE(e.available, 0) OR a.held <> COALESCE(e.held, 0)`},
		{"account held equals the sum of its active holds", `
			SELECT count(*) FROM account AS a
			LEFT JOIN (
				SELECT wallet_id, currency, SUM(amount) AS amount
				FROM account_hold WHERE status = 'active' GROUP BY wallet_id, currency
			) AS h USING (wallet_id, currency)
			WHERE a.held <> COALESCE(h.amount, 0)`},
		// outputs created minus outputs spent, in all and among held ones,
		// must be what the kind of the tx says
		{"every utxo tx conserves value", `
			SELECT count(*) FROM utxo_tx AS t
			LEFT JOIN (
				SELECT created_tx AS tx_id, SUM(amount) AS amount,
					COALESCE(SUM(amount) FILTER (WHERE hold_id IS NOT NULL), 0) AS held
				FROM utxo GROUP BY created_tx
			) AS c USING (tx_id)
			LEFT JOIN (
				SELECT spent_tx AS tx_id, SUM(amount) AS amount,
					COALESCE(SUM(amount) FILTER (WHERE hold_id IS NOT NULL), 0) AS held
				FROM utxo WHERE spent_tx IS NOT NULL GROUP BY spent_tx
			) AS s USING (tx_id)
			WHERE COALESCE(c.amount, 0) - COALESCE(s.amount, 0) <> CASE t.kind
				WHEN 'credit' THEN t.amount WHEN 'debit' THEN -t.amount
				WHEN 'settle' THEN -t.amount ELSE 0 END
			OR COALESCE(c.held, 0) - COALESCE(s.held, 0) <> CASE t.kind
				WHEN 'hold' THEN t.amount WHEN 'release' THEN -t.amount
				WHEN 'settle' THEN -t.amount ELSE 0 END`},
	}
	for _, c := range checks {
		var violations int
		require.NoError(t, testPool.QueryRow(context.Background(), c.sql).Scan(&violations), c.name)
		assert.Zero(t, violations, c.name)
	}
}

// models runs fn as a subtest against each WalletRepo implementation, on an
// empty database, and audits what it leaves behind.
func models(t *testing.T, fn func(t *testing.T, r WalletRepo)) {
	t.Helper()
	for _, impl := range repos() {
		t.Run(impl.name, func(t *testing.T) {
			setup(t)
			fn(t, impl.repo)
			audit(t)
		})
	}
}

type namedRepo struct {
	name string
	repo WalletRepo
}

func repos() []namedRepo {
	return []namedRepo{
		{"account", NewAccountWalletRepo(testPool)},
		{"utxo", NewUTXOWalletRepo(testPool)},
	}
}

func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func fund(t testing.TB, r WalletRepo, walletID int64, cur model.Currency, amount string) {
	t.Helper()
	_, err := r.Credit(context.Background(), walletID, cur, dec(amount))
	require.NoError(t, err)
}

func assertBalance(t testing.TB, r WalletRepo, walletID int64, cur model.Currency, available, held string) {
	t.Helper()
	b, err := r.GetBalance(context.Background(), walletID, cur)
	require.NoError(t, err)
	assert.True(t, b.Available.Equal(dec(available)), "wallet %d %s available: want %s, got %s", walletID, cur, available, b.Available)
	assert.True(t, b.Held.Equal(dec(held)), "wallet %d %s held: want %s, got %s", walletID, cur, held, b.Held)
}

// transfer moves amount between two wallets with the operations a WalletRepo
// offers, which has no transfer of its own: hold on the sender, credit the
// receiver, settle the hold, releasing it instead when the credit fails. the
// steps are separate transactions, so in between the amount shows as held on
// the sender.
func transfer(ctx context.Context, r WalletRepo, from, to int64, cur model.Currency, amount decimal.Decimal) error {
	hold, err := r.Hold(ctx, from, cur, amount)
	if err != nil {
		return err
	}
	if _, err := r.Credit(ctx, to, cur, amount); err != nil {
		_, releaseErr := r.ReleaseHold(ctx, hold.ID)
		return errors.Join(err, releaseErr)
	}
	_, err = r.SettleHold(ctx, hold.ID)
	return err
}

// retryContended repeats op for as long as the utxo model answers
// ErrContended, which asks the caller to retry later.
func retryContended(op func() error) error {
	for {
		if err := op(); !errors.Is(err, ErrContended) {
			return err
		}
	}
}

// parallel runs fn(0) .. fn(n-1) concurrently and returns their errors by index.
func parallel(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = fn(i) })
	}
	wg.Wait()
	return errs
}

// countErrors splits errs into successes and those matching target, failing
// the test on any other error.
func countErrors(t testing.TB, errs []error, target error) (ok, matched int) {
	t.Helper()
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, target):
			matched++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	return ok, matched
}
