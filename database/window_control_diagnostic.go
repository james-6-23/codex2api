package database

import "time"

// WindowControlDiagnostic contains decision-time facts, never credentials,
// authorization tickets, raw session identities, or request bodies.
type WindowControlDiagnostic struct {
	ObservedAt          time.Time                          `json:"observed_at"`
	RootHash            string                             `json:"root_hash,omitempty"`
	OwnerSource         string                             `json:"owner_source,omitempty"`
	OwnerAccountID      int64                              `json:"owner_account_id,omitempty"`
	OwnerLastSeen       time.Time                          `json:"owner_last_seen,omitzero"`
	OwnerLastCompleted  time.Time                          `json:"owner_last_completed,omitzero"`
	OwnerLastStatus     int                                `json:"owner_last_status,omitempty"`
	AllowExpansion      bool                               `json:"allow_expansion"`
	ExtraLimit          int                                `json:"extra_limit"`
	Multiplier          float64                            `json:"multiplier"`
	UserLimit           int                                `json:"user_limit"`
	WindowSeconds       int                                `json:"window_seconds"`
	ActiveWindows       int                                `json:"active_windows"`
	RootWindowState     string                             `json:"root_window_state"`
	RootWindowExpiresAt time.Time                          `json:"root_window_expires_at,omitzero"`
	Grant               *WindowGrantDiagnostic             `json:"grant,omitempty"`
	Account             *AccountSessionAdmissionDiagnostic `json:"account,omitempty"`
	CountsEvaluated     bool                               `json:"counts_evaluated"`
	OrdinaryUsed        int                                `json:"ordinary_used"`
	ExpandedUsed        int                                `json:"expanded_used"`
	NeedsExpansion      bool                               `json:"needs_expansion"`
	Decision            string                             `json:"decision,omitempty"`
	ExpansionBlock      string                             `json:"expansion_block,omitempty"`
}

type WindowGrantDiagnostic struct {
	State          string    `json:"state"`
	Confirmed      bool      `json:"confirmed"`
	Expanded       bool      `json:"expanded"`
	CreatedAt      time.Time `json:"created_at,omitzero"`
	ExpiresAt      time.Time `json:"expires_at,omitzero"`
	PendingUntil   time.Time `json:"pending_until,omitzero"`
	OwnerAccountID int64     `json:"owner_account_id,omitempty"`
}

// Counts and the slot state are captured under the same lock as admission.
// "missing" means no entry was observed, not proof that one never existed.
type AccountSessionAdmissionDiagnostic struct {
	Reason         string    `json:"reason"`
	Enabled        bool      `json:"enabled"`
	TotalLimit     int64     `json:"total_limit"`
	ReservedLimit  int64     `json:"reserved_limit"`
	IdleTTLSeconds int64     `json:"idle_ttl_seconds"`
	TotalUsed      int64     `json:"total_used"`
	ReservedUsed   int64     `json:"reserved_used"`
	SlotState      string    `json:"slot_state"`
	SlotReserved   bool      `json:"slot_reserved"`
	LastSeen       time.Time `json:"last_seen,omitzero"`
	ExpiresAt      time.Time `json:"expires_at,omitzero"`
}

func cloneWindowControlDiagnostic(source *WindowControlDiagnostic) *WindowControlDiagnostic {
	if source == nil {
		return nil
	}
	result := *source
	for _, field := range []*string{&result.RootHash, &result.OwnerSource, &result.RootWindowState, &result.Decision, &result.ExpansionBlock} {
		*field = serviceErrorString(*field, 80)
	}
	if source.Grant != nil {
		grant := *source.Grant
		grant.State = serviceErrorString(grant.State, 80)
		result.Grant = &grant
	}
	if source.Account != nil {
		account := *source.Account
		account.Reason = serviceErrorString(account.Reason, 80)
		account.SlotState = serviceErrorString(account.SlotState, 80)
		result.Account = &account
	}
	return &result
}
