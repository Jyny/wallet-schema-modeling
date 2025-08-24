package svc

import (
	"context"

	"github.com/shopspring/decimal"
)

type WalletSvc interface {
	GetWalletBalance(ctx context.Context, walletID int) (decimal.Decimal, error)
	UpdateWalletBalance(ctx context.Context, walletID int, delta decimal.Decimal) error
}
