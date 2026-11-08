-- name: GetCurrency :one
SELECT code, scale
FROM currency
WHERE code = $1;
