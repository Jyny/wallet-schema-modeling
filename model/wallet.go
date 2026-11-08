package model

import (
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Currency is a currency code, see the currency table for the supported set.
type Currency string

const (
	USD  Currency = "USD"
	USDT Currency = "USDT"
	BTC  Currency = "BTC"
	ETH  Currency = "ETH"
)

// Balance of one currency in a wallet.
type Balance struct {
	WalletID  int64
	Currency  Currency
	Available decimal.Decimal // spendable
	Held      decimal.Decimal // locked by open holds
}

// Total is what the wallet owns: available plus held.
func (b Balance) Total() decimal.Decimal {
	return b.Available.Add(b.Held)
}

// Hold locks an amount until it is released back to available or settled
// out of the wallet.
type Hold struct {
	ID       uuid.UUID
	WalletID int64
	Currency Currency
	Amount   decimal.Decimal
}
