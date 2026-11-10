# Wallet Schema Modeling

Two relational models of a multi-currency wallet balance on PostgreSQL 18,
behind one Go interface, so they can be compared side by side:

- **A. account model, currency as row** (`account*`): one mutable account row
  per (wallet, currency)
- **B. UTXO based** (`utxo*`): the balance is the sum of immutable unspent outputs

Every currency balance has an **available** (spendable) and a **held** (locked)
amount. Seeded currencies: `USD`, `USDT`, `BTC`, `ETH`.

## Layout

```
schema/schema.sql   desired schema (declarative, source of truth)
migration/          versioned migrations generated from it, plus currency seed
query/              sqlc queries: currency.sql, account.sql, utxo.sql
repo/db/            sqlc generated code (do not edit)
model/              Balance, Hold, Currency
repo/               WalletRepo interface, AccountWalletRepo, UTXOWalletRepo, tests
cmd/seed/           demo wallets 1..3, written through both repos
```

## Quick start

Requires Docker (or Podman), [Atlas](https://atlasgo.io) and Go. sqlc runs in
Docker.

```bash
make db.init     # infra.up + migrate.apply + db.seed
make go.test     # unit tests against the local database
make             # list every target
```

Schema workflow follows [atlas-lab](https://github.com/Jyny/atlas-lab) 0.4.0
(postgres only): edit `schema/schema.sql`, then

```bash
make migrate.diff <name>   # generate a migration
make migrate.lint          # lint it
make sqlc.generate         # regenerate repo/db after query or schema changes
```

## Interface

```go
type WalletRepo interface {
	GetBalance(ctx, walletID, cur) (model.Balance, error)
	ListBalances(ctx, walletID) ([]model.Balance, error)     // every currency
	Credit(ctx, walletID, cur, amount) (model.Balance, error) // available +
	Debit(ctx, walletID, cur, amount) (model.Balance, error)  // available -
	Hold(ctx, walletID, cur, amount) (model.Hold, error)      // available -> held
	ReleaseHold(ctx, holdID) (model.Balance, error)           // held -> available
	SettleHold(ctx, holdID) (model.Balance, error)            // held -> out
}
```

Errors: `ErrInsufficientFunds`, `ErrUnknownCurrency`, `ErrInvalidAmount`
(non-positive, more fractional digits than `currency.scale`, or an amount or
resulting balance of 1e20 or more),
`ErrHoldNotFound` (unknown or already closed), `ErrContended` (UTXO model only:
the funds exist but other transactions keep them locked past the retry budget;
retry later).

## Schema

Shared: `currency(code, scale)`. Amounts are `numeric(38,18)`, enough for wei.

### A. Account model (currency as row)

The counterpart of the UTXO model, as in Ethereum's account model vs Bitcoin's
UTXO model: each (wallet, currency) is an account holding its balance.

| table           | role                                                                 |
| --------------- | -------------------------------------------------------------------- |
| `account`       | PK (wallet_id, currency); `available`, `held`, both `CHECK >= 0`     |
| `account_hold`  | one row per hold, `status` active / released / settled              |
| `account_entry` | append-only ledger entries (`available_delta`, `held_delta`) per operation |

Each operation is one guarded `UPDATE` on the account row, e.g. debit is
`SET available = available - $amt WHERE ... AND available >= $amt`. The
update, its entry and, for holds, the hold row are written by one statement of
data-modifying CTEs (`WITH b AS (UPDATE ... RETURNING ...), e AS (INSERT INTO
account_entry SELECT ... FROM b) SELECT ... FROM b`), atomic on its own and
run in autocommit: no `BEGIN`/`COMMIT` round trips, and the row is locked only
while the server runs and commits that statement. A failed guard leaves the
first CTE empty, so nothing is written. Credit, debit and hold first check the
amount against `currency` in a separate read; release and settle are the
statement alone.

### B. UTXO based

| table     | role                                                                     |
| --------- | ------------------------------------------------------------------------ |
| `utxo_tx` | one row per operation (kind, amount, hold_id)                            |
| `utxo`    | outputs; `created_tx` / `spent_tx`, `hold_id` set = held, null = available |

Outputs are never modified except to mark them spent, once.

- credit: create one output
- debit: select available outputs oldest first until they cover the amount
  (recursive CTE, `FOR UPDATE SKIP LOCKED`), spend them, create a change output.
  If the selection falls short while the funds exist (outputs locked by
  concurrent operations, their change not yet visible), back off with jitter
  and retry in a fresh transaction, still skipping locks. It never waits on a
  lock, so one output held by a long transaction cannot stall spends the
  other outputs can serve. After ~1s of retries it returns `ErrContended`
- hold: same selection, then create one output of exactly the amount carrying
  `hold_id`, plus change. `UNIQUE(hold_id)`: a hold is one output
- release: spend the held output, recreate it as available
- settle: spend the held output, create nothing

Balance is `SUM(amount) FILTER (WHERE hold_id IS NULL / IS NOT NULL)` over
unspent outputs, served index-only by
`utxo_unspent_idx (wallet_id, currency) INCLUDE (amount, hold_id) WHERE spent_at IS NULL`.

History, the outputs a tx created and spent, is served by `utxo_created_tx_idx
(created_tx)` and `utxo_spent_tx_idx (spent_tx) WHERE spent_tx IS NOT NULL`.
No repo operation reads by these columns, so they only cost writes (see
[Benchmarks](#benchmarks)); they serve audits and the test suite's
conservation check, which without them scans the whole table per tx.

## Comparison

|                         | A. account (currency as row)                          | B. UTXO                                                         |
| ----------------------- | ----------------------------------------------------- | --------------------------------------------------------------- |
| read balance            | single PK lookup                                      | aggregate over unspent outputs (grows with fragmentation)       |
| write                   | 1 statement: UPDATE + hold / entry insert as CTEs     | select inputs, spend N, insert 1-2 outputs                      |
| non-negative guarantee  | `CHECK (available >= 0)` + guarded UPDATE             | outputs are positive; spends only what was selected and locked  |
| concurrency, same wallet+currency | serialized on one row lock (hot row), held for one statement and its commit, no client round trip | parallel on disjoint outputs via `SKIP LOCKED`                  |
| contention failure mode | waits on the row lock                                 | skips locked outputs; when that leaves it short, backs off and retries |
| a long tx holding a lock | blocks every write of that account until it ends    | spends around the locked output; `ErrContended` only if it is needed |
| history / audit         | needs separate entries, kept in sync by the app       | intrinsic: every output records the tx that created and spent it |
| holds                   | `held` column + `account_hold` rows to keep consistent    | a hold *is* an output; no aggregate to keep in sync             |
| row growth              | 1 row per (wallet, currency); entries grow            | outputs grow per operation; needs consolidation / archiving     |
| precision               | identical, `numeric(38,18)` + per-currency `scale`    | identical                                                       |
| balance bound (< 1e20)  | exact, enforced by the column type under the row lock | per transaction: concurrent credits can exceed it (reads stay correct) |

Rule of thumb: A is simpler, reads are O(1) and each op is ~3 round trips
against B's ~11; pick it unless a single wallet's balance needs more than
~1000 writes/s, its row lock's limit here. B lifts that limit to the
server's capacity, ~2x on this machine, given many connections and enough
outputs (see [Hot account](#hot-account)), and trades read cost and storage
for lock-free parallel spends and a self-auditing history.

Not modeled: consolidating small outputs (B), partial settle, transfers
between wallets, a `wallet` table (wallet ids are external references).

## Benchmarks

Hardware and setup for every table below, measured 2026-11-09:

| | |
| --- | --- |
| machine | MacBook Air (Mac14,2), Apple M2, 8 cores (4 performance + 4 efficiency), 24 GB |
| OS | macOS 26.7.1 |
| container runtime | OrbStack 2.0.5 (Docker 28.5.2, linux/arm64 VM with 8 CPUs, 12 GB) |
| database | PostgreSQL 18.6 (`postgres:18`), defaults: `max_connections = 100`, `shared_buffers = 128MB`, `fsync` and `synchronous_commit` on |
| client | Go 1.27.0, pgx v5, on the same machine |

Each run started from a freshly reset database (`make infra.reset db.init`);
the benchmarks add a few hundred thousand outputs over a full run.

`make go.bench` runs the same workloads against both models
(`repo/bench_test.go`). Medians of 5 runs; parallel rows are wall time per
op across 16 goroutines sharing an 8-connection pool:

| benchmark                                | account | utxo    | utxo / account |
| ---------------------------------------- | ------: | ------: | -------------: |
| GetBalance, 1 output                     | 100µs   | 102µs   | 1.02x          |
| GetBalance, 100 outputs                  | 96µs    | 132µs   | 1.38x          |
| GetBalance, 1000 outputs                 | 126µs   | 313µs   | 2.49x          |
| ListBalances, 100 outputs                | 131µs   | 191µs   | 1.46x          |
| Credit                                   | 1.09ms  | 1.60ms  | 1.46x          |
| Debit                                    | 1.20ms  | 1.98ms  | 1.65x          |
| Hold + release                           | 2.10ms  | 3.79ms  | 1.80x          |
| Debit consuming 10 outputs               | 1.14ms  | 2.23ms  | 1.96x          |
| Debit consuming 100 outputs              | 1.02ms  | 4.10ms  | 4.03x          |
| Parallel debit, same account, 1 output   | 767µs   | 1.77ms  | 2.31x          |
| Parallel debit, same account, 64 outputs | 787µs   | 462µs   | 0.59x          |
| Parallel debit, distinct accounts        | 190µs   | 453µs   | 2.39x          |

- Each account write is one statement (validation aside, see
  [Schema](#a-account-model-currency-as-row)), each UTXO write a transaction
  of 5 or more, so the account model is 1.5-2x faster per op, 4x when the
  UTXO debit must consume 100 outputs.
- The account model costs the same whatever the history; UTXO reads and every
  UTXO write (which re-reads the balance it returns and checks the bound) grow
  with the number of unspent outputs.
- The hot row still costs the account model: parallel debits of one account
  take about 4x the time of debits spread over accounts (787µs vs 190µs).
  With 64 outputs the UTXO model has no hot row and is faster there; with a
  single output its debits chain through each other's change and back off,
  its worst case.
- Locally a round trip is ~0.1ms; see below for the same workloads with
  network latency.

**Cost of the history indexes.** An A/B of the UTXO model only, measured
earlier (Apple M4, same PostgreSQL), dropping and recreating
`utxo_created_tx_idx` and `utxo_spent_tx_idx` between 3 alternating rounds
(medians of 6 runs each); not rerun on the setup above:

| benchmark                                 | without | with    | change |
| ----------------------------------------- | ------: | ------: | -----: |
| Credit                                    | 1.11ms  | 1.06ms  | -5%    |
| Debit                                     | 1.08ms  | 1.29ms  | +19%   |
| Hold + release                            | 2.02ms  | 2.49ms  | +23%   |
| Debit consuming 100 outputs               | 2.06ms  | 2.33ms  | +13%   |
| Parallel debit, same account, 64 outputs  | 489µs   | 602µs   | +23%   |

Each credit and change output adds an entry to `utxo_created_tx_idx`, each
spent output one to `utxo_spent_tx_idx`, so operations that spend pay most:
about 10-20% on UTXO writes. Single runs spread by ±20% (credit shows no
cost within that noise). In exchange the test suite's conservation check
reads by index instead of scanning `utxo` per tx: on a database grown to
~480k outputs, `make go.test` takes ~15s with them and ~77s without.

## Hot account

One account under concurrent writes. The account model locks its row in the
write statement and keeps the lock until that statement commits. Since the
statement autocommits, the commit happens on the server, without another
client round trip: the lock span is the server's time for one statement and
its WAL flush, whatever the network latency. UTXO writes hold their locks
across a whole transaction of ~11 round trips, but on disjoint outputs (or
none: a credit only inserts), so they overlap.

`make go.bench.hot` (`BenchmarkHotAccount*` in `repo/bench_test.go`) sweeps
one dimension at a time around a baseline of **1ms per round trip** (about a
cross-zone hop, injected by delaying every write on the connection), **16
workers** each with its own connection, and a wallet funded by **64 outputs**.
Scenarios:

| benchmark               | workload                                         | why the account model serializes                 |
| ----------------------- | ------------------------------------------------ | ------------------------------------------------ |
| `HotAccountDebit`       | debits                                           | every debit updates the row                      |
| `HotAccountCredit`      | credits to an empty wallet (incoming payments)   | every credit updates the row; UTXO takes no lock |
| `HotAccountHoldSettle`  | hold then settle (authorize, capture)            | two row updates per op; UTXO settle locks only its output |
| `HotAccountDebitSlowTx` | debits while another session locks the funds, on and off | waits for the row; UTXO spends around the locked output |
| `HotAccountDebitPeak`   | debits at 1 and 80 workers, rtt up to 5ms        | see [How far the gap goes](#how-far-the-gap-goes) |

Medians of 3 runs; wall time per op across all workers, lower is better. No
run hit `ErrContended`. Single cells vary by up to ~30% between runs; the
ratios and conclusions below held in each of two full sweeps.

**Round-trip latency** (16 workers, 64 outputs):

| rtt   | debit account | debit utxo | ratio | credit account | credit utxo | ratio | hold+settle account | hold+settle utxo | ratio |
| ----- | ------: | ------: | ----: | ------: | ------: | ----: | -------: | ------: | ----: |
| 0     | 761µs   | 340µs   | 0.45x | 704µs   | 323µs   | 0.46x | 1.35ms   | 679µs   | 0.50x |
| 0.5ms | 721µs   | 491µs   | 0.68x | 665µs   | 412µs   | 0.62x | 1.35ms   | 970µs   | 0.72x |
| 1ms   | 705µs   | 686µs   | 0.97x | 669µs   | 553µs   | 0.83x | 1.83ms   | 1.43ms  | 0.78x |
| 2ms   | 699µs   | 1.35ms  | 1.93x | 649µs   | 967µs   | 1.49x | 1.81ms   | 2.33ms  | 1.28x |

**Workers** (1ms rtt, 64 outputs):

| workers | debit account | debit utxo | ratio | credit account | credit utxo | ratio | hold+settle account | hold+settle utxo | ratio |
| ------: | ------: | ------: | ----: | ------: | ------: | ----: | -------: | ------: | ----: |
| 1       | 6.52ms  | 19.05ms | 2.92x | 6.95ms  | 15.02ms | 2.16x | 9.56ms   | 32.61ms | 3.41x |
| 4       | 838µs   | 3.40ms  | 4.06x | 814µs   | 2.52ms  | 3.10x | 1.42ms   | 5.63ms  | 3.98x |
| 16      | 705µs   | 686µs   | 0.97x | 669µs   | 553µs   | 0.83x | 1.83ms   | 1.43ms  | 0.78x |
| 64      | 849µs   | 345µs   | 0.41x | 830µs   | 286µs   | 0.34x | 1.70ms   | 764µs   | 0.45x |

**Outputs** the wallet starts from (debit, 1ms rtt, 16 workers):

| outputs | account | utxo    | ratio  |
| ------: | ------: | ------: | -----: |
| 1       | 697µs   | 7.51ms  | 10.77x |
| 4       | 731µs   | 1.92ms  | 2.63x  |
| 16      | 675µs   | 685µs   | 1.02x  |
| 64      | 705µs   | 686µs   | 0.97x  |
| 256     | 695µs   | 707µs   | 1.02x  |

**A competing lock** held for `lock`, then released for as long (debit, 16
workers, 64 outputs):

| rtt | lock | account | utxo   | ratio |
| --: | ---: | ------: | -----: | ----: |
| 0   | 5ms  | 966µs   | 364µs  | 0.38x |
| 1ms | 1ms  | 916µs   | 706µs  | 0.77x |
| 1ms | 5ms  | 1.26ms  | 695µs  | 0.55x |
| 1ms | 20ms | 1.46ms  | 705µs  | 0.48x |

### Scaling with workers

Throughput X(N) = 1 / (time per op) over the workers sweep, fitted to two
models:

- **Amdahl's law**: speedup(N) = 1 / (σ + (1 - σ) / N), σ being the serial
  fraction of an op. Solved from the measured speedup at N = 64; the ceiling
  is 1/σ.
- **Universal Scalability Law** (Gunther): X(N) = λN / (1 + σ(N - 1) +
  κN(N - 1)). σ is contention (queueing on a shared lock), κ coherency (costs
  growing with every pair of workers: lock skipping, WAL and CPU on one
  server). Peak throughput at N* = √((1 - σ) / κ).

| benchmark   | model   | T(1)    | X(1)  | X(64)  | speedup at 64 | Amdahl σ | ceiling 1/σ | USL σ | USL κ   | USL peak X | N*  |
| ----------- | ------- | ------: | ----: | -----: | ------------: | -------: | ----------: | ----: | ------: | ---------: | --: |
| debit       | account | 6.52ms  | 153/s | 1178/s | 7.7x          | 0.12     | 8.6x        | 0.03  | 0.0018  | 1638/s     | 23  |
| debit       | utxo    | 19.05ms | 52/s  | 2903/s | 55x           | 0.002    | 400x        | 0     | 0.00010 | 3264/s     | 100 |
| credit      | account | 6.95ms  | 144/s | 1205/s | 8.4x          | 0.11     | 9.5x        | 0.01  | 0.0018  | 1731/s     | 24  |
| credit      | utxo    | 15.02ms | 67/s  | 3500/s | 53x           | 0.003    | 290x        | 0     | 0.00012 | 3819/s     | 91  |
| hold+settle | account | 9.56ms  | 105/s | 589/s  | 5.6x          | 0.16     | 6.1x        | 0.12  | 0.0012  | 647/s      | 28  |
| hold+settle | utxo    | 32.61ms | 31/s  | 1309/s | 43x           | 0.008    | 126x        | 0     | 0.00020 | 1333/s     | 71  |

Reading it:

- **The account model's serial part is no longer the network.** T(1) is
  ~3 round trips, but only the write's server-side time is under the row
  lock, so σ is small (0.1-0.16) and one
  account tops out near ~1200 ops/s (hold+settle, two row writes per op, ~600/s)
  at any rtt; the USL peak lies at N ≈ 23-28 workers.
- **The UTXO model has no measurable σ.** It scales with workers until the
  database server itself saturates (κ), but each op costs ~3x the round
  trips of an account op, so it needs many more workers for the same
  throughput: at 1ms rtt it is 2-4x slower at N ≤ 4, at parity around
  N = 16 and 2-3x faster at N = 64.
- **It needs outputs to spread over.** With 1 output concurrent debits chain
  through each other's change (10x slower); 4 outputs are still 2.6x slower,
  ~16 (≈ workers) reach parity. Beyond that more outputs only add
  balance-read cost.
- **A competing long lock** still slows the account model the longer it is
  held (916µs → 1.46ms per debit from 1ms to 20ms) while the UTXO model is
  unaffected (~0.7ms).

### How far the gap goes

With enough workers each model hits a throughput ceiling:

- account: one op at a time per account, X_account = 1 / S, S being the lock
  span. At 80 workers S ≈ 0.8-1.1ms, independent of the rtt.
- utxo: the lower of the server's capacity C (~2.1k-2.4k debits/s here) and
  N / T_utxo(1), N connections each running one op of T_utxo(1) ≈ 4ms +
  11 x rtt.

```
speedup_max = X_utxo / X_account ≈ min(C x S, N x S / T_utxo(1))
```

The first term is ~2x here; the second falls as the rtt grows, since S does
not grow with it and T_utxo(1) does. So on one account the UTXO model is at
most ~2x faster, and only with enough connections to cover its longer ops.

`BenchmarkHotAccountDebitPeak`: debits at 1 and 80 workers (the most the
database's `max_connections = 100` leaves room for), 128 outputs, medians of
3 runs:

| rtt | account, 1 worker | utxo, 1 worker | account, 80 workers (S) | utxo, 80 workers | utxo speedup | account X | utxo X |
| --: | ------: | ------: | ------: | ------: | ----------: | ------: | ------: |
| 0   | 867µs   | 1.69ms  | 994µs   | 472µs   | 2.11x       | 1006/s  | 2118/s  |
| 1ms | 5.39ms  | 16.33ms | 839µs   | 417µs   | 2.01x       | 1192/s  | 2401/s  |
| 2ms | 8.25ms  | 29.10ms | 1.09ms  | 416µs   | 2.61x       | 919/s   | 2402/s  |
| 5ms | 16.41ms | 58.01ms | 805µs   | 591µs   | 1.36x       | 1242/s  | 1691/s  |

Up to 2ms the server bound holds (UTXO ~2.1k-2.4k/s). At 5ms the connection
bound takes over: 80 / 58.01ms ≈ 1380/s predicted, 1691/s measured (80
concurrent ops finish faster than 80 x one op alone, so the bound is
conservative), and the gap shrinks to 1.36x. Past that, more rtt favours the
account model: about 11 x rtt x 1200/s connections are needed just to match
it, ~70 at 5ms.

Tests in `repo/latency_test.go` predate the single-statement account writes
and assert the old, much larger gap (see [Tests](#tests)):

- `TestHotAccountUnderLatency`: 16 workers x 10 debits at 1ms rtt must
  finish sooner on the UTXO model. The models are now at parity there, so it
  passes or fails run to run (1.02x when it failed)
- `TestHotAccountPeakSpeedup`: 64 workers x 5 debits at 1ms and 5ms rtt; the
  UTXO model must be over 3x and over 8x faster, and the gap must grow with
  the rtt. It now fails (2.5x at 1ms)

## Reads and holds together

`make go.bench.interact` (`BenchmarkInteract*` in `repo/bench_test.go`) runs
reads and hold + release of one account (64 outputs) at the same time and
times one side: 8 readers on an undelayed pool, 16 holders with rtt added to
their round trips. The other side runs in the background; its rate is the
ops it finished while the timed side ran. Medians of 3 runs.

**Reads, alone and while 16 workers hold and release** (time per read across
8 readers):

| read | rtt | account alone | with holds | change | holds/s | utxo alone | with holds | change | holds/s |
| ---- | --: | ------: | ------: | -----: | ------: | ------: | ------: | -----: | ------: |
| get  | 0   | 35µs    | 46µs    | +33%   | 426     | 47µs    | 120µs   | +154%  | 900     |
| get  | 1ms | 35µs    | 45µs    | +31%   | 423     | 49µs    | 90µs    | +86%   | 526     |
| list | 0   | 36µs    | 49µs    | +36%   | 400     | 54µs    | 134µs   | +149%  | 825     |
| list | 1ms | 37µs    | 51µs    | +38%   | 277     | 58µs    | 111µs   | +90%   | 476     |

**Hold + release, alone and while 8 readers poll** (time per op across 16
holders):

| rtt | account alone | with reads | change | reads/s | utxo alone | with reads | change | reads/s |
| --: | ------: | ------: | -----: | ------: | ------: | ------: | -----: | ------: |
| 0   | 1.38ms  | 3.19ms  | +131%  | 16507   | 819µs   | 1.35ms  | +65%   | 8169    |
| 1ms | 1.70ms  | 3.10ms  | +83%   | 16092   | 1.49ms  | 2.14ms  | +44%   | 9758    |

- Reads never wait for a hold's locks in either model; what slows them is
  the shared server. UTXO reads slow down more: their holds run about twice
  as fast in the background, and each sum covers the outputs the holds leave
  behind.
- Holds slow down under reads in both models, the account model more,
  likely because its writes queue on one row: server time taken by the
  readers lengthens the lock span every queued write waits through.

## Tests

`repo/` tests need `DATABASE_URL` (defaults to the local compose database).
Plain `go test` skips them when it is unreachable; `make go.test` sets
`TEST_REQUIRE_DB=1` so they fail instead. Each test uses a fresh random wallet id, so
they run against a seeded database and repeat cleanly.

- `concurrency_test.go`: concurrency scenarios run against both repos: a mixed
  random workload checked against a ledger of successes and the model
  invariants, holds racing for exactly enough funds, reads never seeing a
  half-applied operation, first-credit races, credits racing past the balance
  bound, and contention isolated per currency
- `stuck_test.go`: a long transaction holding a lock: the account model waits
  and honours the context, the UTXO model spends around the locked output, also
  under contention; canceled contexts leave no partial state
- `domain_test.go`: domain edge cases against both repos: the largest amount
  per currency, spending exactly everything, hundreds of dust outputs, exact
  decimal arithmetic, malformed currency codes, amount spellings, hold ids
  across models, release overflowing available, wallets holding only held funds
- `hold_read_test.go`: reads and holds together against both repos: every
  step of overlapping holds agrees across `GetBalance`, `ListBalances` and the
  returned balance, held funds cannot be spent, reads never wait on a stuck
  release or settle, and concurrent reads during hold/settle and hold/release
  on two currencies only ever see whole operations
- `latency_test.go`: concurrent debits of one hot account with latency
  injected per round trip; the UTXO model must finish first, and its lead
  must reach the expected size and grow with the rtt. Skipped under
  `-short`. These assert the gap measured before the single-statement account
  writes and no longer hold: `TestHotAccountPeakSpeedup` fails and
  `TestHotAccountUnderLatency` is flaky (see
  [How far the gap goes](#how-far-the-gap-goes))
- `contract_test.go`: the same behaviour suite run against both repos,
  including concurrent debits, debits and holds contending for a single output,
  and racing release/settle of one hold
- `model_test.go`: 300 random operations applied to both models must give equal
  balances after every step; account == sum of its entries and active holds; every
  utxo tx conserves value; utxo output shapes for change and holds
