-- model A: currency as row

-- name: AccountGetBalance :one
-- a missing account row reads as zero, a missing currency as no rows
SELECT
  c.code AS currency,
  COALESCE(b.available, 0)::numeric(38,18) AS available,
  COALESCE(b.held, 0)::numeric(38,18) AS held
FROM currency AS c
LEFT JOIN account AS b
  ON b.currency = c.code AND b.wallet_id = sqlc.arg(wallet_id)
WHERE c.code = sqlc.arg(currency);

-- name: AccountListBalances :many
SELECT
  c.code AS currency,
  COALESCE(b.available, 0)::numeric(38,18) AS available,
  COALESCE(b.held, 0)::numeric(38,18) AS held
FROM currency AS c
LEFT JOIN account AS b
  ON b.currency = c.code AND b.wallet_id = sqlc.arg(wallet_id)
ORDER BY c.code;

-- name: AccountCredit :one
-- available += amount, creating the row on first credit
INSERT INTO account (wallet_id, currency, available)
VALUES (sqlc.arg(wallet_id), sqlc.arg(currency), sqlc.arg(amount))
ON CONFLICT (wallet_id, currency) DO UPDATE
SET available = account.available + EXCLUDED.available, updated_at = now()
RETURNING currency, available, held;

-- name: AccountDebit :one
-- available -= amount. no rows when funds are insufficient
UPDATE account
SET available = available - sqlc.arg(amount)::numeric, updated_at = now()
WHERE wallet_id = sqlc.arg(wallet_id) AND currency = sqlc.arg(currency)
AND available >= sqlc.arg(amount)::numeric
RETURNING currency, available, held;

-- name: AccountHold :one
-- available -> held. no rows when funds are insufficient
UPDATE account
SET
  available = available - sqlc.arg(amount)::numeric,
  held = held + sqlc.arg(amount)::numeric,
  updated_at = now()
WHERE wallet_id = sqlc.arg(wallet_id) AND currency = sqlc.arg(currency)
AND available >= sqlc.arg(amount)::numeric
RETURNING currency, available, held;

-- name: AccountRelease :one
-- held -> available
UPDATE account
SET
  available = available + sqlc.arg(amount)::numeric,
  held = held - sqlc.arg(amount)::numeric,
  updated_at = now()
WHERE wallet_id = sqlc.arg(wallet_id) AND currency = sqlc.arg(currency)
RETURNING currency, available, held;

-- name: AccountSettle :one
-- held -> out of the wallet
UPDATE account
SET held = held - sqlc.arg(amount)::numeric, updated_at = now()
WHERE wallet_id = sqlc.arg(wallet_id) AND currency = sqlc.arg(currency)
RETURNING currency, available, held;

-- name: AccountInsertHold :exec
INSERT INTO account_hold (hold_id, wallet_id, currency, amount)
VALUES ($1, $2, $3, $4);

-- name: AccountCloseHold :one
-- active -> released | settled. no rows when the hold is unknown or closed
UPDATE account_hold
SET status = sqlc.arg(status), updated_at = now()
WHERE hold_id = sqlc.arg(hold_id) AND status = 'active'
RETURNING wallet_id, currency, amount;

-- name: AccountInsertEntry :exec
INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta, hold_id)
VALUES ($1, $2, $3, $4, $5, $6);
