-- name: GetAccWalletForUpdate :one
SELECT wallet_id, amount, created_at, updated_at
FROM acc_wallet
WHERE wallet_id = $1
FOR UPDATE;

-- name: UpdateAccWalletBalance :one
UPDATE acc_wallet
SET amount = amount + $1, updated_at = now()
WHERE wallet_id = $2
AND amount + $1 >= 0
RETURNING *;
