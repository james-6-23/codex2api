package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CodexInviteRecipientStateReserved     = "reserved"
	CodexInviteRecipientStateSent         = "sent"
	CodexInviteRecipientStateKnownInvited = "known_invited"
	CodexInviteRecipientStateUnknown      = "unknown"
)

// ErrCodexInviteRecipientReservationLost means a completion tried to use a
// reservation that no longer owns the recipient rows. Callers must not turn
// this into an unconditional retry, because another request may own the email.
var ErrCodexInviteRecipientReservationLost = errors.New("Codex invite recipient reservation lost")

// CodexInviteRecipient is the durable, global at-most-once ledger for an
// invitation recipient. EmailKey is the case-insensitive identity; Email keeps
// the first useful display spelling. There is deliberately no cascading
// account foreign key: deleting the sender must not make the recipient
// eligible to be invited again.
type CodexInviteRecipient struct {
	EmailKey                string     `json:"-"`
	Email                   string     `json:"email"`
	SenderAccountID         int64      `json:"sender_account_id,omitempty"`
	ProgramID               string     `json:"program_id,omitempty"`
	Entrypoint              string     `json:"entrypoint,omitempty"`
	State                   string     `json:"state"`
	ReservationID           string     `json:"-"`
	RequestID               string     `json:"request_id,omitempty"`
	ReferralID              string     `json:"referral_id,omitempty"`
	InviteURL               string     `json:"invite_url,omitempty"`
	UpstreamStatus          int        `json:"upstream_status,omitempty"`
	UpstreamRecipientStatus string     `json:"upstream_recipient_status,omitempty"`
	InvitedAt               *time.Time `json:"invited_at,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

// CodexInviteRecipientEvidence is the database-facing subset of an upstream
// send/tracking item. Keeping it in database avoids a dependency on proxy.
type CodexInviteRecipientEvidence struct {
	Email                   string
	ReferralID              string
	InviteURL               string
	UpstreamRecipientStatus string
	InvitedAt               time.Time
}

// CodexInviteRecipientConflictError reports every already-reserved/final
// address in a failed atomic batch. No new row from that batch is committed.
type CodexInviteRecipientConflictError struct {
	Emails []string
}

func (e *CodexInviteRecipientConflictError) Error() string {
	if e == nil || len(e.Emails) == 0 {
		return "Codex invite recipient already recorded"
	}
	return "Codex invite recipient already recorded: " + strings.Join(e.Emails, ", ")
}

var (
	codexInviteRecipientInitMu sync.Mutex
	codexInviteRecipientReady  = make(map[*DB]bool)
)

const codexInviteRecipientSelectColumns = `email_key,email,sender_account_id,program_id,entrypoint,state,reservation_id,request_id,referral_id,invite_url,upstream_status,upstream_recipient_status,invited_at,created_at,updated_at`

type codexInviteRecipientEmail struct {
	key     string
	display string
}

type codexInviteRecipientScanner interface {
	Scan(dest ...any) error
}

type codexInviteRecipientQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// NormalizeCodexInviteRecipientEmail is the single identity rule used by the
// ledger and its callers. It intentionally matches the existing invite form's
// trim + case-insensitive deduplication.
func NormalizeCodexInviteRecipientEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func normalizeCodexInviteRecipientEmails(emails []string) []codexInviteRecipientEmail {
	seen := make(map[string]struct{}, len(emails))
	out := make([]codexInviteRecipientEmail, 0, len(emails))
	for _, email := range emails {
		display := strings.TrimSpace(email)
		key := NormalizeCodexInviteRecipientEmail(display)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, codexInviteRecipientEmail{key: key, display: display})
	}
	return out
}

func normalizeCodexInviteRecipientEvidence(items []CodexInviteRecipientEvidence) []CodexInviteRecipientEvidence {
	seen := make(map[string]struct{}, len(items))
	out := make([]CodexInviteRecipientEvidence, 0, len(items))
	for _, item := range items {
		item.Email = strings.TrimSpace(item.Email)
		key := NormalizeCodexInviteRecipientEmail(item.Email)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		item.ReferralID = strings.TrimSpace(item.ReferralID)
		item.InviteURL = strings.TrimSpace(item.InviteURL)
		item.UpstreamRecipientStatus = strings.TrimSpace(item.UpstreamRecipientStatus)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return NormalizeCodexInviteRecipientEmail(out[i].Email) < NormalizeCodexInviteRecipientEmail(out[j].Email)
	})
	return out
}

func codexInviteRecipientDDL(sqlite bool) []string {
	timeType := "TIMESTAMPTZ"
	if sqlite {
		timeType = "TIMESTAMP"
	}
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS codex_invite_recipients (
			email_key TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			sender_account_id BIGINT NOT NULL DEFAULT 0,
			program_id TEXT NOT NULL DEFAULT '',
			entrypoint TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL CHECK (state IN ('reserved','sent','known_invited','unknown')),
			reservation_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			referral_id TEXT NOT NULL DEFAULT '',
			invite_url TEXT NOT NULL DEFAULT '',
			upstream_status INTEGER NOT NULL DEFAULT 0,
			upstream_recipient_status TEXT NOT NULL DEFAULT '',
			invited_at %s NULL,
			created_at %s NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at %s NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, timeType, timeType, timeType),
		`CREATE INDEX IF NOT EXISTS idx_codex_invite_recipients_state ON codex_invite_recipients(state, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_invite_recipients_reservation ON codex_invite_recipients(reservation_id)`,
	}
}

func (db *DB) ensureCodexInviteRecipientTable(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("数据库不可用")
	}
	codexInviteRecipientInitMu.Lock()
	defer codexInviteRecipientInitMu.Unlock()
	if codexInviteRecipientReady[db] {
		return nil
	}
	for _, statement := range codexInviteRecipientDDL(db.isSQLite()) {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	codexInviteRecipientReady[db] = true
	return nil
}

func scanCodexInviteRecipient(scanner codexInviteRecipientScanner) (CodexInviteRecipient, error) {
	var item CodexInviteRecipient
	var invitedAt, createdAt, updatedAt any
	err := scanner.Scan(
		&item.EmailKey, &item.Email, &item.SenderAccountID, &item.ProgramID, &item.Entrypoint,
		&item.State, &item.ReservationID, &item.RequestID, &item.ReferralID, &item.InviteURL,
		&item.UpstreamStatus, &item.UpstreamRecipientStatus, &invitedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return CodexInviteRecipient{}, err
	}
	if invitedAt != nil {
		parsed, parseErr := parseDBTimeValue(invitedAt)
		if parseErr != nil {
			return CodexInviteRecipient{}, parseErr
		}
		if !parsed.IsZero() {
			item.InvitedAt = &parsed
		}
	}
	if item.CreatedAt, err = parseDBTimeValue(createdAt); err != nil {
		return CodexInviteRecipient{}, err
	}
	if item.UpdatedAt, err = parseDBTimeValue(updatedAt); err != nil {
		return CodexInviteRecipient{}, err
	}
	return item, nil
}

func listCodexInviteRecipientsByReservation(ctx context.Context, queryer codexInviteRecipientQueryer, reservationID string) ([]CodexInviteRecipient, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT `+codexInviteRecipientSelectColumns+`
		FROM codex_invite_recipients WHERE reservation_id=$1 ORDER BY email_key`, reservationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]CodexInviteRecipient, 0)
	for rows.Next() {
		item, scanErr := scanCodexInviteRecipient(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) listCodexInviteRecipientsByKeys(ctx context.Context, queryer codexInviteRecipientQueryer, keys []string) ([]CodexInviteRecipient, error) {
	if len(keys) == 0 {
		return []CodexInviteRecipient{}, nil
	}
	placeholders := dbPlaceholders(db.isSQLite(), 1, len(keys))
	args := make([]any, len(keys))
	for i, key := range keys {
		args[i] = key
	}
	rows, err := queryer.QueryContext(ctx, `SELECT `+codexInviteRecipientSelectColumns+`
		FROM codex_invite_recipients WHERE email_key IN (`+strings.Join(placeholders, ",")+`) ORDER BY email_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]CodexInviteRecipient, 0, len(keys))
	for rows.Next() {
		item, scanErr := scanCodexInviteRecipient(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReserveCodexInviteRecipients atomically claims every unique email in the
// batch. One conflict rolls back all inserts, including earlier loop entries.
// The committed reservation is intentionally durable and has no automatic
// lease expiry: a crash after dispatch is ambiguous, so silently reclaiming it
// would violate the at-most-once promise.
func (db *DB) ReserveCodexInviteRecipients(ctx context.Context, reservationID string, senderAccountID int64, programID, entrypoint string, emails []string) ([]CodexInviteRecipient, error) {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil, fmt.Errorf("reservation_id 不能为空")
	}
	normalized := normalizeCodexInviteRecipientEmails(emails)
	if len(normalized) == 0 {
		return nil, fmt.Errorf("被邀请邮箱不能为空")
	}
	// PostgreSQL 的 ON CONFLICT 可能等待另一事务持有的唯一键；固定锁顺序避免
	// 两个重叠批次以相反邮箱顺序预占时形成循环等待。
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].key < normalized[j].key })
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var reserved []CodexInviteRecipient
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		conflicts := make([]string, 0)
		for _, email := range normalized {
			result, err := tx.ExecContext(ctx, `INSERT INTO codex_invite_recipients
				(email_key,email,sender_account_id,program_id,entrypoint,state,reservation_id,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8)
				ON CONFLICT(email_key) DO NOTHING`,
				email.key, email.display, senderAccountID, strings.TrimSpace(programID), strings.TrimSpace(entrypoint),
				CodexInviteRecipientStateReserved, reservationID, db.timeArg(now))
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				conflicts = append(conflicts, email.display)
			}
		}
		if len(conflicts) > 0 {
			return &CodexInviteRecipientConflictError{Emails: conflicts}
		}
		var err error
		reserved, err = listCodexInviteRecipientsByReservation(ctx, tx, reservationID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reserved, nil
}

// FinalizeCodexInviteRecipients marks every row owned by reservationID as
// sent. Evidence is optional because a successful upstream response may omit
// invites/items while still accepting the whole requested email list.
func (db *DB) FinalizeCodexInviteRecipients(ctx context.Context, reservationID, requestID string, upstreamStatus int, evidence []CodexInviteRecipientEvidence, invitedAt time.Time) ([]CodexInviteRecipient, error) {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil, fmt.Errorf("reservation_id 不能为空")
	}
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return nil, err
	}
	if invitedAt.IsZero() {
		invitedAt = time.Now().UTC()
	}
	items := normalizeCodexInviteRecipientEvidence(evidence)
	var finalized []CodexInviteRecipient
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE codex_invite_recipients SET
			state=$1,request_id=$2,upstream_status=$3,invited_at=$4,updated_at=$4
			WHERE reservation_id=$5 AND state=$6`,
			CodexInviteRecipientStateSent, strings.TrimSpace(requestID), upstreamStatus, db.timeArg(invitedAt),
			reservationID, CodexInviteRecipientStateReserved)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return fmt.Errorf("%w: %s", ErrCodexInviteRecipientReservationLost, reservationID)
		}
		for _, item := range items {
			itemInvitedAt := item.InvitedAt
			if itemInvitedAt.IsZero() {
				itemInvitedAt = invitedAt
			}
			if _, err := tx.ExecContext(ctx, `UPDATE codex_invite_recipients SET
				email=$1,referral_id=$2,invite_url=$3,upstream_recipient_status=$4,invited_at=$5,updated_at=$6
				WHERE reservation_id=$7 AND email_key=$8 AND state=$9`,
				item.Email, item.ReferralID, item.InviteURL, item.UpstreamRecipientStatus,
				db.timeArg(itemInvitedAt), db.timeArg(invitedAt), reservationID,
				NormalizeCodexInviteRecipientEmail(item.Email), CodexInviteRecipientStateSent); err != nil {
				return err
			}
		}
		finalized, err = listCodexInviteRecipientsByReservation(ctx, tx, reservationID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return finalized, nil
}

// MarkCodexInviteRecipientsKnownInvited records recipient-level upstream proof
// (for example "already received an invitation") for rows owned by the
// current reservation. Every evidence email must still be fenced by that
// reservation or the whole update rolls back.
func (db *DB) MarkCodexInviteRecipientsKnownInvited(ctx context.Context, reservationID, requestID string, upstreamStatus int, evidence []CodexInviteRecipientEvidence, observedAt time.Time) ([]CodexInviteRecipient, error) {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil, fmt.Errorf("reservation_id 不能为空")
	}
	items := normalizeCodexInviteRecipientEvidence(evidence)
	if len(items) == 0 {
		return []CodexInviteRecipient{}, nil
	}
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return nil, err
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	keys := make([]string, 0, len(items))
	var marked []CodexInviteRecipient
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, item := range items {
			key := NormalizeCodexInviteRecipientEmail(item.Email)
			keys = append(keys, key)
			itemInvitedAt := item.InvitedAt
			if itemInvitedAt.IsZero() {
				itemInvitedAt = observedAt
			}
			result, err := tx.ExecContext(ctx, `UPDATE codex_invite_recipients SET
				email=$1,state=$2,request_id=$3,referral_id=$4,invite_url=$5,upstream_status=$6,
				upstream_recipient_status=$7,invited_at=$8,updated_at=$9
				WHERE reservation_id=$10 AND email_key=$11 AND state=$12`,
				item.Email, CodexInviteRecipientStateKnownInvited, strings.TrimSpace(requestID), item.ReferralID,
				item.InviteURL, upstreamStatus, item.UpstreamRecipientStatus, db.timeArg(itemInvitedAt),
				db.timeArg(observedAt), reservationID, key, CodexInviteRecipientStateReserved)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return fmt.Errorf("%w: %s (%s)", ErrCodexInviteRecipientReservationLost, reservationID, item.Email)
			}
		}
		var err error
		marked, err = db.listCodexInviteRecipientsByKeys(ctx, tx, keys)
		return err
	})
	if err != nil {
		return nil, err
	}
	return marked, nil
}

// MarkCodexInviteRecipientsUnknown conservatively blocks ambiguous transport
// outcomes. ReleaseCodexInviteRecipients cannot delete these rows.
func (db *DB) MarkCodexInviteRecipientsUnknown(ctx context.Context, reservationID, requestID string, upstreamStatus int) (int64, error) {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return 0, fmt.Errorf("reservation_id 不能为空")
	}
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var affected int64
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE codex_invite_recipients SET
			state=$1,request_id=$2,upstream_status=$3,updated_at=$4
			WHERE reservation_id=$5 AND state=$6`,
			CodexInviteRecipientStateUnknown, strings.TrimSpace(requestID), upstreamStatus, db.timeArg(now),
			reservationID, CodexInviteRecipientStateReserved)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		return err
	})
	return affected, err
}

// ReleaseCodexInviteRecipients releases only a definitively-unsent reserved
// batch. Final and unknown states remain durable and continue blocking retry.
func (db *DB) ReleaseCodexInviteRecipients(ctx context.Context, reservationID string) (int64, error) {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return 0, fmt.Errorf("reservation_id 不能为空")
	}
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return 0, err
	}
	var affected int64
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM codex_invite_recipients
			WHERE reservation_id=$1 AND state=$2`, reservationID, CodexInviteRecipientStateReserved)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		return err
	})
	return affected, err
}

// UpsertCodexInviteRecipientsFromTracking turns upstream tracking items into
// durable proof. It upgrades reserved/unknown rows and never downgrades a sent
// row. An empty/missing tracking result is intentionally not evidence that an
// address may be retried.
func (db *DB) UpsertCodexInviteRecipientsFromTracking(ctx context.Context, senderAccountID int64, programID, entrypoint string, upstreamStatus int, evidence []CodexInviteRecipientEvidence) ([]CodexInviteRecipient, error) {
	items := normalizeCodexInviteRecipientEvidence(evidence)
	if len(items) == 0 {
		return []CodexInviteRecipient{}, nil
	}
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	keys := make([]string, 0, len(items))
	var recorded []CodexInviteRecipient
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, item := range items {
			key := NormalizeCodexInviteRecipientEmail(item.Email)
			keys = append(keys, key)
			invitedAt := item.InvitedAt
			if invitedAt.IsZero() {
				invitedAt = now
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO codex_invite_recipients
				(email_key,email,sender_account_id,program_id,entrypoint,state,reservation_id,request_id,
				 referral_id,invite_url,upstream_status,upstream_recipient_status,invited_at,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,$6,'','',$7,$8,$9,$10,$11,$12,$12)
				ON CONFLICT(email_key) DO UPDATE SET
				email=excluded.email,
				sender_account_id=CASE WHEN codex_invite_recipients.sender_account_id=0 THEN excluded.sender_account_id ELSE codex_invite_recipients.sender_account_id END,
				program_id=CASE WHEN codex_invite_recipients.program_id='' THEN excluded.program_id ELSE codex_invite_recipients.program_id END,
				entrypoint=CASE WHEN codex_invite_recipients.entrypoint='' THEN excluded.entrypoint ELSE codex_invite_recipients.entrypoint END,
				state=CASE WHEN codex_invite_recipients.state='sent' THEN 'sent' ELSE 'known_invited' END,
				referral_id=CASE WHEN excluded.referral_id<>'' THEN excluded.referral_id ELSE codex_invite_recipients.referral_id END,
				invite_url=CASE WHEN excluded.invite_url<>'' THEN excluded.invite_url ELSE codex_invite_recipients.invite_url END,
				upstream_status=CASE WHEN excluded.upstream_status<>0 THEN excluded.upstream_status ELSE codex_invite_recipients.upstream_status END,
				upstream_recipient_status=CASE WHEN excluded.upstream_recipient_status<>'' THEN excluded.upstream_recipient_status ELSE codex_invite_recipients.upstream_recipient_status END,
				invited_at=COALESCE(codex_invite_recipients.invited_at,excluded.invited_at),
				updated_at=excluded.updated_at`,
				key, item.Email, senderAccountID, strings.TrimSpace(programID), strings.TrimSpace(entrypoint),
				CodexInviteRecipientStateKnownInvited, item.ReferralID, item.InviteURL, upstreamStatus,
				item.UpstreamRecipientStatus, db.timeArg(invitedAt), db.timeArg(now))
			if err != nil {
				return err
			}
		}
		var err error
		recorded, err = db.listCodexInviteRecipientsByKeys(ctx, tx, keys)
		return err
	})
	if err != nil {
		return nil, err
	}
	return recorded, nil
}

// ListCodexInviteRecipientsByEmails returns every durable blocking record for
// the requested addresses. Results are unique and sorted by normalized email.
func (db *DB) ListCodexInviteRecipientsByEmails(ctx context.Context, emails []string) ([]CodexInviteRecipient, error) {
	normalized := normalizeCodexInviteRecipientEmails(emails)
	if len(normalized) == 0 {
		return []CodexInviteRecipient{}, nil
	}
	if err := db.ensureCodexInviteRecipientTable(ctx); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(normalized))
	for _, item := range normalized {
		keys = append(keys, item.key)
	}
	sort.Strings(keys)
	return db.listCodexInviteRecipientsByKeys(ctx, db.conn, keys)
}
