package repo

import (
	"context"
	"errors"
	"walletmodeling/model"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrUnknownCurrency   = errors.New("unknown currency")
	ErrInvalidAmount     = errors.New("invalid amount")
	ErrHoldNotFound      = errors.New("hold not found")
	// ErrContended: the funds exist but other transactions keep them locked.
	// only the utxo model, which never waits on a lock, returns it. retry later
	ErrContended = errors.New("funds locked by concurrent operations")
)

// WalletRepo is implemented by both balance models:
//   - AccountWalletRepo: currency as row, one mutable account row per (wallet, currency)
//   - UTXOWalletRepo: balance is the sum of immutable unspent outputs
//
// Amounts are positive and carry at most the currency's scale of fractional
// digits, otherwise ErrInvalidAmount. A wallet needs no registration: one
// without history reads as zero balances.
type WalletRepo interface {
	// GetBalance returns the balance of one currency.
	GetBalance(ctx context.Context, walletID int64, cur model.Currency) (model.Balance, error)
	// ListBalances returns the balance of every supported currency.
	ListBalances(ctx context.Context, walletID int64) ([]model.Balance, error)

	// Credit adds amount to available.
	Credit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error)
	// Debit removes amount from available.
	Debit(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Balance, error)

	// Hold moves amount from available to held.
	Hold(ctx context.Context, walletID int64, cur model.Currency, amount decimal.Decimal) (model.Hold, error)
	// ReleaseHold moves an open hold back to available.
	ReleaseHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error)
	// SettleHold removes an open hold from the wallet.
	SettleHold(ctx context.Context, holdID uuid.UUID) (model.Balance, error)
}

// operation kinds, account_entry.kind and utxo_tx.kind
const (
	kindCredit  = "credit"
	kindDebit   = "debit"
	kindHold    = "hold"
	kindRelease = "release"
	kindSettle  = "settle"
)
