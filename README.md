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
repo/               WalletRepo interface, AccountWalletRepo, UTXOWalletRepo
repo/*_test.go      tests and benchmarks run against both models, see Tests
```

## Quick start

Requires Docker (or Podman), [Atlas](https://atlasgo.io) and Go. sqlc runs in
Docker.

```bash
make db.init     # infra.up + migrate.apply
make go.test     # tests against the local database, see Tests
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
No repo operation reads by these columns, so they only cost writes; they serve
audits, such as checking that each tx conserves value, which without them
scans the whole table per tx.

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
network hop away. B trades read cost and storage for lock-free parallel spends
and a self-auditing history.

Not modeled: consolidating small outputs (B), partial settle, transfers
between wallets, a `wallet` table (wallet ids are external references).

## Tests

```bash
make infra.up    # the tests only need a running database
make go.test     # about 20s
```

The tests build a schema of their own, `wallet_test`, inside `DATABASE_URL`
by replaying `migration/`, and drop it afterwards, so whatever else the
database holds is never touched. Every test runs against both models.

- `repo/scenario_test.go`: business scenarios (deposit and withdraw, hold then
  settle or release, transfer, independent currencies, invalid requests), and
  a check that both models agree after the same operations
- `repo/concurrency_test.go`: concurrent debits and holds never overdraw, a
  hold closes only once, transfers conserve the total, a mixed workload
  matches its ledger, reads see whole operations

A plain `go test ./...` skips the package when the database is unreachable;
`make go.test` sets `TEST_REQUIRE_DB=1` and fails instead.

## Benchmarks

```bash
make go.bench                        # scenarios and hot wallet, 6 to 10 min
BENCH_COUNT=6 make go.bench.scale    # cardinality sweeps, 25 to 40 min
```

Both run the same load against both models (`repo/bench_test.go`): 64
concurrent clients (`BENCH_WORKERS`) over a pool of 32 connections, 2s per
benchmark (`BENCH_TIME`). Each round (`BENCH_COUNT`) is a fresh process on a
fresh seed, with the models in random order. Raw results land in
`bench/scenario.txt` and `bench/scale.txt` (git-ignored) and each target ends
with a benchstat summary: median and 95% confidence interval per model. Run
them one after the other on an otherwise idle machine: they share the
database.

Each benchmark reports wall time per operation across all clients (`sec/op`),
`ops/s`, the `p50-ms` and `p99-ms` latency of one operation, and
`contended/op`, the share of operations the UTXO model gave up on with
`ErrContended`.

### Results

2026-11-15, Intel Core i7-12700 (20 logical CPUs), 62 GiB RAM, EndeavourOS on
Linux 7.2.11, Go 1.27.4, PostgreSQL 18.7 in Docker 29.9.2 with no CPU or
memory limit. The container used host networking instead of the published
port of `docker-compose.yaml`, so connections skipped Docker's port
forwarding. Medians; the ± column is the 95% confidence interval where it is
wider than ±10%.

**Scenarios** (`make go.bench`, 10 rounds), load spread over 1000 wallets:

| scenario     | utxo    | ±   | account | ±   | account vs utxo | utxo ops/s | account ops/s |
| ------------ | ------: | --: | ------: | --: | --------------: | ---------: | ------------: |
| balance      | 3.94µs  |     | 3.52µs  |     | -11%            | 253.6k     | 283.9k        |
| deposit      | 50.1µs  | 32% | 39.0µs  | 49% | -22%            | 19.95k     | 25.63k        |
| withdraw     | 62.5µs  | 48% | 38.5µs  | 51% | -38%            | 16.00k     | 25.98k        |
| payment      | 119.3µs |     | 90.7µs  | 22% | -24%            | 8.38k      | 11.03k        |
| hold-release | 124.0µs |     | 91.2µs  |     | -26%            | 8.07k      | 10.97k        |
| transfer     | 164.8µs |     | 129.1µs |     | -22%            | 6.07k      | 7.75k         |
| mixed        | 57.7µs  |     | 45.4µs  |     | -21%            | 17.34k     | 22.04k        |

`mixed` is 50% balance, 20% payment, 15% transfer, 10% deposit, 5% withdraw.
The wide intervals on deposit, withdraw and payment come from rounds 1 and 2,
which ran 30 to 50% slower than rounds 3 to 10 on both models, on a database
volume created minutes earlier; the later rounds agree within about 3%.

**Hot wallet**, every client paying from one wallet funded by N outputs:

| outputs | utxo   | account | account vs utxo | utxo p50 / p99  | account p50 / p99 |
| ------: | -----: | ------: | --------------: | --------------: | ----------------: |
| 1       | 1.55ms | 1.39ms  | ~ (p=0.105)     | 42.6 / 575.8 ms | 81.4 / 180.6 ms   |
| 64      | 230µs  | 1.39ms  | +503%           | 14.5 / 23.0 ms  | 81.5 / 182.9 ms   |
| 1024    | 447µs  | 1.40ms  | +213%           | 28.6 / 48.8 ms  | 81.9 / 181.6 ms   |

- With the load spread over wallets the account model is 20 to 40% faster on
  every write and 11% faster on reads.
- On one hot wallet the account model serializes at about 720 ops/s whatever
  the wallet holds. The UTXO model reaches 4.3k ops/s with 64 outputs to pick
  from and 2.2k with 1024, where selection and the balance read cost more.
- With a single output the UTXO model is at its worst case: clients chain
  through each other's change. Throughput matches the account model, p99 is
  three times higher (576 ms vs 181 ms) and 0.2% of operations returned
  `ErrContended` (interval ±128%), the only benchmark where any did.

**Cardinality** (`BENCH_COUNT=6 make go.bench.scale`, 6 rounds), the mixed
scenario as the data grows; `Mrows` is the rows in the wallet tables of both
models together:

| level             | Mrows | utxo   | ±   | account | ±   | account vs utxo |
| ----------------- | ----: | -----: | --: | ------: | --: | --------------: |
| wallets=1,000     | 0.14  | 57.7µs |     | 45.0µs  |     | -22%            |
| wallets=10,000    | 0.32  | 59.3µs |     | 45.2µs  | 26% | -24%            |
| wallets=100,000   | 1.40  | 60.2µs |     | 45.8µs  | 56% | ~ (p=0.065)     |
| wallets=1,000,000 | 13.12 | 73.9µs |     | 66.5µs  | 29% | ~ (p=0.065)     |
| outputs=1         | 0.10  | 57.4µs |     | 46.0µs  | 35% | -20%            |
| outputs=10        | 0.17  | 58.7µs |     | 45.3µs  |     | -23%            |
| outputs=100       | 0.40  | 61.9µs |     | 45.5µs  | 22% | -26%            |
| outputs=1,000     | 3.13  | 75.4µs |     | 55.3µs  | 17% | -27%            |
| history=1         | 0.15  | 57.6µs | 13% | 45.6µs  | 11% | -21%            |
| history=100       | 0.69  | 58.1µs |     | 45.5µs  |     | -22%            |
| history=1,000     | 5.11  | 58.7µs |     | 49.7µs  | 25% | ~ (p=0.394)     |

The `outputs` and `history` sweeps run on 1000 wallets; `Mrows` is the utxo
column of the benchstat summary.

- Both models stay flat up to 100k wallets and slow down at 1M (13M rows):
  UTXO by 28%.
- More unspent outputs per wallet cost the UTXO model 31% from 1 to 1000.
  Closed history costs it nothing measurable.
- The account model's medians are noisier here: single rounds ran 20 to 55%
  slow on otherwise flat rows, and at 1M wallets it ranged from 52 to 86µs.
  Six rounds do not separate the models at 100k and 1M wallets or at
  history=1000, and do not show how the account model itself scales past
  100k wallets.
