package database

import (
	"errors"
	"testing"
)

func TestSessionReservePartialUpdateRespectsExistingAccountMaximum(test *testing.T) {
	merged := map[string]interface{}{"session_capacity_max": int64(2), "session_capacity_reserved": int64(3)}
	err := validateSessionCapacityCredentialUpdate(merged, map[string]interface{}{"session_capacity_reserved": int64(3)})
	if !errors.Is(err, ErrInvalidSessionReserve) {
		test.Fatal("reserve-only update exceeded current maximum")
	}
	if err := validateSessionCapacityCredentialUpdate(merged, map[string]interface{}{"session_capacity_max": int64(2)}); err != nil {
		test.Fatal(err)
	}
	if merged["session_capacity_reserved"] != int64(2) {
		test.Fatal("maximum reduction did not clamp stored reserve")
	}
}
