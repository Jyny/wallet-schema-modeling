-- Seed initial wallet data
INSERT INTO utxo_wallet (tx_id, wallet_id, amount, created_at, updated_at, spent) VALUES
('0198B7E0-0477-2AF7-76A8-4E6970C6F6E9', 1, 3847283.345238472800, now() - interval '7 days', now() - interval '7 days', false),
('0198BD06-6077-9492-3044-CC52EC4DB575', 2, 4895791.234872358200, now() - interval '6 days', now() - interval '6 days', false),
('0198C22C-BC77-B049-D737-F037FD2AAFD4', 3, 1079465.587928748300, now() - interval '5 days', now() - interval '5 days', false);

INSERT INTO balance_wallet (wallet_id, amount, created_at, updated_at) VALUES
(1, 3847283.345238472800, now() - interval '7 days', now() - interval '7 days'),
(2, 4895791.234872358200, now() - interval '6 days', now() - interval '6 days'),
(3, 1079465.587928748300, now() - interval '5 days', now() - interval '5 days');
