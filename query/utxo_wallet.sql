-- name: GetUTXOWalletBalanceForUpdate :one
WITH locked AS (
    SELECT *
    FROM utxo_wallet
    WHERE wallet_id = $1
    AND spent_at IS NULL
    FOR UPDATE
)
SELECT
    wallet_id,
    COALESCE(SUM(amount), 0)::NUMERIC(20,12) AS total_amount,
    MAX(created_at) AS last_created_at,
    MAX(created_at) AS last_updated_at
FROM locked
GROUP BY wallet_id;

-- name: MarkUTXOsAsSpent :exec
UPDATE utxo_wallet
SET spent_at = now()
WHERE tx_id = ANY($1::uuid[]);

-- name: InsertUTXO :exec
INSERT INTO utxo_wallet (tx_id, wallet_id, amount, created_at)
VALUES ($1, $2, $3, now());

-- name: InsertUTXOWithoutTxID :exec
INSERT INTO utxo_wallet (wallet_id, amount, created_at)
VALUES ($1, $2, now());

-- name: FindUnspentUTXOsGTETarget :many
WITH RECURSIVE candidate AS (
    SELECT u.tx_id, u.wallet_id, u.amount, u.created_at, u.spent_at,
        u.amount::NUMERIC(20,12) AS acc
    FROM (
        SELECT tx_id, wallet_id, amount, created_at, spent_at
        FROM utxo_wallet
        WHERE utxo_wallet.wallet_id = sqlc.arg(wallet_id)
        AND utxo_wallet.spent_at IS NULL
        ORDER BY utxo_wallet.tx_id
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    ) AS u

    UNION ALL

    SELECT u.tx_id, u.wallet_id, u.amount, u.created_at, u.spent_at,
        (c.acc + u.amount)::NUMERIC(20,12) AS acc
    FROM candidate AS c
    JOIN LATERAL (
        SELECT tx_id, wallet_id, amount, created_at, spent_at
        FROM utxo_wallet
        WHERE utxo_wallet.wallet_id = c.wallet_id
        AND utxo_wallet.spent_at IS NULL
        AND utxo_wallet.tx_id > c.tx_id
        ORDER BY utxo_wallet.tx_id
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    ) AS u ON true
    WHERE c.acc < sqlc.arg(target)::NUMERIC(20,12)
)
SELECT tx_id, wallet_id, amount, created_at, spent_at
FROM candidate;
