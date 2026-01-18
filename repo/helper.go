package repo

import (
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
)

func decimalToNumeric(d decimal.Decimal) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(d.String())
	return n
}

func numericToDecimal(n pgtype.Numeric) (decimal.Decimal, error) {
	if !n.Valid {
		return decimal.Zero, nil
	}
	return decimal.NewFromBigInt(n.Int, n.Exp), nil
}

