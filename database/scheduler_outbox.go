package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	SchedulerEntityAccount  = "account"
	SchedulerEntityAPIKey   = "api_key"
	SchedulerEntityGroup    = "group"
	SchedulerEntityProxy    = "proxy"
	SchedulerEntitySettings = "settings"
)

type SchedulerOutboxEvent struct {
	ID         int64
	EntityType string
	EntityID   int64
	EventType  string
	CreatedAt  time.Time
}

func normalizeSchedulerOutboxValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func insertSchedulerOutboxEventTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, eventType string) error {
	entityType = normalizeSchedulerOutboxValue(entityType)
	eventType = normalizeSchedulerOutboxValue(eventType)
	if entityType == "" || eventType == "" {
		return fmt.Errorf("scheduler outbox entity_type and event_type are required")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO scheduler_outbox(entity_type,entity_id,event_type,created_at) VALUES($1,$2,$3,CURRENT_TIMESTAMP)`, entityType, entityID, eventType)
	return err
}

func (db *DB) InsertSchedulerOutboxEvent(ctx context.Context, entityType string, entityID int64, eventType string) error {
	if db == nil || db.conn == nil {
		return nil
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		return insertSchedulerOutboxEventTx(ctx, tx, entityType, entityID, eventType)
	})
}

func (db *DB) SchedulerOutboxHighWatermark(ctx context.Context) (int64, error) {
	if db == nil || db.conn == nil {
		return 0, nil
	}
	var watermark int64
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM scheduler_outbox`).Scan(&watermark)
	return watermark, err
}

func (db *DB) ListSchedulerOutboxEventsAfter(ctx context.Context, afterID int64, limit int) ([]SchedulerOutboxEvent, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id,entity_type,entity_id,event_type,created_at FROM scheduler_outbox WHERE id>$1 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]SchedulerOutboxEvent, 0, limit)
	for rows.Next() {
		var event SchedulerOutboxEvent
		var created any
		if err := rows.Scan(&event.ID, &event.EntityType, &event.EntityID, &event.EventType, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, _ = parseDBTimeValue(created)
		events = append(events, event)
	}
	return events, rows.Err()
}

// ListSchedulerOutboxEventsByIDs fetches specific event rows. The consumer
// uses it to re-poll watermark holes: a BIGSERIAL id is assigned at INSERT but
// only becomes visible at COMMIT, so an id below the watermark can appear
// after the watermark has already advanced past it.
func (db *DB) ListSchedulerOutboxEventsByIDs(ctx context.Context, ids []int64) ([]SchedulerOutboxEvent, error) {
	if db == nil || db.conn == nil || len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		if db.isSQLite() {
			placeholders[i] = "?"
		} else {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		args[i] = id
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id,entity_type,entity_id,event_type,created_at FROM scheduler_outbox WHERE id IN (`+strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]SchedulerOutboxEvent, 0, len(ids))
	for rows.Next() {
		var event SchedulerOutboxEvent
		var created any
		if err := rows.Scan(&event.ID, &event.EntityType, &event.EntityID, &event.EventType, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, _ = parseDBTimeValue(created)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (db *DB) CleanupSchedulerOutbox(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	return db.CleanupSchedulerOutboxThrough(ctx, olderThan, 0, limit)
}

// CleanupSchedulerOutboxThrough only removes events already consumed by the
// caller. A zero throughID keeps the legacy unrestricted cleanup behavior used
// by database maintenance tests and one-shot tools.
func (db *DB) CleanupSchedulerOutboxThrough(ctx context.Context, olderThan time.Time, throughID int64, limit int) (int64, error) {
	if db == nil || db.conn == nil || olderThan.IsZero() {
		return 0, nil
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	query := `DELETE FROM scheduler_outbox WHERE id IN (SELECT id FROM scheduler_outbox WHERE created_at<$1 ORDER BY id LIMIT $2)`
	args := []interface{}{db.timeArg(olderThan), limit}
	if throughID > 0 {
		query = `DELETE FROM scheduler_outbox WHERE id IN (SELECT id FROM scheduler_outbox WHERE created_at<$1 AND id<=$2 ORDER BY id LIMIT $3)`
		args = []interface{}{db.timeArg(olderThan), throughID, limit}
	}
	result, err := db.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// installSchedulerOutboxTriggers keeps the routing projection event log in the
// same transaction as the source mutation.  The request path never writes or
// polls the database because the auth.Store consumes this log in the
// background.  API-key usage counters are intentionally excluded: only fields
// that can change routing eligibility emit an event.
func (db *DB) installSchedulerOutboxTriggers(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return nil
	}
	if db.isSQLite() {
		return db.installSQLiteSchedulerOutboxTriggers(ctx)
	}
	return db.installPostgresSchedulerOutboxTriggers(ctx)
}

func (db *DB) installSQLiteSchedulerOutboxTriggers(ctx context.Context) error {
	const query = `
		DROP TRIGGER IF EXISTS scheduler_outbox_accounts_insert;
		DROP TRIGGER IF EXISTS scheduler_outbox_accounts_update;
		DROP TRIGGER IF EXISTS scheduler_outbox_accounts_delete;
		DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_insert;
		DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_update;
		DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_delete;
		DROP TRIGGER IF EXISTS scheduler_outbox_groups_insert;
		DROP TRIGGER IF EXISTS scheduler_outbox_groups_update;
		DROP TRIGGER IF EXISTS scheduler_outbox_groups_delete;
		DROP TRIGGER IF EXISTS scheduler_outbox_members_insert;
		DROP TRIGGER IF EXISTS scheduler_outbox_members_delete;
		DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_insert;
		DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_update;
		DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_delete;
		DROP TRIGGER IF EXISTS scheduler_outbox_proxies_insert;
		DROP TRIGGER IF EXISTS scheduler_outbox_proxies_update;
		DROP TRIGGER IF EXISTS scheduler_outbox_proxies_delete;
		DROP TRIGGER IF EXISTS scheduler_outbox_settings_update;
		DROP TRIGGER IF EXISTS grok_maintenance_accounts_insert;
		DROP TRIGGER IF EXISTS grok_maintenance_accounts_update;
		DROP TRIGGER IF EXISTS grok_maintenance_accounts_delete;

		CREATE TRIGGER scheduler_outbox_accounts_insert AFTER INSERT ON accounts BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',NEW.id,'created');
		END;
		CREATE TRIGGER scheduler_outbox_accounts_update AFTER UPDATE OF credentials,proxy_url,status,error_message,cooldown_reason,cooldown_until,enabled,locked,score_bias_override,base_concurrency_override,skip_warm_tier,tags,credit_enabled,credit_skip_usage_window,deleted_at ON accounts
		WHEN OLD.proxy_url IS NOT NEW.proxy_url OR OLD.status IS NOT NEW.status OR OLD.error_message IS NOT NEW.error_message
		  OR OLD.cooldown_reason IS NOT NEW.cooldown_reason OR OLD.cooldown_until IS NOT NEW.cooldown_until
		  OR OLD.enabled IS NOT NEW.enabled OR OLD.locked IS NOT NEW.locked
		  OR OLD.score_bias_override IS NOT NEW.score_bias_override OR OLD.base_concurrency_override IS NOT NEW.base_concurrency_override
		  OR OLD.skip_warm_tier IS NOT NEW.skip_warm_tier OR OLD.tags IS NOT NEW.tags
		  OR OLD.credit_enabled IS NOT NEW.credit_enabled OR OLD.credit_skip_usage_window IS NOT NEW.credit_skip_usage_window
		  OR OLD.deleted_at IS NOT NEW.deleted_at
		  OR COALESCE(json_extract(OLD.credentials,'$.refresh_token'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.refresh_token'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.session_token'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.session_token'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.access_token'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.access_token'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.upstream_type'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.upstream_type'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.base_url'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.base_url'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.api_key'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.api_key'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.models'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.models'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.model_mapping'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.model_mapping'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.custom_headers'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.custom_headers'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.allowed_api_key_ids'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.allowed_api_key_ids'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.plan_type'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.plan_type'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.dispatch_count_limit'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.dispatch_count_limit'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.scheduler_priority'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.scheduler_priority'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.auth_mode'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.auth_mode'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.agent_runtime_id'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.agent_runtime_id'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.agent_private_key'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.agent_private_key'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.task_id'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.task_id'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.grok_principal_id'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.grok_principal_id'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.grok_oidc_issuer'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.grok_oidc_issuer'),'')
		BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',NEW.id,'updated');
		END;
		CREATE TRIGGER scheduler_outbox_accounts_delete AFTER DELETE ON accounts BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',OLD.id,'deleted');
		END;

		CREATE TRIGGER scheduler_outbox_api_keys_insert AFTER INSERT ON api_keys BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('api_key',NEW.id,'created');
		END;
		CREATE TRIGGER scheduler_outbox_api_keys_update AFTER UPDATE OF allowed_group_ids,limits,expires_at ON api_keys BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('api_key',NEW.id,'updated');
		END;
		CREATE TRIGGER scheduler_outbox_api_keys_delete AFTER DELETE ON api_keys BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('api_key',OLD.id,'deleted');
		END;

		CREATE TRIGGER scheduler_outbox_groups_insert AFTER INSERT ON account_groups BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('group',NEW.id,'created');
		END;
		CREATE TRIGGER scheduler_outbox_groups_update AFTER UPDATE ON account_groups BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('group',NEW.id,'updated');
		END;
		CREATE TRIGGER scheduler_outbox_groups_delete AFTER DELETE ON account_groups BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('group',OLD.id,'deleted');
		END;

		CREATE TRIGGER scheduler_outbox_members_insert AFTER INSERT ON account_group_members BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',NEW.account_id,'membership_changed');
		END;
		CREATE TRIGGER scheduler_outbox_members_delete AFTER DELETE ON account_group_members BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',OLD.account_id,'membership_changed');
		END;

		CREATE TRIGGER scheduler_outbox_cooldowns_insert AFTER INSERT ON account_model_cooldowns BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',NEW.account_id,'model_cooldown_changed');
		END;
		CREATE TRIGGER scheduler_outbox_cooldowns_update AFTER UPDATE ON account_model_cooldowns
		WHEN OLD.reset_at IS NOT NEW.reset_at OR OLD.reason IS NOT NEW.reason OR OLD.model IS NOT NEW.model BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',NEW.account_id,'model_cooldown_changed');
		END;
		CREATE TRIGGER scheduler_outbox_cooldowns_delete AFTER DELETE ON account_model_cooldowns BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('account',OLD.account_id,'model_cooldown_changed');
		END;

		CREATE TRIGGER scheduler_outbox_proxies_insert AFTER INSERT ON proxies BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('proxy',NEW.id,'created');
		END;
		CREATE TRIGGER scheduler_outbox_proxies_update AFTER UPDATE ON proxies BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('proxy',NEW.id,'updated');
		END;
		CREATE TRIGGER scheduler_outbox_proxies_delete AFTER DELETE ON proxies BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('proxy',OLD.id,'deleted');
		END;

		CREATE TRIGGER scheduler_outbox_settings_update AFTER UPDATE ON system_settings BEGIN
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type) VALUES('settings',NEW.id,'updated');
		END;

		CREATE TRIGGER grok_maintenance_accounts_insert AFTER INSERT ON accounts
		WHEN LOWER(COALESCE(json_extract(NEW.credentials,'$.upstream_type'),''))='grok'
		 AND NEW.status<>'deleted' AND COALESCE(NEW.error_message,'')<>'deleted' AND COALESCE(NEW.enabled,1)<>0
		BEGIN
			INSERT INTO maintenance_jobs(entity_id,job_kind,due_at) VALUES(NEW.id,'grok_freshness',CURRENT_TIMESTAMP)
			ON CONFLICT(entity_id,job_kind) DO UPDATE SET due_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP;
		END;
		CREATE TRIGGER grok_maintenance_accounts_update AFTER UPDATE OF credentials,credential_generation,status,error_message,enabled,deleted_at ON accounts
		WHEN OLD.credential_generation IS NOT NEW.credential_generation
		  OR COALESCE(json_extract(OLD.credentials,'$.upstream_type'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.upstream_type'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.base_url'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.base_url'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.api_key'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.api_key'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.refresh_token'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.refresh_token'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.access_token'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.access_token'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.models'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.models'),'')
		  OR COALESCE(json_extract(OLD.credentials,'$.model_mapping'),'') IS NOT COALESCE(json_extract(NEW.credentials,'$.model_mapping'),'')
		  OR OLD.status IS NOT NEW.status OR OLD.error_message IS NOT NEW.error_message
		  OR OLD.enabled IS NOT NEW.enabled OR OLD.deleted_at IS NOT NEW.deleted_at
		BEGIN
			DELETE FROM maintenance_jobs WHERE entity_id=NEW.id AND job_kind='grok_freshness'
			 AND (LOWER(COALESCE(json_extract(NEW.credentials,'$.upstream_type'),''))<>'grok'
			      OR NEW.status='deleted' OR COALESCE(NEW.error_message,'')='deleted' OR COALESCE(NEW.enabled,1)=0);
			INSERT INTO maintenance_jobs(entity_id,job_kind,due_at)
			SELECT NEW.id,'grok_freshness',CURRENT_TIMESTAMP
			WHERE LOWER(COALESCE(json_extract(NEW.credentials,'$.upstream_type'),''))='grok'
			  AND NEW.status<>'deleted' AND COALESCE(NEW.error_message,'')<>'deleted' AND COALESCE(NEW.enabled,1)<>0
			ON CONFLICT(entity_id,job_kind) DO UPDATE SET due_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP;
		END;
		CREATE TRIGGER grok_maintenance_accounts_delete AFTER DELETE ON accounts BEGIN
			DELETE FROM maintenance_jobs WHERE entity_id=OLD.id AND job_kind='grok_freshness';
		END;
	`
	_, err := db.conn.ExecContext(ctx, query)
	return err
}

func (db *DB) installPostgresSchedulerOutboxTriggers(ctx context.Context) error {
	const query = `
		CREATE OR REPLACE FUNCTION codex2api_scheduler_outbox_row() RETURNS trigger AS $$
		DECLARE
			payload JSONB;
			entity_id BIGINT;
		BEGIN
			payload := CASE WHEN TG_OP = 'DELETE' THEN to_jsonb(OLD) ELSE to_jsonb(NEW) END;
			entity_id := COALESCE((payload ->> TG_ARGV[1])::BIGINT, 0);
			INSERT INTO scheduler_outbox(entity_type,entity_id,event_type,created_at)
			VALUES(TG_ARGV[0], entity_id, lower(TG_OP), NOW());
			IF TG_OP = 'DELETE' THEN
				RETURN OLD;
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS scheduler_outbox_accounts_insert ON accounts;
		DROP TRIGGER IF EXISTS scheduler_outbox_accounts_update ON accounts;
		DROP TRIGGER IF EXISTS scheduler_outbox_accounts_delete ON accounts;
		CREATE TRIGGER scheduler_outbox_accounts_insert AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','id');
		CREATE TRIGGER scheduler_outbox_accounts_update AFTER UPDATE OF credentials,proxy_url,status,error_message,cooldown_reason,cooldown_until,enabled,locked,score_bias_override,base_concurrency_override,skip_warm_tier,tags,credit_enabled,credit_skip_usage_window,deleted_at ON accounts FOR EACH ROW WHEN (
			OLD.proxy_url IS DISTINCT FROM NEW.proxy_url OR OLD.status IS DISTINCT FROM NEW.status OR OLD.error_message IS DISTINCT FROM NEW.error_message OR
			OLD.cooldown_reason IS DISTINCT FROM NEW.cooldown_reason OR OLD.cooldown_until IS DISTINCT FROM NEW.cooldown_until OR
			OLD.enabled IS DISTINCT FROM NEW.enabled OR OLD.locked IS DISTINCT FROM NEW.locked OR
			OLD.score_bias_override IS DISTINCT FROM NEW.score_bias_override OR OLD.base_concurrency_override IS DISTINCT FROM NEW.base_concurrency_override OR
			OLD.skip_warm_tier IS DISTINCT FROM NEW.skip_warm_tier OR OLD.tags IS DISTINCT FROM NEW.tags OR
			OLD.credit_enabled IS DISTINCT FROM NEW.credit_enabled OR OLD.credit_skip_usage_window IS DISTINCT FROM NEW.credit_skip_usage_window OR
			OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR
			COALESCE(OLD.credentials->>'refresh_token','') IS DISTINCT FROM COALESCE(NEW.credentials->>'refresh_token','') OR
			COALESCE(OLD.credentials->>'session_token','') IS DISTINCT FROM COALESCE(NEW.credentials->>'session_token','') OR
			COALESCE(OLD.credentials->>'access_token','') IS DISTINCT FROM COALESCE(NEW.credentials->>'access_token','') OR
			COALESCE(OLD.credentials->>'upstream_type','') IS DISTINCT FROM COALESCE(NEW.credentials->>'upstream_type','') OR
			COALESCE(OLD.credentials->>'base_url','') IS DISTINCT FROM COALESCE(NEW.credentials->>'base_url','') OR
			COALESCE(OLD.credentials->>'api_key','') IS DISTINCT FROM COALESCE(NEW.credentials->>'api_key','') OR
			COALESCE(OLD.credentials->'models','null'::jsonb) IS DISTINCT FROM COALESCE(NEW.credentials->'models','null'::jsonb) OR
			COALESCE(OLD.credentials->>'model_mapping','') IS DISTINCT FROM COALESCE(NEW.credentials->>'model_mapping','') OR
			COALESCE(OLD.credentials->'custom_headers','null'::jsonb) IS DISTINCT FROM COALESCE(NEW.credentials->'custom_headers','null'::jsonb) OR
			COALESCE(OLD.credentials->'allowed_api_key_ids','null'::jsonb) IS DISTINCT FROM COALESCE(NEW.credentials->'allowed_api_key_ids','null'::jsonb) OR
			COALESCE(OLD.credentials->>'plan_type','') IS DISTINCT FROM COALESCE(NEW.credentials->>'plan_type','') OR
			COALESCE(OLD.credentials->>'dispatch_count_limit','') IS DISTINCT FROM COALESCE(NEW.credentials->>'dispatch_count_limit','') OR
			COALESCE(OLD.credentials->>'scheduler_priority','') IS DISTINCT FROM COALESCE(NEW.credentials->>'scheduler_priority','') OR
			COALESCE(OLD.credentials->>'auth_mode','') IS DISTINCT FROM COALESCE(NEW.credentials->>'auth_mode','') OR
			COALESCE(OLD.credentials->>'agent_runtime_id','') IS DISTINCT FROM COALESCE(NEW.credentials->>'agent_runtime_id','') OR
			COALESCE(OLD.credentials->>'agent_private_key','') IS DISTINCT FROM COALESCE(NEW.credentials->>'agent_private_key','') OR
			COALESCE(OLD.credentials->>'task_id','') IS DISTINCT FROM COALESCE(NEW.credentials->>'task_id','') OR
			COALESCE(OLD.credentials->>'grok_principal_id','') IS DISTINCT FROM COALESCE(NEW.credentials->>'grok_principal_id','') OR
			COALESCE(OLD.credentials->>'grok_oidc_issuer','') IS DISTINCT FROM COALESCE(NEW.credentials->>'grok_oidc_issuer','')
		) EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','id');
		CREATE TRIGGER scheduler_outbox_accounts_delete AFTER DELETE ON accounts FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','id');

		DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_insert ON api_keys;
		DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_update ON api_keys;
		DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_delete ON api_keys;
		CREATE TRIGGER scheduler_outbox_api_keys_insert AFTER INSERT ON api_keys FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('api_key','id');
		CREATE TRIGGER scheduler_outbox_api_keys_update AFTER UPDATE OF allowed_group_ids,limits,expires_at ON api_keys FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('api_key','id');
		CREATE TRIGGER scheduler_outbox_api_keys_delete AFTER DELETE ON api_keys FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('api_key','id');

		DROP TRIGGER IF EXISTS scheduler_outbox_groups_insert ON account_groups;
		DROP TRIGGER IF EXISTS scheduler_outbox_groups_update ON account_groups;
		DROP TRIGGER IF EXISTS scheduler_outbox_groups_delete ON account_groups;
		CREATE TRIGGER scheduler_outbox_groups_insert AFTER INSERT ON account_groups FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('group','id');
		CREATE TRIGGER scheduler_outbox_groups_update AFTER UPDATE ON account_groups FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('group','id');
		CREATE TRIGGER scheduler_outbox_groups_delete AFTER DELETE ON account_groups FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('group','id');

		DROP TRIGGER IF EXISTS scheduler_outbox_members_insert ON account_group_members;
		DROP TRIGGER IF EXISTS scheduler_outbox_members_delete ON account_group_members;
		CREATE TRIGGER scheduler_outbox_members_insert AFTER INSERT ON account_group_members FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','account_id');
		CREATE TRIGGER scheduler_outbox_members_delete AFTER DELETE ON account_group_members FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','account_id');

		DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_insert ON account_model_cooldowns;
		DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_update ON account_model_cooldowns;
		DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_delete ON account_model_cooldowns;
		CREATE TRIGGER scheduler_outbox_cooldowns_insert AFTER INSERT ON account_model_cooldowns FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','account_id');
		CREATE TRIGGER scheduler_outbox_cooldowns_update AFTER UPDATE ON account_model_cooldowns FOR EACH ROW WHEN (
			OLD.reset_at IS DISTINCT FROM NEW.reset_at OR OLD.reason IS DISTINCT FROM NEW.reason OR OLD.model IS DISTINCT FROM NEW.model
		) EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','account_id');
		CREATE TRIGGER scheduler_outbox_cooldowns_delete AFTER DELETE ON account_model_cooldowns FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('account','account_id');

		DROP TRIGGER IF EXISTS scheduler_outbox_proxies_insert ON proxies;
		DROP TRIGGER IF EXISTS scheduler_outbox_proxies_update ON proxies;
		DROP TRIGGER IF EXISTS scheduler_outbox_proxies_delete ON proxies;
		CREATE TRIGGER scheduler_outbox_proxies_insert AFTER INSERT ON proxies FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('proxy','id');
		CREATE TRIGGER scheduler_outbox_proxies_update AFTER UPDATE ON proxies FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('proxy','id');
		CREATE TRIGGER scheduler_outbox_proxies_delete AFTER DELETE ON proxies FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('proxy','id');

		DROP TRIGGER IF EXISTS scheduler_outbox_settings_update ON system_settings;
		CREATE TRIGGER scheduler_outbox_settings_update AFTER UPDATE ON system_settings FOR EACH ROW EXECUTE FUNCTION codex2api_scheduler_outbox_row('settings','id');

		CREATE OR REPLACE FUNCTION codex2api_grok_maintenance_account() RETURNS trigger AS $$
		DECLARE
			account_id BIGINT;
		BEGIN
			IF TG_OP = 'DELETE' THEN
				DELETE FROM maintenance_jobs WHERE entity_id=OLD.id AND job_kind='grok_freshness';
				RETURN OLD;
			END IF;
			account_id := NEW.id;
			IF lower(COALESCE(NEW.credentials->>'upstream_type',''))='grok'
			   AND NEW.status<>'deleted' AND COALESCE(NEW.error_message,'')<>'deleted' AND COALESCE(NEW.enabled,true) THEN
				INSERT INTO maintenance_jobs(entity_id,job_kind,due_at,updated_at)
				VALUES(account_id,'grok_freshness',NOW(),NOW())
				ON CONFLICT(entity_id,job_kind) DO UPDATE SET due_at=NOW(),updated_at=NOW();
			ELSE
				DELETE FROM maintenance_jobs WHERE entity_id=account_id AND job_kind='grok_freshness';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS grok_maintenance_accounts_insert ON accounts;
		DROP TRIGGER IF EXISTS grok_maintenance_accounts_update ON accounts;
		DROP TRIGGER IF EXISTS grok_maintenance_accounts_delete ON accounts;
		CREATE TRIGGER grok_maintenance_accounts_insert AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION codex2api_grok_maintenance_account();
		CREATE TRIGGER grok_maintenance_accounts_update AFTER UPDATE OF credentials,credential_generation,status,error_message,enabled,deleted_at ON accounts
		FOR EACH ROW WHEN (
			OLD.credential_generation IS DISTINCT FROM NEW.credential_generation OR
			COALESCE(OLD.credentials->>'upstream_type','') IS DISTINCT FROM COALESCE(NEW.credentials->>'upstream_type','') OR
			COALESCE(OLD.credentials->>'base_url','') IS DISTINCT FROM COALESCE(NEW.credentials->>'base_url','') OR
			COALESCE(OLD.credentials->>'api_key','') IS DISTINCT FROM COALESCE(NEW.credentials->>'api_key','') OR
			COALESCE(OLD.credentials->>'refresh_token','') IS DISTINCT FROM COALESCE(NEW.credentials->>'refresh_token','') OR
			COALESCE(OLD.credentials->>'access_token','') IS DISTINCT FROM COALESCE(NEW.credentials->>'access_token','') OR
			COALESCE(OLD.credentials->'models','null'::jsonb) IS DISTINCT FROM COALESCE(NEW.credentials->'models','null'::jsonb) OR
			COALESCE(OLD.credentials->>'model_mapping','') IS DISTINCT FROM COALESCE(NEW.credentials->>'model_mapping','') OR
			OLD.status IS DISTINCT FROM NEW.status OR OLD.error_message IS DISTINCT FROM NEW.error_message OR
			OLD.enabled IS DISTINCT FROM NEW.enabled OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
		) EXECUTE FUNCTION codex2api_grok_maintenance_account();
		CREATE TRIGGER grok_maintenance_accounts_delete AFTER DELETE ON accounts FOR EACH ROW EXECUTE FUNCTION codex2api_grok_maintenance_account();
	`
	_, err := db.conn.ExecContext(ctx, query)
	return err
}
