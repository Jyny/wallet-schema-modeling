package model

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type UTXOWallet struct {
	TxID      uuid.UUID       `db:"tx_id"`
	WalletID  int             `db:"wallet_id"`
	Amount    decimal.Decimal `db:"amount"`
	Spent     bool            `db:"spent"`
	CreatedAt time.Time       `db:"created_at"`
	UpdatedAt time.Time       `db:"updated_at"`
}

type BalanceWallet struct {
	WalletID  int             `db:"wallet_id"`
	Amount    decimal.Decimal `db:"amount"`
	CreatedAt time.Time       `db:"created_at"`
	UpdatedAt time.Time       `db:"updated_at"`
}
