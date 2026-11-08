-- shared reference data for both models
CREATE TABLE "currency" (
  "code" text NOT NULL,
  -- max fractional digits accepted for an amount, e.g. USD 2, BTC 8, ETH 18
  "scale" smallint NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("code"),
  CONSTRAINT "currency_code_format" CHECK (code ~ '^[A-Z0-9]{2,10}$'),
  -- amounts are stored as numeric(38,18)
  CONSTRAINT "currency_scale_range" CHECK ((scale >= 0) AND (scale <= 18))
);
-- model A: currency as row. one mutable account row per (wallet, currency)
CREATE TABLE "account" (
  "wallet_id" bigint NOT NULL,
  "currency" text NOT NULL,
  "available" numeric(38,18) NOT NULL DEFAULT 0,
  "held" numeric(38,18) NOT NULL DEFAULT 0,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("wallet_id", "currency"),
  CONSTRAINT "account_currency_fkey" FOREIGN KEY ("currency") REFERENCES "currency" ("code") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "account_available_check" CHECK (available >= (0)::numeric),
  CONSTRAINT "account_held_check" CHECK (held >= (0)::numeric)
);
-- model A: one row per hold, account.held is the sum of its active holds
CREATE TABLE "account_hold" (
  "hold_id" uuid NOT NULL,
  "wallet_id" bigint NOT NULL,
  "currency" text NOT NULL,
  "amount" numeric(38,18) NOT NULL,
  "status" text NOT NULL DEFAULT 'active',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("hold_id"),
  CONSTRAINT "account_hold_account_fkey" FOREIGN KEY ("wallet_id", "currency") REFERENCES "account" ("wallet_id", "currency") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "account_hold_amount_check" CHECK (amount > (0)::numeric),
  CONSTRAINT "account_hold_status_check" CHECK (status = ANY (ARRAY['active'::text, 'released'::text, 'settled'::text]))
);
CREATE INDEX "account_hold_active_idx" ON "account_hold" ("wallet_id", "currency") WHERE (status = 'active'::text);
-- model A: append-only ledger entries, the history the account row overwrites
CREATE TABLE "account_entry" (
  "entry_id" uuid NOT NULL DEFAULT uuidv7(),
  "wallet_id" bigint NOT NULL,
  "currency" text NOT NULL,
  "kind" text NOT NULL,
  "available_delta" numeric(38,18) NOT NULL,
  "held_delta" numeric(38,18) NOT NULL,
  "hold_id" uuid NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("entry_id"),
  CONSTRAINT "account_entry_account_fkey" FOREIGN KEY ("wallet_id", "currency") REFERENCES "account" ("wallet_id", "currency") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "account_entry_hold_fkey" FOREIGN KEY ("hold_id") REFERENCES "account_hold" ("hold_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "account_entry_kind_check" CHECK (kind = ANY (ARRAY['credit'::text, 'debit'::text, 'hold'::text, 'release'::text, 'settle'::text]))
);
CREATE INDEX "account_entry_account_idx" ON "account_entry" ("wallet_id", "currency", "entry_id");
-- model B: one row per balance-changing operation, groups the outputs it
-- spends and creates
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
CREATE INDEX "utxo_tx_wallet_idx" ON "utxo_tx" ("wallet_id", "currency", "tx_id");
-- model B: immutable outputs. balance = sum of unspent outputs, an output with
-- a hold_id is held (locked), one without is available. only spent_* is ever
-- updated, exactly once
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
  -- a hold locks exactly one output, release/settle spend it
  CONSTRAINT "utxo_hold_id_key" UNIQUE ("hold_id"),
  CONSTRAINT "utxo_created_tx_fkey" FOREIGN KEY ("created_tx") REFERENCES "utxo_tx" ("tx_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_currency_fkey" FOREIGN KEY ("currency") REFERENCES "currency" ("code") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_spent_tx_fkey" FOREIGN KEY ("spent_tx") REFERENCES "utxo_tx" ("tx_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "utxo_amount_check" CHECK (amount > (0)::numeric),
  CONSTRAINT "utxo_spent_check" CHECK ((spent_tx IS NULL) = (spent_at IS NULL))
);
-- coin selection: available outputs in creation order
CREATE INDEX "utxo_available_idx" ON "utxo" ("wallet_id", "currency", "utxo_id") WHERE ((spent_at IS NULL) AND (hold_id IS NULL));
-- balance: index-only sum over unspent outputs
CREATE INDEX "utxo_unspent_idx" ON "utxo" ("wallet_id", "currency") INCLUDE ("amount", "hold_id") WHERE (spent_at IS NULL);
