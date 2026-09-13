package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

type SessionOutboundWindowInput struct {
	Number    uint64
	ContextID string
}

func (input SessionOutboundWindowInput) key() string {
	if input.ContextID != "" {
		return "context:" + input.ContextID
	}
	return "number:" + strconv.FormatUint(input.Number, 10)
}

type SessionOutboundWindowEntry struct {
	Original uint64 `json:"original"`
	Number   uint64 `json:"number"`
}

type SessionOutboundWindowState struct {
	Next          uint64                                `json:"next"`
	LegacyBase    *uint64                               `json:"legacy_base,omitempty"`
	LegacyMaximum uint64                                `json:"legacy_maximum,omitempty"`
	Entries       map[string]SessionOutboundWindowEntry `json:"entries"`
}

func (db *DB) ResolveSessionOutboundWindowNumbers(ctx context.Context, rootKey string, accountID int64, generation uint64, inputs map[string]SessionOutboundWindowInput) (map[string]uint64, error) {
	numbers := make(map[string]uint64, len(inputs))
	err := db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=state WHERE root_key=$1`, rootKey); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM codex_session_continuity WHERE root_key=$1`, rootKey).Scan(&raw); err != nil {
			return err
		}
		var record SessionContinuityRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return err
		}
		if !record.OutboundWindowReset || record.AccountID != accountID || record.FailoverCount != generation {
			return ErrSessionOwnerConflict
		}
		if record.OutboundWindows == nil {
			record.OutboundWindows = make(map[string]*SessionOutboundWindowState)
		}
		changed := false
		for thread, input := range inputs {
			if thread == "" || len(thread) > 128 || len(input.ContextID) > 128 {
				return errors.New("invalid outbound window identity")
			}
			state := record.OutboundWindows[thread]
			if state == nil {
				if len(record.OutboundWindows) >= 1024 {
					return errors.New("outbound window thread limit exceeded")
				}
				state = &SessionOutboundWindowState{Entries: make(map[string]SessionOutboundWindowEntry)}
				if base, exists := record.OutboundWindowBases[thread]; exists && record.OutboundWindowMode == "" {
					maximum := max(base, input.Number)
					if thread == record.ThreadID && record.NumberKnown {
						maximum = max(maximum, record.Number)
					}
					if maximum-base == ^uint64(0) {
						return errors.New("outbound window sequence exhausted")
					}
					state.LegacyBase, state.LegacyMaximum, state.Next = &base, maximum, maximum-base+1
				}
				record.OutboundWindows[thread] = state
				changed = true
			}
			key := input.key()
			if entry, exists := state.Entries[key]; exists {
				if entry.Original != input.Number {
					return errors.New("context window number is inconsistent")
				}
				numbers[thread] = entry.Number
				continue
			}
			if len(state.Entries) >= 4096 || state.Next == ^uint64(0) {
				return errors.New("outbound window sequence limit exceeded")
			}
			if input.ContextID != "" {
				numericKey := (SessionOutboundWindowInput{Number: input.Number}).key()
				if entry, exists := state.Entries[numericKey]; exists {
					delete(state.Entries, numericKey)
					state.Entries[key] = entry
					numbers[thread], changed = entry.Number, true
					continue
				}
			} else {
				var candidate *SessionOutboundWindowEntry
				for _, entry := range state.Entries {
					if entry.Original == input.Number {
						if candidate != nil {
							return errors.New("context window identity required for reused number")
						}
						copy := entry
						candidate = &copy
					}
				}
				if candidate != nil {
					numbers[thread] = candidate.Number
					continue
				}
			}
			number := state.Next
			if state.LegacyBase != nil && input.Number >= *state.LegacyBase && input.Number <= state.LegacyMaximum {
				number = input.Number - *state.LegacyBase
				for identity, entry := range state.Entries {
					if entry.Number == number && identity != key {
						number = state.Next
						break
					}
				}
			}
			if state.Entries == nil {
				state.Entries = make(map[string]SessionOutboundWindowEntry)
			}
			state.Entries[key] = SessionOutboundWindowEntry{Original: input.Number, Number: number}
			state.Next = max(state.Next, number+1)
			numbers[thread], changed = number, true
		}
		if !changed {
			return nil
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=$2 WHERE root_key=$1`, rootKey, string(payload))
		return err
	})
	return numbers, err
}
