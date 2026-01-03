-- Create utxo_wallet table
CREATE TABLE utxo_wallet (
    tx_id       UUID PRIMARY KEY,
    wallet_id   INT NOT NULL,
    amount      NUMERIC(20,12) NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL DEFAULT now(),
    updated_at  TIMESTAMP NOT NULL DEFAULT now(),
    spent       BOOLEAN NOT NULL DEFAULT false
);

-- Create index for unspent UTXOs lookup
CREATE INDEX utxo_wallet_unspent_wallet_txid
ON utxo_wallet (wallet_id, tx_id)
WHERE spent = false;
