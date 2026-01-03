-- Create "balance_wallet" table
CREATE TABLE "balance_wallet" (
  "wallet_id" integer NOT NULL,
  "amount" numeric(20,12) NOT NULL DEFAULT 0,
  "created_at" timestamp NOT NULL DEFAULT now(),
  "updated_at" timestamp NOT NULL DEFAULT now(),
  PRIMARY KEY ("wallet_id")
);
-- Create "utxo_wallet" table
CREATE TABLE "utxo_wallet" (
  "tx_id" uuid NOT NULL,
  "wallet_id" integer NOT NULL,
  "amount" numeric(20,12) NOT NULL DEFAULT 0,
  "created_at" timestamp NOT NULL DEFAULT now(),
  "updated_at" timestamp NOT NULL DEFAULT now(),
  "spent" boolean NOT NULL DEFAULT false,
  PRIMARY KEY ("tx_id")
);
-- Create index "utxo_wallet_unspent_wallet_txid" to table: "utxo_wallet"
CREATE INDEX "utxo_wallet_unspent_wallet_txid" ON "utxo_wallet" ("wallet_id", "tx_id") WHERE (spent = false);
