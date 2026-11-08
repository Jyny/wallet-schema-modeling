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
| concurrency, same wallet+currency | serialized on one row lock (hot row)        | parallel on disjoint outputs via `SKIP LOCKED`                  |
| contention failure mode | waits on the row lock                                 | skips locked outputs; when that leaves it short, backs off and retries |
| a long tx holding a lock | blocks every write of that account until it ends    | spends around the locked output; `ErrContended` only if it is needed |
| history / audit         | needs separate entries, kept in sync by the app       | intrinsic: every output records the tx that created and spent it |
| holds                   | `held` column + `account_hold` rows to keep consistent    | a hold *is* an output; no aggregate to keep in sync             |
| row growth              | 1 row per (wallet, currency); entries grow            | outputs grow per operation; needs consolidation / archiving     |
| precision               | identical, `numeric(38,18)` + per-currency `scale`    | identical                                                       |
| balance bound (< 1e20)  | exact, enforced by the column type under the row lock | per transaction: concurrent credits can exceed it (reads stay correct) |

Rule of thumb: A is simpler and reads are O(1); pick it unless a single
wallet's balance is a write hotspot. B trades read cost and storage for
lock-free parallel spends and a self-auditing history.

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
- `Credit` accumulates outputs on one wallet over the run, so its UTXO time
  includes a growing balance read (hence the higher variance).

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
- `contract_test.go`: the same behaviour suite run against both repos,
  including concurrent debits, debits and holds contending for a single output,
  and racing release/settle of one hold
- `model_test.go`: 300 random operations applied to both models must give equal
  balances after every step; account == sum of its entries and active holds; every
  utxo tx conserves value; utxo output shapes for change and holds
