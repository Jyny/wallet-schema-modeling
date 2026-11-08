-- Create "currency" table
CREATE TABLE "currency" (
  "code" text NOT NULL,
  "scale" smallint NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("code"),
  CONSTRAINT "currency_code_format" CHECK (code ~ '^[A-Z0-9]{2,10}$'::text),
  CONSTRAINT "currency_scale_range" CHECK ((scale >= 0) AND (scale <= 18))
);
