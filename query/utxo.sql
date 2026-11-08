-- model B: utxo based
--
-- sums are unbounded numeric, not numeric(38,18): each output fits the column,
-- but concurrent credits can push their sum past it (the 1e20 balance bound is
-- checked by the repo, per transaction), and a read must never fail on that

-- name: UTXOGetBalance :one
-- a wallet without outputs reads as zero, a missing currency as no rows
SELECT
  c.code AS currency,
  COALESCE(SUM(u.amount) FILTER (WHERE u.hold_id IS NULL), 0)::numeric AS available,
  COALESCE(SUM(u.amount) FILTER (WHERE u.hold_id IS NOT NULL), 0)::numeric AS held
FROM currency AS c
LEFT JOIN utxo AS u
  ON u.currency = c.code AND u.wallet_id = sqlc.arg(wallet_id) AND u.spent_at IS NULL
WHERE c.code = sqlc.arg(currency)
GROUP BY c.code;

-- name: UTXOListBalances :many
SELECT
  c.code AS currency,
  COALESCE(SUM(u.amount) FILTER (WHERE u.hold_id IS NULL), 0)::numeric AS available,
  COALESCE(SUM(u.amount) FILTER (WHERE u.hold_id IS NOT NULL), 0)::numeric AS held
FROM currency AS c
LEFT JOIN utxo AS u
  ON u.currency = c.code AND u.wallet_id = sqlc.arg(wallet_id) AND u.spent_at IS NULL
GROUP BY c.code
ORDER BY c.code;

-- name: UTXOInsertTx :exec
INSERT INTO utxo_tx (tx_id, wallet_id, currency, kind, amount, hold_id)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UTXOInsertOutput :exec
INSERT INTO utxo (wallet_id, currency, amount, hold_id, created_tx)
VALUES ($1, $2, $3, $4, $5);

-- name: UTXOSelectInputs :many
-- coin selection: walk available outputs oldest first, locking each one, until
-- their running sum reaches the target. outputs locked by a concurrent
-- operation are skipped instead of waited for, so concurrent spends of one
-- wallet pick disjoint outputs and never stall on a lock. the caller checks
-- the sum: a short result means insufficient funds, or funds that concurrent
-- operations hold locked (see UTXOAvailable)
WITH RECURSIVE selected AS (
  SELECT u.utxo_id, u.wallet_id, u.currency, u.amount,
    u.amount::numeric AS running
  FROM (
    SELECT utxo_id, wallet_id, currency, amount
    FROM utxo
    WHERE utxo.wallet_id = sqlc.arg(wallet_id) AND utxo.currency = sqlc.arg(currency)
    AND utxo.spent_at IS NULL AND utxo.hold_id IS NULL
    ORDER BY utxo.utxo_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
  ) AS u

  UNION ALL

  SELECT u.utxo_id, u.wallet_id, u.currency, u.amount,
    (s.running + u.amount)::numeric AS running
  FROM selected AS s
  JOIN LATERAL (
    SELECT utxo_id, wallet_id, currency, amount
    FROM utxo
    WHERE utxo.wallet_id = s.wallet_id AND utxo.currency = s.currency
    AND utxo.spent_at IS NULL AND utxo.hold_id IS NULL
    AND utxo.utxo_id > s.utxo_id
    ORDER BY utxo.utxo_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
  ) AS u ON true
  WHERE s.running < sqlc.arg(target)::numeric
)
SELECT utxo_id, amount FROM selected;

-- name: UTXOAvailable :one
-- available sum without locking: tells insufficient funds from funds that
-- concurrent operations hold locked
SELECT COALESCE(SUM(amount), 0)::numeric AS available
FROM utxo
WHERE wallet_id = $1 AND currency = $2 AND spent_at IS NULL AND hold_id IS NULL;

-- name: UTXOGetHeldForUpdate :one
-- the output locked by an open hold. no rows when the hold is unknown or closed
SELECT utxo_id, wallet_id, currency, amount
FROM utxo
WHERE hold_id = $1 AND spent_at IS NULL
FOR UPDATE;

-- name: UTXOSpend :execrows
UPDATE utxo
SET spent_tx = sqlc.arg(spent_tx), spent_at = now()
WHERE utxo_id = ANY(sqlc.arg(utxo_ids)::uuid[]) AND spent_at IS NULL;
