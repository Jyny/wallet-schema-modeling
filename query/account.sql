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

-- every write is one statement: the account row update and its entry (and
-- for holds the hold row) are data-modifying CTEs of a single query, so the
-- row stays locked from that statement until COMMIT, one round trip, rather
-- than across a statement per table. a guard that fails leaves the first CTE
-- empty, nothing is written and the query returns no rows

-- name: AccountCredit :one
-- available += amount, creating the row on first credit
WITH b AS (
  INSERT INTO account (wallet_id, currency, available)
  VALUES (sqlc.arg(wallet_id), sqlc.arg(currency), sqlc.arg(amount)::numeric)
  ON CONFLICT (wallet_id, currency) DO UPDATE
  SET available = account.available + EXCLUDED.available, updated_at = now()
  RETURNING wallet_id, currency, available, held
), e AS (
  INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta)
  SELECT wallet_id, currency, 'credit', sqlc.arg(amount)::numeric, 0 FROM b
)
SELECT currency, available, held FROM b;

-- name: AccountDebit :one
-- available -= amount. no rows when funds are insufficient
WITH b AS (
  UPDATE account AS a
  SET available = a.available - sqlc.arg(amount)::numeric, updated_at = now()
  WHERE a.wallet_id = sqlc.arg(wallet_id) AND a.currency = sqlc.arg(currency)
  AND a.available >= sqlc.arg(amount)::numeric
  RETURNING a.wallet_id, a.currency, a.available, a.held
), e AS (
  INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta)
  SELECT wallet_id, currency, 'debit', -sqlc.arg(amount)::numeric, 0 FROM b
)
SELECT currency, available, held FROM b;

-- name: AccountHold :one
-- available -> held, recorded as an active hold. no rows when funds are
-- insufficient
WITH b AS (
  UPDATE account AS a
  SET
    available = a.available - sqlc.arg(amount)::numeric,
    held = a.held + sqlc.arg(amount)::numeric,
    updated_at = now()
  WHERE a.wallet_id = sqlc.arg(wallet_id) AND a.currency = sqlc.arg(currency)
  AND a.available >= sqlc.arg(amount)::numeric
  RETURNING a.wallet_id, a.currency, a.available, a.held
), h AS (
  INSERT INTO account_hold (hold_id, wallet_id, currency, amount)
  SELECT sqlc.arg(hold_id)::uuid, wallet_id, currency, sqlc.arg(amount)::numeric FROM b
), e AS (
  INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta, hold_id)
  SELECT wallet_id, currency, 'hold', -sqlc.arg(amount)::numeric, sqlc.arg(amount)::numeric, sqlc.arg(hold_id)::uuid
  FROM b
)
SELECT currency, available, held FROM b;

-- name: AccountReleaseHold :one
-- active hold -> released, held -> available. no rows when the hold is
-- unknown or closed: a concurrent close of the same hold waits on its row,
-- then finds it closed
WITH h AS (
  UPDATE account_hold AS ah
  SET status = 'released', updated_at = now()
  WHERE ah.hold_id = sqlc.arg(hold_id) AND ah.status = 'active'
  RETURNING ah.hold_id, ah.wallet_id, ah.currency, ah.amount
), b AS (
  UPDATE account AS a
  SET available = a.available + h.amount, held = a.held - h.amount, updated_at = now()
  FROM h
  WHERE a.wallet_id = h.wallet_id AND a.currency = h.currency
  RETURNING a.wallet_id, a.currency, a.available, a.held, h.amount, h.hold_id
), e AS (
  INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta, hold_id)
  SELECT wallet_id, currency, 'release', amount, -amount, hold_id FROM b
)
SELECT wallet_id, currency, available, held FROM b;

-- name: AccountSettleHold :one
-- active hold -> settled, held -> out of the wallet. no rows when the hold is
-- unknown or closed
WITH h AS (
  UPDATE account_hold AS ah
  SET status = 'settled', updated_at = now()
  WHERE ah.hold_id = sqlc.arg(hold_id) AND ah.status = 'active'
  RETURNING ah.hold_id, ah.wallet_id, ah.currency, ah.amount
), b AS (
  UPDATE account AS a
  SET held = a.held - h.amount, updated_at = now()
  FROM h
  WHERE a.wallet_id = h.wallet_id AND a.currency = h.currency
  RETURNING a.wallet_id, a.currency, a.available, a.held, h.amount, h.hold_id
), e AS (
  INSERT INTO account_entry (wallet_id, currency, kind, available_delta, held_delta, hold_id)
  SELECT wallet_id, currency, 'settle', 0, -amount, hold_id FROM b
)
SELECT wallet_id, currency, available, held FROM b;
