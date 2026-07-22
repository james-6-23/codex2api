package database

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

const oauthIdentityUniqueIndexName = "idx_accounts_oauth_identity_active"

// ErrDuplicateOAuthIdentity indicates that an active OAuth account already
// owns the same normalized email + workspace_id identity.
var ErrDuplicateOAuthIdentity = errors.New("duplicate oauth identity")

func oauthIdentityEmailExpr(isSQLite bool) string {
	if isSQLite {
		return `lower(trim(CASE WHEN json_valid(credentials) THEN COALESCE(json_extract(credentials, '$.email'), '') ELSE '' END))`
	}
	return `lower(trim(COALESCE(credentials->>'email', '')))`
}

func oauthIdentityWorkspaceExpr(isSQLite bool) string {
	if isSQLite {
		return `trim(CASE WHEN json_valid(credentials) THEN COALESCE(json_extract(credentials, '$.workspace_id'), '') ELSE '' END)`
	}
	return `trim(COALESCE(credentials->>'workspace_id', ''))`
}

func oauthIdentityUniqueIndexSQL(isSQLite bool) string {
	emailExpr := oauthIdentityEmailExpr(isSQLite)
	workspaceExpr := oauthIdentityWorkspaceExpr(isSQLite)
	return fmt.Sprintf(`
		CREATE UNIQUE INDEX IF NOT EXISTS %s
		ON accounts (%s, %s)
		WHERE type = 'oauth'
		  AND status <> 'deleted'
		  AND COALESCE(error_message, '') <> 'deleted'
		  AND %s <> ''
		  AND %s <> ''
		  AND lower(%s) NOT LIKE 'user-%%'
	`, oauthIdentityUniqueIndexName, emailExpr, workspaceExpr, emailExpr, workspaceExpr, workspaceExpr)
}

func wrapOAuthIdentityConstraintError(err error) error {
	if err == nil {
		return nil
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Constraint == oauthIdentityUniqueIndexName {
		return fmt.Errorf("%w: %v", ErrDuplicateOAuthIdentity, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(oauthIdentityUniqueIndexName)) {
		return fmt.Errorf("%w: %v", ErrDuplicateOAuthIdentity, err)
	}
	return err
}
