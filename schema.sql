DROP TABLE IF EXISTS utxo_wallet;
CREATE TABLE utxo_wallet (
    tx_id       UUID PRIMARY KEY,
    wallet_id   INT NOT NULL,
    amount      NUMERIC(20,12) NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL DEFAULT now(),
    updated_at  TIMESTAMP NOT NULL DEFAULT now(),
    spent       BOOLEAN NOT NULL DEFAULT false
);

DROP INDEX CONCURRENTLY IF EXISTS utxo_wallet_unspent_wallet_txid;
CREATE INDEX CONCURRENTLY IF NOT EXISTS utxo_wallet_unspent_wallet_txid
ON utxo_wallet (wallet_id, tx_id)
WHERE spent = false;

DROP TABLE IF EXISTS balance_wallet;
CREATE TABLE balance_wallet (
    wallet_id   INT PRIMARY KEY,
    balance     NUMERIC(20,12) NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL DEFAULT now(),
    updated_at  TIMESTAMP NOT NULL DEFAULT now()
);

INSERT INTO utxo_wallet (tx_id, wallet_id, amount, created_at, updated_at, spent) VALUES
('0198B7E0-0477-2AF7-76A8-4E6970C6F6E9', 1,  3847283.345238472800, now() - interval '7 days', now() - interval '7 days', false),
('0198BD06-6077-9492-3044-CC52EC4DB575', 2,  4895791.234872358200, now() - interval '6 days', now() - interval '6 days', false),
('0198C22C-BC77-B049-D737-F037FD2AAFD4', 3,  1079465.587928748300, now() - interval '5 days', now() - interval '5 days', false);

DROP TABLE IF EXISTS balance_wallet;
CREATE TABLE balance_wallet (
    wallet_id   INT PRIMARY KEY,
    amount     NUMERIC(20,12) NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL DEFAULT now(),
    updated_at  TIMESTAMP NOT NULL DEFAULT now()
);

INSERT INTO balance_wallet (wallet_id, amount, created_at, updated_at) VALUES
(1,  3847283.345238472800, now() - interval '7 days', now() - interval '7 days'),
(2,  4895791.234872358200, now() - interval '6 days', now() - interval '6 days'),
(3,  1079465.587928748300, now() - interval '5 days', now() - interval '5 days');