package model

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func TestBalanceTotal(t *testing.T) {
	b := Balance{
		Available: decimal.RequireFromString("1.000000000000000001"),
		Held:      decimal.RequireFromString("2.5"),
	}
	assert.Equal(t, "3.500000000000000001", b.Total().String())
}
