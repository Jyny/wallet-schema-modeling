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
local container, 2026-11-08; parallel rows are wall time per op across 20
goroutines sharing a 10-connection pool:

| benchmark                                | account | utxo    | utxo / account |
| ---------------------------------------- | ------: | ------: | -------------: |
| GetBalance, 1 output                     | 127µs   | 129µs   | 1.01x          |
| GetBalance, 100 outputs                  | 126µs   | 145µs   | 1.16x          |
| GetBalance, 1000 outputs                 | 128µs   | 285µs   | 2.22x          |
| ListBalances, 100 outputs                | 129µs   | 144µs   | 1.12x          |
| Credit                                   | 698µs   | 1.24ms  | 1.77x          |
| Debit                                    | 731µs   | 1.00ms  | 1.37x          |
| Hold + release                           | 1.61ms  | 1.96ms  | 1.22x          |
| Debit consuming 10 outputs               | 744µs   | 1.06ms  | 1.42x          |
| Debit consuming 100 outputs              | 746µs   | 1.98ms  | 2.65x          |
| Parallel debit, same account, 1 output   | 591µs   | 1.92ms  | 3.24x          |
| Parallel debit, same account, 64 outputs | 579µs   | 471µs   | 0.81x          |
| Parallel debit, distinct accounts        | 299µs   | 482µs   | 1.62x          |

- The account model costs the same whatever the history; UTXO reads and every
  UTXO write (which re-reads the balance it returns and checks the bound) grow
  with the number of unspent outputs.
- The account model pays for its hot row: parallel debits of one account take
  about 2x the time of debits spread over accounts (591µs vs 299µs).
- The UTXO model has no hot row when the account holds enough outputs: with 64
  outputs, parallel debits of one account match distinct accounts and beat the
  account model. With a single output they chain through each other's change
  and back off, which is its worst case.
- Locally a round trip is ~0.1ms, which hides most of the hot-row cost; see
  below for the same workload with network latency.
- `Credit` accumulates outputs on one wallet over the run, so its UTXO time
  includes a growing balance read (hence the higher variance).

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

Medians of 3 runs, Apple M4, PostgreSQL 18 in a local container, 2026-11-09;
wall time per op across all workers, lower is better. No run hit
`ErrContended`.

**Round-trip latency** (16 workers, 64 outputs):

| rtt   | debit account | debit utxo | ratio | credit account | credit utxo | ratio | hold+settle account | hold+settle utxo | ratio |
| ----- | ------: | ------: | ----: | ------: | ------: | ----: | -------: | ------: | ----: |
| 0     | 670µs   | 430µs   | 0.64x | 1.70ms  | 482µs   | 0.28x | 3.64ms   | 930µs   | 0.26x |
| 0.5ms | 1.84ms  | 539µs   | 0.29x | 1.86ms  | 495µs   | 0.27x | 4.69ms   | 1.06ms  | 0.23x |
| 1ms   | 3.11ms  | 695µs   | 0.22x | 3.06ms  | 554µs   | 0.18x | 8.02ms   | 1.37ms  | 0.17x |
| 2ms   | 5.51ms  | 1.23ms  | 0.22x | 5.32ms  | 1.01ms  | 0.19x | 13.54ms  | 2.44ms  | 0.18x |

**Workers** (1ms rtt, 64 outputs):

| workers | debit account | debit utxo | ratio | credit account | credit utxo | ratio | hold+settle account | hold+settle utxo | ratio |
| ------: | ------: | ------: | ----: | ------: | ------: | ----: | -------: | ------: | ----: |
| 1       | 7.73ms  | 12.28ms | 1.59x | 7.68ms  | 8.62ms  | 1.12x | 19.46ms  | 22.23ms | 1.14x |
| 4       | 3.05ms  | 2.95ms  | 0.97x | 2.96ms  | 2.12ms  | 0.72x | 7.54ms   | 5.73ms  | 0.76x |
| 16      | 3.11ms  | 695µs   | 0.22x | 3.06ms  | 554µs   | 0.18x | 8.02ms   | 1.37ms  | 0.17x |
| 64      | 3.36ms  | 382µs   | 0.11x | 3.42ms  | 414µs   | 0.12x | 10.58ms  | 1.36ms  | 0.13x |

**Outputs** the wallet starts from (debit, 1ms rtt, 16 workers):

| outputs | account | utxo    | ratio |
| ------: | ------: | ------: | ----: |
| 1       | 3.53ms  | 6.36ms  | 1.80x |
| 4       | 3.52ms  | 1.68ms  | 0.48x |
| 16      | 3.18ms  | 692µs   | 0.22x |
| 64      | 3.11ms  | 695µs   | 0.22x |
| 256     | 2.95ms  | 797µs   | 0.27x |

**A competing lock** held for `lock`, then released for as long (debit, 16
workers, 64 outputs):

| rtt | lock | account | utxo   | ratio |
| --: | ---: | ------: | -----: | ----: |
| 0   | 5ms  | 1.35ms  | 753µs  | 0.56x |
| 1ms | 1ms  | 3.37ms  | 925µs  | 0.27x |
| 1ms | 5ms  | 3.69ms  | 1.02ms | 0.28x |
| 1ms | 20ms | 5.13ms  | 923µs  | 0.18x |

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
| debit       | account | 7.73ms  | 129/s | 297/s  | 2.3x          | 0.43     | 2.3x        | 0.37  | 0.0025  | 348/s      | 16  |
| debit       | utxo    | 12.28ms | 81/s  | 2617/s | 32x           | 0.016    | 61x         | 0     | 0.00034 | 2651/s     | 55  |
| credit      | account | 7.68ms  | 130/s | 293/s  | 2.2x          | 0.44     | 2.3x        | 0.36  | 0.0029  | 355/s      | 15  |
| credit      | utxo    | 8.62ms  | 116/s | 2413/s | 21x           | 0.033    | 30x         | 0     | 0.00059 | 2659/s     | 41  |
| hold+settle | account | 19.46ms | 51/s  | 95/s   | 1.8x          | 0.54     | 1.9x        | 0.33  | 0.0054  | 138/s      | 11  |
| hold+settle | utxo    | 22.23ms | 45/s  | 734/s  | 16x           | 0.046    | 22x         | 0     | 0.00092 | 909/s      | 33  |

Reading it:

- **The account model is an Amdahl ceiling of ~2.3x.** About 43% of a debit
  is spent holding the row lock, so one account tops out near 300-350 ops/s
  at 1ms rtt whatever the pool size; past N* ≈ 16 more workers only make it
  slower (3.11ms → 3.36ms per op at 64). Hold+settle updates the row twice and
  caps lower, at ~1.9x.
- **The serial part is round trips.** Over the rtt sweep the account model's
  time per debit at 16 workers fits S ≈ 0.66ms + 2.4 x rtt: the lock spans
  2-3 round trips plus the commit. Its peak throughput is about
  1 / (0.66ms + 2.4 x rtt): ~1500 ops/s locally, ~300/s at 1ms, ~180/s at 2ms.
- **The UTXO model has no measurable σ.** It scales with workers until the
  database server itself saturates (κ), peaking at ~2650 debits/s at N ≈ 55
  on this machine: **~7.6x the account model's peak** for debits and credits,
  ~6.6x for hold+settle. Its time per op is ≈ T(1) / N, so against the
  account model's flat S the ratio is ≈ T_utxo(1) / (N x S_account): 1.6x
  slower at N = 1, break-even at N ≈ 4, ~0.2x at 16, ~0.1x at 64.
- **It needs outputs to spread over.** With 1 output concurrent debits chain
  through each other's change (1.8x slower); 4 outputs already halve the
  account model's time, ~16 (≈ workers) reach the full gain. Beyond that more
  outputs only add balance-read cost (256: 0.27x).
- **A competing long lock** makes the account model worse the longer it is
  held (3.37ms → 5.13ms per debit from 1ms to 20ms), while the UTXO model is
  unaffected (~0.92ms).

`TestHotAccountUnderLatency` (`repo/latency_test.go`) asserts the baseline
case: 16 workers x 10 debits at 1ms rtt must finish sooner on the UTXO model
(0.16x-0.28x across runs).

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
- `latency_test.go`: concurrent debits of one hot account with 1ms injected
  per round trip; the UTXO model must finish first (see
  [Where UTXO is faster](#where-utxo-is-faster)). Skipped under `-short`
- `contract_test.go`: the same behaviour suite run against both repos,
  including concurrent debits, debits and holds contending for a single output,
  and racing release/settle of one hold
- `model_test.go`: 300 random operations applied to both models must give equal
  balances after every step; account == sum of its entries and active holds; every
  utxo tx conserves value; utxo output shapes for change and holds
