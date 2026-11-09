-- reference data shared by both models. scale is the number of fractional
-- digits an amount may carry (USD cents, USDT 6 like the ERC-20 token,
-- BTC satoshi, ETH wei)
INSERT INTO "currency" ("code", "scale") VALUES
  ('USD', 2),
  ('USDT', 6),
  ('BTC', 8),
  ('ETH', 18);
