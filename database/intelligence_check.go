package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	ic "github.com/codex2api/internal/intelligencecheck"
	"strings"
	"time"
)

var ErrIntelligenceCheckRevisionConflict = errors.New("intelligence check settings revision conflict")
var ErrIntelligenceCheckTargetGroup = errors.New("intelligence check target group is incompatible")
var ErrIntelligenceCheckUnsupportedAccount = errors.New("account is not eligible for intelligence checks")

type IntelligenceCheckTargetConflictAccount struct {
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	Channel     string `json:"channel"`
}

// Bound the response to the same batch size accepted by the account toggle API.
// Existing opt-ins are preserved until the administrator explicitly changes them.
type IntelligenceCheckTargetConflictError struct {
	TargetGroupID      int64                                    `json:"target_group_id"`
	TargetGroupChannel string                                   `json:"target_group_channel"`
	ConflictCount      int                                      `json:"conflict_count"`
	Conflicts          []IntelligenceCheckTargetConflictAccount `json:"conflicts"`
}

func (e *IntelligenceCheckTargetConflictError) Error() string {
	return fmt.Sprintf("%s: %d enabled accounts belong to another channel", ErrIntelligenceCheckTargetGroup, e.ConflictCount)
}

func (e *IntelligenceCheckTargetConflictError) Unwrap() error { return ErrIntelligenceCheckTargetGroup }

type IntelligenceCheckAccountFilter struct {
	AccountID               int64
	Search, Channel, Status string
	Offset, Limit           int
}
type IntelligenceCheckRunFilter struct {
	AccountID               int64
	Search, Channel, Status string
	Offset, Limit           int
}
type IntelligenceCheckAccount struct {
	CredentialGeneration int64      `json:"-"`
	ConfigError          string     `json:"config_error,omitempty"`
	AccountID            int64      `json:"account_id"`
	AccountName          string     `json:"account_name"`
	Channel              string     `json:"channel"`
	Enabled              bool       `json:"enabled"`
	ManualEnabled        bool       `json:"manual_enabled"`
	AccountStatus        string     `json:"account_status"`
	Status               string     `json:"latest_status"`
	QualityPaused        bool       `json:"quality_paused"`
	ManualRevision       int64      `json:"manual_revision"`
	NextDueAt            *time.Time `json:"next_due_at"`
	QuestionCursor       int        `json:"question_cursor"`
	ic.State
}
type IntelligenceCheckRun struct {
	CredentialGeneration int64       `json:"credential_generation,omitempty"`
	ID                   int64       `json:"id"`
	AccountID            int64       `json:"account_id"`
	AccountName          string      `json:"account_name"`
	Channel              string      `json:"channel"`
	Source               string      `json:"source"`
	Status               string      `json:"status"`
	ConfigRevision       int64       `json:"config_revision"`
	ManualRevision       int64       `json:"manual_revision"`
	Model                string      `json:"model"`
	ReasoningEffort      string      `json:"reasoning_effort"`
	JudgeMode            string      `json:"judge_mode"`
	Question             ic.Question `json:"question"`
	Settings             ic.Settings `json:"settings"`
	ic.Observation
	AnswerPassed  bool       `json:"answer_passed"`
	LatencyPassed bool       `json:"latency_passed"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at"`
	Action        string     `json:"action"`
	ActionMessage string     `json:"action_message,omitempty"`
	LeaseOwner    string     `json:"-"`
	LeaseUntil    int64      `json:"-"`
}
type IntelligenceCheckBar struct {
	ProbePhase          string                        `json:"probe_phase,omitempty"`
	NextIntervalSeconds int                           `json:"next_interval_seconds,omitempty"`
	PassedStreak        int                           `json:"passed_streak"`
	NextDueAt           *time.Time                    `json:"next_due_at,omitempty"`
	ActiveRunID         int64                         `json:"active_run_id,omitempty"`
	ActiveStatus        string                        `json:"active_status,omitempty"`
	Enabled             bool                          `json:"enabled"`
	QualityPaused       bool                          `json:"quality_paused"`
	OwnedAction         string                        `json:"action"`
	Records             []IntelligenceCheckRunSummary `json:"records"`
}
type IntelligenceCheckRunSummary struct {
	ID           int64      `json:"id"`
	Status       string     `json:"status"`
	QuestionName string     `json:"question_name"`
	DurationMS   int64      `json:"duration_ms"`
	CompletedAt  *time.Time `json:"completed_at"`
}

// ensureIntelligenceCheckSchema installs detector storage and source-aware enrollment triggers.
func (db *DB) ensureIntelligenceCheckSchema(ctx context.Context) error {
	idType := "BIGSERIAL PRIMARY KEY"
	if db.isSQLite() {
		idType = "INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS intelligence_check_settings (id INTEGER PRIMARY KEY, revision BIGINT NOT NULL, config_json TEXT NOT NULL, new_accounts_enabled BOOLEAN NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS intelligence_check_accounts (account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE, enabled BOOLEAN NOT NULL DEFAULT FALSE, quality_paused BOOLEAN NOT NULL DEFAULT FALSE, manual_revision BIGINT NOT NULL DEFAULT 0, state_json TEXT NOT NULL DEFAULT '{}', next_due_ms BIGINT NOT NULL DEFAULT 0, question_cursor INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'unknown')`,
		`CREATE TABLE IF NOT EXISTS intelligence_check_runs (id ` + idType + `, account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, status TEXT NOT NULL, run_json TEXT NOT NULL, created_ms BIGINT NOT NULL, ready_ms BIGINT NOT NULL, completed_ms BIGINT NOT NULL DEFAULT 0, lease_owner TEXT NOT NULL DEFAULT '', lease_until_ms BIGINT NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_intelligence_due ON intelligence_check_accounts(enabled,next_due_ms,account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_intelligence_queue ON intelligence_check_runs(status,ready_ms,id)`,
		`CREATE INDEX IF NOT EXISTS idx_intelligence_history ON intelligence_check_runs(account_id,id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_intelligence_one_active ON intelligence_check_runs(account_id) WHERE status IN ('queued','running')`,
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		if err := db.ensureIntelligenceCheckSummariesTx(ctx, tx); err != nil {
			return err
		}
		s := ic.DefaultSettings()
		raw, _ := json.Marshal(s)
		if _, err := tx.ExecContext(ctx, `INSERT INTO intelligence_check_settings(id,revision,config_json,new_accounts_enabled) VALUES(1,$1,$2,$3) ON CONFLICT(id) DO NOTHING`, s.Revision, string(raw), s.NewAccountsEnabled); err != nil {
			return err
		}
		// Backfill must precede installing the creation trigger: preexisting accounts
		// remain opted out, while subsequent INSERTs inherit the saved default.
		if _, err := tx.ExecContext(ctx, `INSERT INTO intelligence_check_accounts(account_id,enabled) SELECT id,FALSE FROM accounts WHERE TRUE ON CONFLICT(account_id) DO NOTHING`); err != nil {
			return err
		}
		// Reuse the eligibility/channel predicates used by manual enrollment and
		// settings validation. AFTER INSERT can read the newly inserted account.
		action := `json_extract(s.config_json,'$.action')`
		target := `json_extract(s.config_json,'$.target_group_id')`
		if !db.isSQLite() {
			action = `(s.config_json::jsonb)->>'action'`
			target = `((s.config_json::jsonb)->>'target_group_id')::bigint`
		}
		channelEnabled := db.intelligenceSourceEnabledSQL("a", "s")
		defaultEnabled := `COALESCE((SELECT s.new_accounts_enabled AND (` + db.intelligenceEligibleSQL() + `) AND (` + channelEnabled + `) AND
			(COALESCE(` + action + `,'none')<>'switch_group' OR EXISTS (
				SELECT 1 FROM account_groups g WHERE g.id=` + target + ` AND LOWER(TRIM(COALESCE(g.channel,'codex')))=` + db.intelligenceChannelExpr() + `))
			FROM intelligence_check_settings s JOIN accounts a ON a.id=NEW.id WHERE s.id=1),FALSE)`
		insert := `INSERT INTO intelligence_check_accounts(account_id,enabled) VALUES(NEW.id,` + defaultEnabled + `) ON CONFLICT(account_id) DO NOTHING;`
		if db.isSQLite() {
			// IF NOT EXISTS would leave the old, unconditional trigger on upgrades.
			if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS intelligence_check_new_account`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `CREATE TRIGGER intelligence_check_new_account AFTER INSERT ON accounts BEGIN `+insert+` END`)
			return err
		}
		// Serialize enrollment with settings changes, including a concurrent switch
		// from none/pause to switch_group. SQLite already serializes writers.
		if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION intelligence_check_new_account_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM 1 FROM intelligence_check_settings WHERE id=1 FOR SHARE; `+insert+` RETURN NEW; END $$`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS intelligence_check_new_account ON accounts`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER intelligence_check_new_account AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION intelligence_check_new_account_fn()`)
		return err
	})
}
func (db *DB) intelligenceSettingsTx(ctx context.Context, tx *sql.Tx) (ic.Settings, error) {
	q := `SELECT config_json FROM intelligence_check_settings WHERE id=1`
	if !db.isSQLite() {
		q += ` FOR UPDATE`
	}
	var raw string
	var s ic.Settings
	if err := tx.QueryRowContext(ctx, q).Scan(&raw); err != nil {
		return s, err
	}
	err := json.Unmarshal([]byte(raw), &s)
	return ic.AdaptiveDefaults(s), err
}
func (db *DB) GetIntelligenceCheckSettings(ctx context.Context) (ic.Settings, error) {
	var raw string
	var s ic.Settings
	err := db.conn.QueryRowContext(ctx, `SELECT config_json FROM intelligence_check_settings WHERE id=1`).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &s)
	}
	return ic.AdaptiveDefaults(s), err
}

// SaveIntelligenceCheckSettings atomically updates policy while preserving source-specific credential fences.
func (db *DB) SaveIntelligenceCheckSettings(ctx context.Context, s ic.Settings) (ic.Settings, error) {
	s = ic.AdaptiveDefaults(s)
	if err := ic.ValidateSettings(s); err != nil {
		return s, err
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		old, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if old.Revision != s.Revision {
			return ErrIntelligenceCheckRevisionConflict
		}
		// Old admin clients must not reset the source switches when saving.
		if s.Channels == nil {
			s.Channels = old.Channels
		}
		if !s.GrokModelSupplied && s.GrokModel == "" {
			s.GrokModel = old.GrokModel
		}
		if !s.AntigravityModelSupplied && s.AntigravityModel == "" {
			s.AntigravityModel = old.AntigravityModel
		}
		if s.Action == "switch_group" {
			channel, err := intelligenceTargetGroupChannel(ctx, tx, s)
			if err != nil {
				return err
			}
			conflict := &IntelligenceCheckTargetConflictError{TargetGroupID: s.TargetGroupID, TargetGroupChannel: channel}
			rows, err := tx.QueryContext(ctx, `SELECT a.id,COALESCE(a.name,''),`+db.intelligenceChannelExpr()+`,COUNT(*) OVER () FROM accounts a JOIN intelligence_check_accounts st ON st.account_id=a.id WHERE `+db.intelligenceEligibleSQL()+` AND `+db.intelligenceEnabledChannelsSQL(s)+` AND st.enabled=TRUE AND COALESCE(`+db.intelligenceStateActionExpr("st")+`,'')='' AND `+db.intelligenceChannelExpr()+`<>$1 ORDER BY a.id LIMIT 200`, channel)
			if err != nil {
				return err
			}
			for rows.Next() {
				var account IntelligenceCheckTargetConflictAccount
				if err = rows.Scan(&account.AccountID, &account.AccountName, &account.Channel, &conflict.ConflictCount); err != nil {
					rows.Close()
					return err
				}
				conflict.Conflicts = append(conflict.Conflicts, account)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if conflict.ConflictCount > 0 {
				return conflict
			}
		}

		s.Revision++
		raw, _ := json.Marshal(s)
		if _, err := tx.ExecContext(ctx, `UPDATE intelligence_check_settings SET revision=$1,config_json=$2,new_accounts_enabled=$3 WHERE id=1`, s.Revision, string(raw), s.NewAccountsEnabled); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT st.account_id,st.state_json,`+db.intelligenceChannelExpr()+` FROM intelligence_check_accounts st JOIN accounts a ON a.id=st.account_id`)
		if err != nil {
			return err
		}
		states := map[int64]ic.State{}
		channels := map[int64]string{}
		for rows.Next() {
			var id int64
			var raw, channel string
			if err = rows.Scan(&id, &raw, &channel); err != nil {
				rows.Close()
				return err
			}
			var state ic.State
			if err = json.Unmarshal([]byte(raw), &state); err != nil {
				rows.Close()
				return err
			}
			state.DegradeStreak = 0
			state.RecoverStreak = 0
			states[id] = state
			channels[id] = channel
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for id, state := range states {
			delay := s.IntervalSeconds
			state.ErrorRetrySeconds = 0
			state.ProbePhase = ""
			if s.AdaptiveEnabled {
				state.ProbePhase = "normal"
				if state.PauseBackoffSeconds > 0 {
					state.PauseBackoffSeconds = min(s.PauseMaxSeconds, max(s.PauseInitialSeconds, state.PauseBackoffSeconds))
				}
				if state.OwnedAction == "pause" {
					if state.PauseBackoffSeconds == 0 {
						state.PauseBackoffSeconds = s.PauseInitialSeconds
					}
					delay, state.ProbePhase = state.PauseBackoffSeconds, "backoff"
				}
			} else {
				if channels[id] != "antigravity" {
					state.BlockedCredentialGeneration = nil
				}
				state.PauseBackoffSeconds = 0
			}
			state.NextIntervalSeconds = delay
			raw, _ := json.Marshal(state)
			// Keep an earlier due check unless switching timing policies.
			due := time.Now().Add(time.Duration(delay) * time.Second).UnixMilli()
			if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET state_json=$1,next_due_ms=CASE WHEN next_due_ms>$3 OR $4 THEN $3 ELSE next_due_ms END WHERE account_id=$2`, string(raw), id, due, old.AdaptiveEnabled != s.AdaptiveEnabled); err != nil {
				return err
			}
		}
		return nil
	})
	return ic.AdaptiveDefaults(s), err
}
func (db *DB) intelligenceUpstreamExpr() string {
	if db.isSQLite() {
		return `LOWER(COALESCE(json_extract(a.credentials,'$.upstream_type'),''))`
	}
	return `LOWER(COALESCE(a.credentials->>'upstream_type',''))`
}

// intelligenceChannelExpr gives explicit upstream metadata precedence over legacy platform labels.
func (db *DB) intelligenceChannelExpr() string {
	upstream := `TRIM(` + db.intelligenceUpstreamExpr() + `)`
	return `CASE WHEN ` + upstream + `='antigravity' THEN 'antigravity' WHEN ` + upstream + `='grok' THEN 'grok' WHEN ` + upstream + `='claude' THEN 'claude' WHEN ` + upstream + `<>'' THEN 'codex' WHEN LOWER(COALESCE(a.platform,'')) IN ('antigravity','google') THEN 'antigravity' WHEN LOWER(COALESCE(a.platform,'')) IN ('grok','xai') THEN 'grok' WHEN LOWER(COALESCE(a.platform,'')) IN ('anthropic','claude') THEN 'claude' ELSE 'codex' END`
}
func (db *DB) intelligenceEligibleSQL() string {
	return `a.status<>'deleted' AND COALESCE(a.error_message,'')<>'deleted'`
}

// Use the proposed (not yet persisted) settings for target-group validation.
func (db *DB) intelligenceEnabledChannelsSQL(settings ic.Settings) string {
	channels := []string{}
	for _, channel := range []string{"codex", "claude", "grok", "antigravity"} {
		if settings.ChannelEnabled(channel) {
			channels = append(channels, "'"+channel+"'")
		}
	}
	if len(channels) == 0 {
		return "FALSE"
	}
	return db.intelligenceChannelExpr() + " IN (" + strings.Join(channels, ",") + ")"
}

// intelligenceSourceEnabledSQL uses only fixed source names and trusted SQL
// aliases. Defaults preserve the original Codex/Claude scheduling behavior.
func (db *DB) intelligenceSourceEnabledSQL(account, config string) string {
	channel := strings.ReplaceAll(db.intelligenceChannelExpr(), "a.", account+".")
	query := "CASE " + channel
	for _, name := range []string{"codex", "claude", "grok", "antigravity"} {
		value := "json_extract(" + config + ".config_json,'$.channels." + name + "')"
		if !db.isSQLite() {
			value = "((" + config + ".config_json::jsonb)->'channels'->>'" + name + "')::boolean"
		}
		query += fmt.Sprintf(" WHEN '%s' THEN COALESCE(%s,%t)", name, value, name == "codex" || name == "claude")
	}
	return query + " ELSE FALSE END"
}

// intelligenceAutomaticSQL combines account opt-in, source selection and the
// credential-generation fence. Manual checks bypass automatic admission.
func (db *DB) intelligenceAutomaticSQL(account, state string) string {
	blocked := "(" + state + ".state_json::jsonb->>'blocked_credential_generation')::bigint"
	if db.isSQLite() {
		blocked = "json_extract(" + state + ".state_json,'$.blocked_credential_generation')"
	}
	return state + `.enabled=TRUE AND COALESCE(` + account + `.enabled,TRUE)=TRUE AND (` + blocked + ` IS NULL OR ` + blocked + `<>COALESCE(` + account + `.credential_generation,1)) AND (SELECT ` + db.intelligenceSourceEnabledSQL(account, "source_config") + ` FROM intelligence_check_settings source_config WHERE id=1)`
}
func intelligencePage(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	return offset, limit
}

const intelligenceAccountColumns = `a.id,a.name,COALESCE(a.credential_generation,1), s.enabled,COALESCE(a.enabled,TRUE),a.status,s.status,s.quality_paused,s.manual_revision,s.next_due_ms,s.question_cursor,s.state_json,`

func scanIntelligenceAccount(row interface{ Scan(...interface{}) error }) (IntelligenceCheckAccount, error) {
	var a IntelligenceCheckAccount
	var due int64
	var raw string
	err := row.Scan(&a.AccountID, &a.AccountName, &a.CredentialGeneration, &a.Enabled, &a.ManualEnabled, &a.AccountStatus, &a.Status, &a.QualityPaused, &a.ManualRevision, &due, &a.QuestionCursor, &raw, &a.Channel)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &a.State)
	}
	if due > 0 {
		at := time.UnixMilli(due).UTC()
		a.NextDueAt = &at
	}
	return a, err
}

// ListIntelligenceCheckAccounts returns filtered detector enrollment and source metadata.
func (db *DB) ListIntelligenceCheckAccounts(ctx context.Context, f IntelligenceCheckAccountFilter) ([]IntelligenceCheckAccount, int, error) {
	settings, err := db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		return nil, 0, err
	}
	targetChannel, targetError := "", ""
	if settings.Action == "switch_group" {
		var e error
		targetChannel, e = intelligenceTargetGroupChannel(ctx, db.conn, settings)
		if e != nil {
			if !errors.Is(e, ErrIntelligenceCheckTargetGroup) {
				return nil, 0, e
			}
			targetError = e.Error()
		}
	}

	where := db.intelligenceEligibleSQL()
	args := []interface{}{}
	add := func(expr string, val interface{}) {
		args = append(args, val)
		where += fmt.Sprintf(" AND "+expr, "$"+fmt.Sprint(len(args)))
	}
	if f.AccountID > 0 {
		add(`a.id=%s`, f.AccountID)
	}
	if f.Search != "" {
		add(`LOWER(a.name) LIKE %s`, "%"+strings.ToLower(f.Search)+"%")
	}
	if f.Channel != "" {
		add(db.intelligenceChannelExpr()+`=%s`, f.Channel)
	}
	if f.Status != "" {
		add(`s.status=%s`, f.Status)
	}
	from := ` FROM accounts a JOIN intelligence_check_accounts s ON s.account_id=a.id WHERE ` + where
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	offset, limit := intelligencePage(f.Offset, f.Limit)
	args = append(args, limit, offset)
	rows, err := db.conn.QueryContext(ctx, `SELECT `+intelligenceAccountColumns+db.intelligenceChannelExpr()+from+fmt.Sprintf(` ORDER BY a.id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []IntelligenceCheckAccount{}
	for rows.Next() {
		a, e := scanIntelligenceAccount(rows)
		if e != nil {
			return nil, 0, e
		}
		if a.Enabled && settings.ChannelEnabled(a.Channel) && a.OwnedAction == "" && settings.Action == "switch_group" {
			if targetError != "" {
				a.ConfigError = targetError
			} else if a.Channel != targetChannel {
				a.ConfigError = fmt.Sprintf("target group belongs to %s, account belongs to %s", targetChannel, a.Channel)
			}
		}

		out = append(out, a)
	}
	return out, total, rows.Err()
}
func (db *DB) intelligenceAccountTx(ctx context.Context, tx *sql.Tx, id int64) (IntelligenceCheckAccount, error) {
	q := `SELECT ` + intelligenceAccountColumns + db.intelligenceChannelExpr() + ` FROM accounts a JOIN intelligence_check_accounts s ON s.account_id=a.id WHERE a.id=$1 AND ` + db.intelligenceEligibleSQL()
	if !db.isSQLite() {
		q += ` FOR UPDATE OF s`
	}
	a, err := scanIntelligenceAccount(tx.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrIntelligenceCheckUnsupportedAccount
	}
	return a, err
}
func (db *DB) SetIntelligenceCheckEnabled(ctx context.Context, ids []int64, enabled bool) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		s, err := db.intelligenceSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range normalizeIDSlice(ids) {
			a, err := db.intelligenceAccountTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if enabled {
				if err := validateIntelligenceAccountSettings(ctx, tx, s, a.Channel, a.OwnedAction); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET enabled=$1,next_due_ms=0 WHERE account_id=$2`, enabled, id); err != nil {
				return err
			}
		}
		return nil
	})
}
func (db *DB) IsIntelligenceCheckPaused(ctx context.Context, id int64) (bool, error) {
	var paused bool
	err := db.conn.QueryRowContext(ctx, `SELECT quality_paused FROM intelligence_check_accounts WHERE account_id=$1`, id).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return paused, err
}
func (db *DB) ListIntelligenceCheckPausedAccountIDs(ctx context.Context) ([]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id FROM intelligence_check_accounts WHERE quality_paused=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// intelligenceManualMutationTx records manual intent before enabled/group writes.
// Scheduling writes revoke every action. Group-only writes revoke restoration
// ownership, while preserving a quality pause unrelated to group membership.
// The singleton lock orders these mutations against configuration and results.
func (db *DB) intelligenceManualMutationTx(ctx context.Context, tx *sql.Tx, ids []int64, schedulingWrite bool) error {
	if _, err := db.intelligenceSettingsTx(ctx, tx); err != nil {
		return err
	}
	for _, id := range normalizeIDSlice(ids) {
		var raw string
		var paused bool
		err := tx.QueryRowContext(ctx, `SELECT state_json,quality_paused FROM intelligence_check_accounts WHERE account_id=$1`, id).Scan(&raw, &paused)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var state ic.State
		if err = json.Unmarshal([]byte(raw), &state); err != nil {
			return err
		}
		state.DegradeStreak = 0
		state.RecoverStreak = 0
		state.OriginalGroupIDs = nil
		state.OriginalGroupChannels = nil
		if schedulingWrite {
			state.PauseBackoffSeconds, state.ErrorRetrySeconds, state.NextIntervalSeconds = 0, 0, 0
			state.ProbePhase = ""
			state.OwnedAction = ""
			paused = false
		} else if state.OwnedAction == "switch_group" {
			state.OwnedAction = ""
		}
		encoded, _ := json.Marshal(state)
		if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_accounts SET manual_revision=manual_revision+1,state_json=$1,quality_paused=$2 WHERE account_id=$3`, string(encoded), paused, id); err != nil {
			return err
		}
		if err = insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, id, "upsert"); err != nil {
			return err
		}
	}
	return nil
}

type intelligenceQueryRower interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

// intelligenceTargetGroupChannel validates the configured action target against supported sources.
func intelligenceTargetGroupChannel(ctx context.Context, q intelligenceQueryRower, s ic.Settings) (string, error) {
	var channel string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(channel,'codex') FROM account_groups WHERE id=$1`, s.TargetGroupID).Scan(&channel)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: target group does not exist", ErrIntelligenceCheckTargetGroup)
	}
	if err != nil {
		return "", err
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != "codex" && channel != "claude" && channel != "grok" && channel != "antigravity" {
		return "", fmt.Errorf("%w: unsupported target channel", ErrIntelligenceCheckTargetGroup)
	}
	return channel, nil
}
func validateIntelligenceAccountSettings(ctx context.Context, q intelligenceQueryRower, s ic.Settings, channel string, ownedAction ...string) error {
	if len(ownedAction) > 0 && ownedAction[0] != "" {
		return nil
	}
	if s.Action != "switch_group" {
		return nil
	}
	target, err := intelligenceTargetGroupChannel(ctx, q, s)
	if err != nil {
		return err
	}
	if target != channel {
		return fmt.Errorf("%w: target belongs to %s, account belongs to %s", ErrIntelligenceCheckTargetGroup, target, channel)
	}
	return nil
}

// ValidateIntelligenceCheckAccountSettings checks action prerequisites without
// mutating state or making any upstream request. Call again just before sending
// a frozen run, since its target may have changed after it entered the queue.
func (db *DB) ValidateIntelligenceCheckAccountSettings(ctx context.Context, s ic.Settings, channel string, accountID ...int64) error {
	owned := ""
	if len(accountID) > 0 && accountID[0] > 0 {
		var raw string
		err := db.conn.QueryRowContext(ctx, `SELECT state_json FROM intelligence_check_accounts WHERE account_id=$1`, accountID[0]).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var state ic.State
			if err = json.Unmarshal([]byte(raw), &state); err != nil {
				return err
			}
			owned = state.OwnedAction
		}
	}
	return validateIntelligenceAccountSettings(ctx, db.conn, s, channel, owned)
}
func (db *DB) intelligenceStateActionExpr(alias string) string {
	if db.isSQLite() {
		return "json_extract(" + alias + ".state_json,'$.action')"
	}
	return "(" + alias + ".state_json::jsonb->>'action')"
}

// Summary columns decouple bounded account bars from large response bodies.
// Existing history is backfilled once without rewriting the original run JSON.
func (db *DB) ensureIntelligenceCheckSummariesTx(ctx context.Context, tx *sql.Tx) error {
	query := `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='intelligence_check_runs'`
	if db.isSQLite() {
		query = `SELECT name FROM pragma_table_info('intelligence_check_runs')`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, col := range []struct{ name, def string }{{"question_name", "TEXT NOT NULL DEFAULT ''"}, {"duration_ms", "BIGINT NOT NULL DEFAULT 0"}, {"summary_version", "INTEGER NOT NULL DEFAULT 0"}} {
		if !columns[col.name] {
			if _, err = tx.ExecContext(ctx, `ALTER TABLE intelligence_check_runs ADD COLUMN `+col.name+` `+col.def); err != nil {
				return err
			}
		}
	}
	nameExpr := `COALESCE(run_json::jsonb #>> '{question,name}','')`
	durationExpr := `COALESCE((run_json::jsonb->>'duration_ms')::bigint,0)`
	if db.isSQLite() {
		nameExpr = `COALESCE(json_extract(run_json,'$.question.name'),'')`
		durationExpr = `COALESCE(json_extract(run_json,'$.duration_ms'),0)`
	}
	if _, err = tx.ExecContext(ctx, `UPDATE intelligence_check_runs SET question_name=`+nameExpr+`,duration_ms=`+durationExpr+`,summary_version=1 WHERE summary_version=0 AND status NOT IN ('queued','running')`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_intelligence_bars ON intelligence_check_runs(account_id,id DESC,status,question_name,duration_ms,completed_ms) WHERE status NOT IN ('queued','running')`)
	return err
}
