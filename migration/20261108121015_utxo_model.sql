-- Create "utxo_tx" table
CREATE TABLE "utxo_tx" (
  "tx_id" uuid NOT NULL DEFAULT uuidv7(),
  "wallet_id" bigint NOT NULL,
  "currency" text NOT NULL,
  "kind" text NOT NULL,
  "amount" numeric(38,18) NOT NULL,
  "hold_id" uuid NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("tx_id"),
  CONSTRAINT "utxo_tx_currency_fkey" FOREIGN KEY ("currency") REFERENCES "currency" ("code") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_tx_amount_check" CHECK (amount > (0)::numeric),
  CONSTRAINT "utxo_tx_kind_check" CHECK (kind = ANY (ARRAY['credit'::text, 'debit'::text, 'hold'::text, 'release'::text, 'settle'::text]))
);
-- Create index "utxo_tx_wallet_idx" to table: "utxo_tx"
CREATE INDEX "utxo_tx_wallet_idx" ON "utxo_tx" ("wallet_id", "currency", "tx_id");
-- Create "utxo" table
CREATE TABLE "utxo" (
  "utxo_id" uuid NOT NULL DEFAULT uuidv7(),
  "wallet_id" bigint NOT NULL,
  "currency" text NOT NULL,
  "amount" numeric(38,18) NOT NULL,
  "hold_id" uuid NULL,
  "created_tx" uuid NOT NULL,
  "spent_tx" uuid NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "spent_at" timestamptz NULL,
  PRIMARY KEY ("utxo_id"),
  CONSTRAINT "utxo_hold_id_key" UNIQUE ("hold_id"),
  CONSTRAINT "utxo_created_tx_fkey" FOREIGN KEY ("created_tx") REFERENCES "utxo_tx" ("tx_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_currency_fkey" FOREIGN KEY ("currency") REFERENCES "currency" ("code") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_spent_tx_fkey" FOREIGN KEY ("spent_tx") REFERENCES "utxo_tx" ("tx_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_amount_check" CHECK (amount > (0)::numeric),
  CONSTRAINT "utxo_spent_check" CHECK ((spent_tx IS NULL) = (spent_at IS NULL))
);
-- Create index "utxo_available_idx" to table: "utxo"
CREATE INDEX "utxo_available_idx" ON "utxo" ("wallet_id", "currency", "utxo_id") WHERE ((spent_at IS NULL) AND (hold_id IS NULL));
-- Create index "utxo_created_tx_idx" to table: "utxo"
CREATE INDEX "utxo_created_tx_idx" ON "utxo" ("created_tx");
-- Create index "utxo_spent_tx_idx" to table: "utxo"
CREATE INDEX "utxo_spent_tx_idx" ON "utxo" ("spent_tx") WHERE (spent_tx IS NOT NULL);
-- Create index "utxo_unspent_idx" to table: "utxo"
CREATE INDEX "utxo_unspent_idx" ON "utxo" ("wallet_id", "currency") INCLUDE ("amount", "hold_id") WHERE (spent_at IS NULL);
