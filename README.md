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
`SET available = available - $amt WHERE ... AND available >= $amt`, plus a hold
and an entry row in the same transaction.

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
| write                   | 1 UPDATE (+ hold / entry insert)                      | select inputs, spend N, insert 1-2 outputs                      |
| non-negative guarantee  | `CHECK (available >= 0)` + guarded UPDATE             | outputs are positive; spends only what was selected and locked  |
| concurrency, same wallet+currency | serialized on one row lock (hot row), held ~3 round trips per write | parallel on disjoint outputs via `SKIP LOCKED`                  |
| contention failure mode | waits on the row lock                                 | skips locked outputs; when that leaves it short, backs off and retries |
| a long tx holding a lock | blocks every write of that account until it ends    | spends around the locked output; `ErrContended` only if it is needed |
| history / audit         | needs separate entries, kept in sync by the app       | intrinsic: every output records the tx that created and spent it |
| holds                   | `held` column + `account_hold` rows to keep consistent    | a hold *is* an output; no aggregate to keep in sync             |
| row growth              | 1 row per (wallet, currency); entries grow            | outputs grow per operation; needs consolidation / archiving     |
| precision               | identical, `numeric(38,18)` + per-currency `scale`    | identical                                                       |
| balance bound (< 1e20)  | exact, enforced by the column type under the row lock | per transaction: concurrent credits can exceed it (reads stay correct) |

Rule of thumb: A is simpler and reads are O(1); pick it unless a single
wallet's balance is a write hotspot, the more so when the database is a
network hop away (see [Where UTXO is faster](#where-utxo-is-faster)). B trades
read cost and storage for lock-free parallel spends and a self-auditing
history.

Not modeled: consolidating small outputs (B), partial settle, transfers
between wallets, a `wallet` table (wallet ids are external references).

## Benchmarks

`make go.bench` runs the same workloads against both models
(`repo/bench_test.go`). Medians of 5 runs on an Apple M4, PostgreSQL 18 in a
local container, 2026-11-09, starting from a freshly reset database
(`make infra.reset db.init`; the benchmarks themselves add ~600k outputs over
a full run of this section); parallel rows are wall time per op across 20
goroutines sharing a 10-connection pool:

| benchmark                                | account | utxo    | utxo / account |
| ---------------------------------------- | ------: | ------: | -------------: |
| GetBalance, 1 output                     | 126µs   | 125µs   | 0.99x          |
| GetBalance, 100 outputs                  | 128µs   | 145µs   | 1.13x          |
| GetBalance, 1000 outputs                 | 132µs   | 246µs   | 1.86x          |
| ListBalances, 100 outputs                | 133µs   | 174µs   | 1.31x          |
| Credit                                   | 724µs   | 1.19ms  | 1.65x          |
| Debit                                    | 753µs   | 1.06ms  | 1.41x          |
| Hold + release                           | 1.47ms  | 1.95ms  | 1.32x          |
| Debit consuming 10 outputs               | 752µs   | 1.32ms  | 1.76x          |
| Debit consuming 100 outputs              | 1.01ms  | 2.45ms  | 2.44x          |
| Parallel debit, same account, 1 output   | 612µs   | 2.16ms  | 3.53x          |
| Parallel debit, same account, 64 outputs | 673µs   | 615µs   | 0.91x          |
| Parallel debit, distinct accounts        | 411µs   | 679µs   | 1.65x          |

- The account model costs the same whatever the history; UTXO reads and every
  UTXO write (which re-reads the balance it returns and checks the bound) grow
  with the number of unspent outputs.
- The account model pays for its hot row: parallel debits of one account take
  about 1.6x the time of debits spread over accounts (673µs vs 411µs).
- The UTXO model has no hot row when the account holds enough outputs: with 64
  outputs, parallel debits of one account beat the account model. With a
  single output they chain through each other's change and back off, which is
  its worst case.
- Locally a round trip is ~0.1ms, which hides most of the hot-row cost; see
  below for the same workload with network latency.
- `Credit` accumulates outputs on one wallet over the run, so its UTXO time
  includes a growing balance read and varies most between runs.

**Cost of the history indexes.** An A/B right after the runs above, dropping
and recreating `utxo_created_tx_idx` and `utxo_spent_tx_idx` between 3
alternating rounds (medians of 6 runs each, UTXO model):

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

## Where UTXO is faster

One hot account under concurrent writes. The account model locks the row at
its `UPDATE` and keeps it until `COMMIT` returns, across the `account_entry`
INSERT and the COMMIT round trips, so writes of one account queue up however
many connections the app has. UTXO writes hold their locks as long, but on
disjoint outputs (or none: a credit only inserts), so they overlap.

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

Medians of 3 runs, same machine and session as [Benchmarks](#benchmarks); wall
time per op across all workers, lower is better. No run hit `ErrContended`.
Single cells vary by up to ~30% between sweeps on different days; the ratios
and the conclusions below held in every sweep.

**Round-trip latency** (16 workers, 64 outputs):

| rtt   | debit account | debit utxo | ratio | credit account | credit utxo | ratio | hold+settle account | hold+settle utxo | ratio |
| ----- | ------: | ------: | ----: | ------: | ------: | ----: | -------: | ------: | ----: |
| 0     | 785µs   | 628µs   | 0.80x | 702µs   | 430µs   | 0.61x | 1.46ms   | 904µs   | 0.62x |
| 0.5ms | 1.93ms  | 673µs   | 0.35x | 2.01ms  | 428µs   | 0.21x | 4.73ms   | 977µs   | 0.21x |
| 1ms   | 3.25ms  | 683µs   | 0.21x | 3.49ms  | 520µs   | 0.15x | 9.68ms   | 1.26ms  | 0.13x |
| 2ms   | 6.29ms  | 1.22ms  | 0.19x | 6.45ms  | 898µs   | 0.14x | 16.82ms  | 2.52ms  | 0.15x |

**Workers** (1ms rtt, 64 outputs):

| workers | debit account | debit utxo | ratio | credit account | credit utxo | ratio | hold+settle account | hold+settle utxo | ratio |
| ------: | ------: | ------: | ----: | ------: | ------: | ----: | -------: | ------: | ----: |
| 1       | 8.76ms  | 13.97ms | 1.60x | 9.96ms  | 10.61ms | 1.06x | 18.83ms  | 28.79ms | 1.53x |
| 4       | 3.55ms  | 2.91ms  | 0.82x | 3.71ms  | 2.19ms  | 0.59x | 9.23ms   | 6.06ms  | 0.66x |
| 16      | 3.25ms  | 683µs   | 0.21x | 3.49ms  | 520µs   | 0.15x | 9.68ms   | 1.26ms  | 0.13x |
| 64      | 3.34ms  | 435µs   | 0.13x | 3.35ms  | 360µs   | 0.11x | 8.80ms   | 723µs   | 0.08x |

**Outputs** the wallet starts from (debit, 1ms rtt, 16 workers):

| outputs | account | utxo    | ratio |
| ------: | ------: | ------: | ----: |
| 1       | 3.53ms  | 6.13ms  | 1.74x |
| 4       | 3.44ms  | 1.56ms  | 0.45x |
| 16      | 3.42ms  | 662µs   | 0.19x |
| 64      | 3.25ms  | 683µs   | 0.21x |
| 256     | 3.39ms  | 667µs   | 0.20x |

**A competing lock** held for `lock`, then released for as long (debit, 16
workers, 64 outputs):

| rtt | lock | account | utxo   | ratio |
| --: | ---: | ------: | -----: | ----: |
| 0   | 5ms  | 914µs   | 423µs  | 0.46x |
| 1ms | 1ms  | 3.72ms  | 677µs  | 0.18x |
| 1ms | 5ms  | 4.10ms  | 664µs  | 0.16x |
| 1ms | 20ms | 4.71ms  | 664µs  | 0.14x |

### How much faster under high concurrency

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
| debit       | account | 8.76ms  | 114/s | 300/s  | 2.6x          | 0.37     | 2.7x        | 0.35  | 0.0014  | 325/s      | 21  |
| debit       | utxo    | 13.97ms | 72/s  | 2298/s | 32x           | 0.016    | 62x         | 0     | 0.00044 | 2402/s     | 48  |
| credit      | account | 9.96ms  | 100/s | 298/s  | 3.0x          | 0.33     | 3.1x        | 0.35  | 0.00095 | 310/s      | 26  |
| credit      | utxo    | 10.61ms | 94/s  | 2779/s | 30x           | 0.019    | 52x         | 0     | 0.00051 | 2975/s     | 44  |
| hold+settle | account | 18.83ms | 53/s  | 114/s  | 2.1x          | 0.46     | 2.2x        | 0.49  | 0.00054 | 114/s      | 31  |
| hold+settle | utxo    | 28.79ms | 35/s  | 1384/s | 40x           | 0.010    | 99x         | 0     | 0.00036 | 1411/s     | 53  |

Reading it:

- **The account model is an Amdahl ceiling of ~2-3x.** A third to a half of
  an op is spent holding the row lock, so one account tops out near 300-325
  ops/s at 1ms rtt (hold+settle, two row updates per op, ~115/s) whatever the
  pool size; past N* more workers only make it slower.
- **The serial part is round trips.** At 80 workers the account model's time
  per debit fits S ≈ 0.96ms + 2.9 x rtt: the lock spans 2-3 round trips plus
  the commit. Its peak throughput is about 1 / S: ~1000 ops/s locally,
  ~270/s at 1ms, ~150/s at 2ms, ~65/s at 5ms.
- **The UTXO model has no measurable σ.** It scales with workers until the
  database server itself saturates (κ), peaking at ~2.4k-3.0k ops/s at
  N ≈ 44-53 on this machine (hold+settle ~1.4k/s): **~7-12x the account
  model's peak** (debit 7.4x, credit 9.6x, hold+settle 12.4x). Its time per
  op is ≈ T(1) / N, so against the account model's flat S the ratio is
  ≈ T_utxo(1) / (N x S_account): ~1.5x slower at N = 1, break-even at
  N ≈ 3-4, ~0.15-0.2x at 16, ~0.1x at 64.
- **It needs outputs to spread over.** With 1 output concurrent debits chain
  through each other's change (1.7x slower); 4 outputs already halve the
  account model's time, ~16 (≈ workers) reach the full gain. Beyond that more
  outputs only add balance-read cost.
- **A competing long lock** makes the account model worse the longer it is
  held (3.72ms → 4.71ms per debit from 1ms to 20ms), while the UTXO model is
  unaffected (~0.67ms).

### How far the gap goes

With enough workers each model hits a throughput ceiling, so the largest
speedup is the ratio of the two ceilings:

- account: one op at a time per account, X_account = 1 / S, S being the lock
  span. At 80 workers S ≈ 0.96ms + 2.9 x rtt.
- utxo: the lower of the server's capacity C (~2.3k-2.9k debits/s here) and
  N / T_utxo(1), N connections each running one op of T_utxo(1) ≈ 1.06ms +
  10.8 x rtt (~12 round trips).

```
speedup_max = X_utxo / X_account ≈ min(C x S, N x S / T_utxo(1))
```

The first term grows linearly with the rtt; the second tends to
N x 2.9 / 10.8 ≈ **N / 4**: the account lock spans about a quarter of the
round trips of a utxo op. So the gap is bounded by the connections the app
can run against one account, not by the rtt.

`BenchmarkHotAccountDebitPeak`: debits at 1 and 80 workers (the most the
database's `max_connections = 100` leaves room for), 128 outputs, medians of
3 runs:

| rtt | account, 1 worker | utxo, 1 worker | account, 80 workers (S) | utxo, 80 workers | utxo speedup | bound in effect |
| --: | ------: | ------: | ------: | ------: | ----------: | --------------- |
| 0   | 696µs   | 1.06ms  | 956µs   | 346µs   | 2.8x        | server (C x S)  |
| 1ms | 9.30ms  | 15.17ms | 3.65ms  | 401µs   | 9.1x        | server          |
| 2ms | 16.34ms | 26.36ms | 6.27ms  | 432µs   | 14.5x       | server          |
| 5ms | 34.94ms | 55.08ms | 15.32ms | 570µs   | 26.9x       | connections     |

At 5ms the connection bound N x S / T_utxo(1) = 80 x 15.32 / 55.08 = 22x
falls ~20% short of the measured 26.9x: 80 concurrent ops finish faster than
80 x one op alone, so the bound is a conservative estimate (an earlier sweep
matched it exactly at 21.8x). Past that, more rtt adds little: the limit is
about N / 4 to N / 3, ~20-27x for 80 connections, ~25-33x for 100; more
connections raise it until the server's capacity binds again.

Tests in `repo/latency_test.go`:

- `TestHotAccountUnderLatency`: 16 workers x 10 debits at 1ms rtt finish
  sooner on the UTXO model (0.16x-0.28x across runs)
- `TestHotAccountPeakSpeedup`: 64 workers x 5 debits at 1ms and 5ms rtt; the
  UTXO model must be over 3x and over 8x faster, and the gap must grow with
  the rtt (measured 7.2x and 12.1x)

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
- `latency_test.go`: concurrent debits of one hot account with latency
  injected per round trip; the UTXO model must finish first, and its lead
  must reach the expected size and grow with the rtt (see
  [Where UTXO is faster](#where-utxo-is-faster)). Skipped under `-short`
- `contract_test.go`: the same behaviour suite run against both repos,
  including concurrent debits, debits and holds contending for a single output,
  and racing release/settle of one hold
- `model_test.go`: 300 random operations applied to both models must give equal
  balances after every step; account == sum of its entries and active holds; every
  utxo tx conserves value; utxo output shapes for change and holds
