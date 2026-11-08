// Command seed writes demo wallets 1..3 into both models through their repos,
// replacing whatever those wallets held, then prints the balances side by side.
//
//	DATABASE_URL=postgres://... go run ./cmd/seed
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"text/tabwriter"
	"walletmodeling/model"
	"walletmodeling/repo"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type op struct {
	kind   string // credit | debit | hold | settle | release
	wallet int64
	cur    model.Currency
	amount string
	ref    string // names a hold, for settle/release to refer to
}

// several credits per currency so the utxo model shows multiple outputs, and
// open holds so both models carry held amounts
var script = []op{
	{kind: "credit", wallet: 1, cur: model.USD, amount: "1500.00"},
	{kind: "credit", wallet: 1, cur: model.USD, amount: "250.75"},
	{kind: "debit", wallet: 1, cur: model.USD, amount: "120.50"},
	{kind: "hold", wallet: 1, cur: model.USD, amount: "300.00", ref: "w1-usd-order"},
	{kind: "credit", wallet: 1, cur: model.USDT, amount: "10000.000000"},
	{kind: "hold", wallet: 1, cur: model.USDT, amount: "2500.125000", ref: "w1-usdt-withdraw"},
	{kind: "credit", wallet: 1, cur: model.BTC, amount: "0.50000000"},
	{kind: "credit", wallet: 1, cur: model.BTC, amount: "0.12345678"},
	{kind: "credit", wallet: 1, cur: model.ETH, amount: "3.141592653589793238"},

	{kind: "credit", wallet: 2, cur: model.USD, amount: "80.00"},
	{kind: "credit", wallet: 2, cur: model.BTC, amount: "1.00000000"},
	{kind: "hold", wallet: 2, cur: model.BTC, amount: "0.25000000", ref: "w2-btc-trade"},
	{kind: "settle", ref: "w2-btc-trade"},
	{kind: "hold", wallet: 2, cur: model.BTC, amount: "0.10000000", ref: "w2-btc-order"},
	{kind: "credit", wallet: 2, cur: model.ETH, amount: "10.000000000000000000"},
	{kind: "credit", wallet: 2, cur: model.ETH, amount: "0.000000000000000001"},
	{kind: "debit", wallet: 2, cur: model.ETH, amount: "1.5"},

	{kind: "credit", wallet: 3, cur: model.USDT, amount: "500.000000"},
	{kind: "credit", wallet: 3, cur: model.USDT, amount: "499.999999"},
	{kind: "hold", wallet: 3, cur: model.USDT, amount: "999.999999", ref: "w3-usdt-cancelled"},
	{kind: "release", ref: "w3-usdt-cancelled"},
	{kind: "hold", wallet: 3, cur: model.USDT, amount: "100.000000", ref: "w3-usdt-order"},
}

var wallets = []int64{1, 2, 3}

func main() {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://user:passwd@localhost:5432/default?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if err := reset(ctx, pool); err != nil {
		log.Fatalf("reset: %v", err)
	}

	repos := []struct {
		name string
		repo repo.WalletRepo
	}{
		{"account", repo.NewAccountWalletRepo(pool)},
		{"utxo", repo.NewUTXOWalletRepo(pool)},
	}
	for _, r := range repos {
		if err := run(ctx, r.repo); err != nil {
			log.Fatalf("seed %s: %v", r.name, err)
		}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "model\twallet\tcurrency\tavailable\theld\t")
	for _, r := range repos {
		for _, w := range wallets {
			bs, err := r.repo.ListBalances(ctx, w)
			if err != nil {
				log.Fatal(err)
			}
			for _, b := range bs {
				if b.Total().IsZero() {
					continue
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t\n", r.name, w, b.Currency, b.Available, b.Held)
			}
		}
	}
	tw.Flush()
}

// reset deletes the seed wallets from both models, children first
func reset(ctx context.Context, pool *pgxpool.Pool) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, table := range []string{"account_entry", "account_hold", "account", "utxo", "utxo_tx"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE wallet_id = ANY($1)", wallets); err != nil {
				return err
			}
		}
		return nil
	})
}

func run(ctx context.Context, r repo.WalletRepo) error {
	holds := map[string]model.Hold{}
	for _, o := range script {
		var err error
		switch o.kind {
		case "credit":
			_, err = r.Credit(ctx, o.wallet, o.cur, decimal.RequireFromString(o.amount))
		case "debit":
			_, err = r.Debit(ctx, o.wallet, o.cur, decimal.RequireFromString(o.amount))
		case "hold":
			holds[o.ref], err = r.Hold(ctx, o.wallet, o.cur, decimal.RequireFromString(o.amount))
		case "settle":
			_, err = r.SettleHold(ctx, holds[o.ref].ID)
		case "release":
			_, err = r.ReleaseHold(ctx, holds[o.ref].ID)
		default:
			err = fmt.Errorf("unknown op %q", o.kind)
		}
		if err != nil {
			return fmt.Errorf("%s %+v: %w", o.kind, o, err)
		}
	}
	return nil
}
