package database

import "errors"

const DefaultAccountSessionCapacity = int64(5)

var ErrInvalidSessionReserve = errors.New("session_capacity_reserved must be between zero and the account's session_capacity_max")

func validateSessionCapacityCredentialUpdate(merged, updates map[string]interface{}) error {
	_, reserveChanged := updates["session_capacity_reserved"]
	_, maximumChanged := updates["session_capacity_max"]
	if !reserveChanged && !maximumChanged {
		return nil
	}
	row := AccountRow{Credentials: merged}
	maximum, _ := row.GetCredentialInt64("session_capacity_max")
	if maximum <= 0 {
		maximum = DefaultAccountSessionCapacity
	}
	reserved, _ := row.GetCredentialInt64("session_capacity_reserved")
	if reserveChanged && (reserved < 0 || reserved > maximum) {
		return ErrInvalidSessionReserve
	}
	if !reserveChanged && maximumChanged && reserved > maximum {
		merged["session_capacity_reserved"] = maximum
	}
	return nil
}
