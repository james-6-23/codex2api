package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const CodexInstallationIDCredentialKey = "codex_installation_id"

func prepareCodexDeviceCredentials(credentials map[string]interface{}) (map[string]interface{}, error) {
	upstreamType, _ := credentials["upstream_type"].(string)
	if upstreamType = strings.ToLower(strings.TrimSpace(upstreamType)); upstreamType != "" && upstreamType != "codex" {
		return credentials, nil
	}
	prepared := make(map[string]interface{}, len(credentials)+1)
	for key, value := range credentials {
		prepared[key] = value
	}
	installationID, _ := prepared[CodexInstallationIDCredentialKey].(string)
	installationID = strings.TrimSpace(installationID)
	if installationID == "" {
		generated, err := uuid.NewRandom()
		if err != nil {
			return nil, fmt.Errorf("generate Codex installation ID: %w", err)
		}
		installationID = generated.String()
	}
	prepared[CodexInstallationIDCredentialKey] = installationID
	return prepared, nil
}

func (db *DB) EnsureCodexInstallationID(ctx context.Context, accountID int64) (string, error) {
	var installationID string
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT credentials FROM accounts WHERE id = $1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw interface{}
		if err := tx.QueryRowContext(ctx, query, accountID).Scan(&raw); err != nil {
			return err
		}
		credentials := decodeCredentials(raw)
		installationID, _ = credentials[CodexInstallationIDCredentialKey].(string)
		installationID = strings.TrimSpace(installationID)
		if installationID != "" {
			return nil
		}
		prepared, err := prepareCodexDeviceCredentials(credentials)
		if err != nil {
			return err
		}
		installationID, _ = prepared[CodexInstallationIDCredentialKey].(string)
		if installationID == "" {
			return nil
		}
		query = `UPDATE accounts SET credentials = jsonb_set(COALESCE(credentials, '{}'::jsonb), '{codex_installation_id}', to_jsonb($1::text), true), updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		if db.isSQLite() {
			query = `UPDATE accounts SET credentials = json_set(COALESCE(credentials, '{}'), '$.codex_installation_id', $1), updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		}
		_, err = tx.ExecContext(ctx, query, installationID, accountID)
		return err
	})
	if err != nil {
		return "", err
	}
	return installationID, nil
}
