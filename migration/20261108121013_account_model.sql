-- Create "account" table
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
-- Create "account_hold" table
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
-- Create index "account_hold_active_idx" to table: "account_hold"
CREATE INDEX "account_hold_active_idx" ON "account_hold" ("wallet_id", "currency") WHERE (status = 'active'::text);
-- Create "account_entry" table
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
-- Create index "account_entry_account_idx" to table: "account_entry"
CREATE INDEX "account_entry_account_idx" ON "account_entry" ("wallet_id", "currency", "entry_id");
