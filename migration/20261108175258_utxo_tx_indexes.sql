-- Create index "utxo_created_tx_idx" to table: "utxo"
CREATE INDEX "utxo_created_tx_idx" ON "utxo" ("created_tx");
-- Create index "utxo_spent_tx_idx" to table: "utxo"
CREATE INDEX "utxo_spent_tx_idx" ON "utxo" ("spent_tx") WHERE (spent_tx IS NOT NULL);
