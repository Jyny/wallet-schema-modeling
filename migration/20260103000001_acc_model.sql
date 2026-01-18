-- Create acc_wallet table
CREATE TABLE acc_wallet (
    wallet_id   INT PRIMARY KEY,
    amount      NUMERIC(20,12) NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL DEFAULT now(),
    updated_at  TIMESTAMP NOT NULL DEFAULT now()
);
