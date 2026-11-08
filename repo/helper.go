package repo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"walletmodeling/model"
	"walletmodeling/repo/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// maxAmount bounds amounts and balances: numeric(38,18) has 20 integer digits
var maxAmount = decimal.New(1, 20)

// balanceRow has the field set shared by every sqlc balance row type, which
// convert to it directly.
type balanceRow struct {
	Currency  string
	Available decimal.Decimal
	Held      decimal.Decimal
}

func toBalance(walletID int64, r balanceRow) model.Balance {
	return model.Balance{
		WalletID:  walletID,
		Currency:  model.Currency(r.Currency),
		Available: r.Available,
		Held:      r.Held,
	}
}

// currencyCode is the format the currency table's CHECK allows
var currencyCode = regexp.MustCompile(`^[A-Z0-9]{2,10}$`)

// checkCurrencyCode rejects a malformed code before it reaches the database,
// which would fail on some (a NUL byte, invalid UTF-8) instead of not finding it
func checkCurrencyCode(cur model.Currency) error {
	if !currencyCode.MatchString(string(cur)) {
		return fmt.Errorf("%w: %q", ErrUnknownCurrency, cur)
	}
	return nil
}

// validateAmount checks amount against the currency, which must exist.
func validateAmount(ctx context.Context, q *db.Queries, cur model.Currency, amount decimal.Decimal) error {
	if err := checkCurrencyCode(cur); err != nil {
		return err
	}
	c, err := q.GetCurrency(ctx, string(cur))
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownCurrency, cur)
	}
	if err != nil {
		return err
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: %s must be positive", ErrInvalidAmount, amount)
	}
	if amount.GreaterThanOrEqual(maxAmount) {
		return fmt.Errorf("%w: %s must be below %s", ErrInvalidAmount, amount, maxAmount)
	}
	if !amount.Equal(amount.Truncate(int32(c.Scale))) {
		return fmt.Errorf("%w: %s exceeds %s scale %d", ErrInvalidAmount, amount, cur, c.Scale)
	}
	return nil
}

// checkBound rejects a balance at or above maxAmount. the account model gets
// this from its numeric(38,18) columns, the utxo model, whose outputs fit the
// column individually but not necessarily in sum, checks it explicitly.
func checkBound(b model.Balance) error {
	if b.Available.GreaterThanOrEqual(maxAmount) || b.Held.GreaterThanOrEqual(maxAmount) {
		return fmt.Errorf("%w: balance would exceed %s", ErrInvalidAmount, maxAmount)
	}
	return nil
}

// translate maps database errors with a meaning in the WalletRepo contract to
// its sentinel errors.
func translate(err error) error {
	var pgErr *pgconn.PgError
	// numeric_value_out_of_range: the resulting balance exceeds maxAmount
	if errors.As(err, &pgErr) && pgErr.Code == "22003" {
		return fmt.Errorf("%w: balance would exceed %s: %w", ErrInvalidAmount, maxAmount, err)
	}
	return err
}
