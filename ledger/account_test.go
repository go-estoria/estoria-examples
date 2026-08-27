package main

import (
	"testing"
	"time"

	"github.com/go-estoria/estoria"
)

func TestAccountCommands(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("open requires a holder", func(t *testing.T) {
		t.Parallel()

		if _, err := (Account{}).Open("", now); err == nil {
			t.Error("want an error opening an account without a holder")
		}
	})

	t.Run("open twice is refused", func(t *testing.T) {
		t.Parallel()

		account := openAccount(t, now)

		if _, err := account.Open("Ada", now); err == nil {
			t.Error("want an error re-opening an open account")
		}
	})

	t.Run("deposits and withdrawals fold into the balance", func(t *testing.T) {
		t.Parallel()

		account := openAccount(t, now)
		account = mustApply(t, account)(account.Deposit(1000, now))
		account = mustApply(t, account)(account.Withdraw(300, now))

		if account.Balance != 700 {
			t.Errorf("want balance 700, got %d", account.Balance)
		}
	})

	t.Run("overdraft is refused", func(t *testing.T) {
		t.Parallel()

		account := openAccount(t, now)
		account = mustApply(t, account)(account.Deposit(100, now))

		if _, err := account.Withdraw(200, now); err == nil {
			t.Error("want an error withdrawing more than the balance")
		}
	})

	t.Run("nonpositive amounts are refused", func(t *testing.T) {
		t.Parallel()

		account := openAccount(t, now)

		if _, err := account.Deposit(0, now); err == nil {
			t.Error("want an error depositing zero")
		}

		if _, err := account.Withdraw(-5, now); err == nil {
			t.Error("want an error withdrawing a negative amount")
		}
	})

	t.Run("commands on an unopened account are refused", func(t *testing.T) {
		t.Parallel()

		if _, err := (Account{}).Deposit(100, now); err == nil {
			t.Error("want an error depositing into an unopened account")
		}
	})
}

// openAccount folds a fresh opened account for a test to build on.
func openAccount(t *testing.T, now time.Time) Account {
	t.Helper()

	event, err := Account{}.Open("Ada", now)
	if err != nil {
		t.Fatalf("opening account: %v", err)
	}

	return event.ApplyTo(Account{})
}

// mustApply folds a command's event into the account, failing the test if
// the command was refused.
func mustApply(t *testing.T, account Account) func(estoria.DomainEvent[Account], error) Account {
	t.Helper()

	return func(event estoria.DomainEvent[Account], err error) Account {
		if err != nil {
			t.Fatalf("command refused: %v", err)
		}

		return event.ApplyTo(account)
	}
}
