package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type AccountGroup struct {
	ID                      int64
	Name                    string
	Description             string
	Color                   string
	SortOrder               int64
	BaseConcurrencyOverride sql.NullInt64
	MemberCount             int64
	AutoPause5hThreshold    float64
	AutoPause7dThreshold    float64
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

func (db *DB) ListAccountGroups(ctx context.Context) ([]AccountGroup, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT g.id, g.name, g.description, g.color, g.sort_order, g.base_concurrency_override,
			COALESCE(COUNT(a.id), 0),
			COALESCE(g.auto_pause_5h_threshold, 0), COALESCE(g.auto_pause_7d_threshold, 0),
			g.created_at, g.updated_at
		FROM account_groups g
		LEFT JOIN account_group_members m ON m.group_id = g.id
		LEFT JOIN accounts a ON a.id = m.account_id
			AND a.status <> 'deleted'
			AND COALESCE(a.error_message, '') <> 'deleted'
		GROUP BY g.id, g.name, g.description, g.color, g.sort_order, g.base_concurrency_override,
			g.auto_pause_5h_threshold, g.auto_pause_7d_threshold, g.created_at, g.updated_at
		ORDER BY g.sort_order, g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]AccountGroup, 0)
	for rows.Next() {
		var g AccountGroup
		var createdRaw, updatedRaw interface{}
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.SortOrder, &g.BaseConcurrencyOverride, &g.MemberCount, &g.AutoPause5hThreshold, &g.AutoPause7dThreshold, &createdRaw, &updatedRaw); err != nil {
			return nil, err
		}
		var parseErr error
		g.CreatedAt, parseErr = parseDBTimeValue(createdRaw)
		if parseErr != nil {
			return nil, parseErr
		}
		g.UpdatedAt, parseErr = parseDBTimeValue(updatedRaw)
		if parseErr != nil {
			return nil, parseErr
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (db *DB) CreateAccountGroup(ctx context.Context, name, description, color string, autoPause5h, autoPause7d float64, baseConcurrencyOverride sql.NullInt64, sortOrder ...int64) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("group name is required")
	}
	order := int64(0)
	if len(sortOrder) > 0 {
		order = sortOrder[0]
	}
	if db.isSQLite() {
		res, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name, description, color, sort_order, auto_pause_5h_threshold, auto_pause_7d_threshold, base_concurrency_override) VALUES (?, ?, ?, ?, ?, ?, ?)`, name, description, color, order, autoPause5h, autoPause7d, nullableInt64Value(baseConcurrencyOverride))
		if err != nil {
			if isUniqueViolation(err) {
				return 0, ErrDuplicateAccountGroupName
			}
			return 0, err
		}
		return res.LastInsertId()
	}
	var id int64
	err := db.conn.QueryRowContext(ctx, `INSERT INTO account_groups (name, description, color, sort_order, auto_pause_5h_threshold, auto_pause_7d_threshold, base_concurrency_override) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, name, description, color, order, autoPause5h, autoPause7d, nullableInt64Value(baseConcurrencyOverride)).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicateAccountGroupName
		}
		return 0, err
	}
	return id, nil
}

type UpdateAccountGroupOpts struct {
	AutoPause5hThreshold    *float64
	AutoPause7dThreshold    *float64
	BaseConcurrencyOverride OptionalNullInt64
}

func (db *DB) UpdateAccountGroup(ctx context.Context, id int64, name, description, color *string, opts *UpdateAccountGroupOpts, sortOrder ...*int64) error {
	sets := make([]string, 0, 5)
	args := make([]interface{}, 0, 6)
	add := func(col string, value interface{}) {
		args = append(args, value)
		ph := "?"
		if !db.isSQLite() {
			ph = fmt.Sprintf("$%d", len(args))
		}
		sets = append(sets, col+" = "+ph)
	}
	if name != nil {
		clean := strings.TrimSpace(*name)
		if clean == "" {
			return fmt.Errorf("group name is required")
		}
		add("name", clean)
	}
	if description != nil {
		add("description", *description)
	}
	if color != nil {
		add("color", *color)
	}
	if len(sortOrder) > 0 && sortOrder[0] != nil {
		add("sort_order", *sortOrder[0])
	}
	if opts != nil {
		if opts.AutoPause5hThreshold != nil {
			add("auto_pause_5h_threshold", *opts.AutoPause5hThreshold)
		}
		if opts.AutoPause7dThreshold != nil {
			add("auto_pause_7d_threshold", *opts.AutoPause7dThreshold)
		}
		if opts.BaseConcurrencyOverride.Set {
			add("base_concurrency_override", nullableInt64Value(opts.BaseConcurrencyOverride.Value))
		}
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, id)
	ph := "?"
	if !db.isSQLite() {
		ph = fmt.Sprintf("$%d", len(args))
	}
	res, err := db.conn.ExecContext(ctx, "UPDATE account_groups SET "+strings.Join(sets, ", ")+" WHERE id = "+ph, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateAccountGroupName
		}
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) DeleteAccountGroup(ctx context.Context, id int64, force ...bool) error {
	allowMembers := len(force) > 0 && force[0]
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ph := "$1"
	if db.isSQLite() {
		ph = "?"
	}
	var count int64
	memberCountQuery := `
		SELECT COUNT(*)
		FROM account_group_members m
		JOIN accounts a ON a.id = m.account_id
		WHERE m.group_id = ` + ph + ` AND a.status <> 'deleted' AND COALESCE(a.error_message, '') <> 'deleted'`
	if err := tx.QueryRowContext(ctx, memberCountQuery, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 && !allowMembers {
		return ErrAccountGroupNotEmpty
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_group_members WHERE group_id = "+ph, id); err != nil {
		return err
	}
	if err := pruneDeletedGroupFromAPIKeyScopes(ctx, tx, db.isSQLite(), id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM account_groups WHERE id = "+ph, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func pruneDeletedGroupFromAPIKeyScopes(ctx context.Context, tx *sql.Tx, sqlite bool, groupID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(allowed_group_ids, '[]') FROM api_keys`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type update struct {
		id     int64
		groups []int64
	}
	updates := make([]update, 0)
	for rows.Next() {
		var id int64
		var raw interface{}
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		groups := decodeInt64SliceValue(raw)
		if !containsInt64(groups, groupID) {
			continue
		}
		nextGroups := removeInt64(groups, groupID)
		// If the deleted group was the key's only allowed group, keep the stale
		// ID instead of broadening the key into an unrestricted key.
		if len(nextGroups) == 0 && len(groups) > 0 {
			continue
		}
		updates = append(updates, update{id: id, groups: nextGroups})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	query := `UPDATE api_keys SET allowed_group_ids = $1::jsonb WHERE id = $2`
	if sqlite {
		query = `UPDATE api_keys SET allowed_group_ids = ? WHERE id = ?`
	}
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx, query, encodeInt64SliceJSON(item.groups), item.id); err != nil {
			return err
		}
	}
	return nil
}

func removeInt64(slice []int64, target int64) []int64 {
	out := make([]int64, 0, len(slice))
	for _, v := range slice {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

func containsInt64(slice []int64, target int64) bool {
	for _, v := range slice {
		if v == target {
			return true
		}
	}
	return false
}

func (db *DB) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ph := "$1"
	insertQ := "INSERT INTO account_group_members (account_id, group_id) VALUES ($1, $2)"
	if db.isSQLite() {
		ph = "?"
		insertQ = "INSERT INTO account_group_members (account_id, group_id) VALUES (?, ?)"
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_group_members WHERE account_id = "+ph, accountID); err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, gid := range groupIDs {
		if gid <= 0 {
			continue
		}
		if _, ok := seen[gid]; ok {
			continue
		}
		seen[gid] = struct{}{}
		if _, err := tx.ExecContext(ctx, insertQ, accountID, gid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) GetAccountGroupIDs(ctx context.Context, accountID int64) ([]int64, error) {
	query := "SELECT group_id FROM account_group_members WHERE account_id = $1 ORDER BY group_id"
	if db.isSQLite() {
		query = "SELECT group_id FROM account_group_members WHERE account_id = ? ORDER BY group_id"
	}
	rows, err := db.conn.QueryContext(ctx, query, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) ListAccountIDsInGroups(ctx context.Context, groupIDs []int64) ([]int64, error) {
	groupIDs = normalizeIDSlice(groupIDs)
	if len(groupIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]interface{}, len(groupIDs))
	for i, id := range groupIDs {
		if db.isSQLite() {
			placeholders[i] = "?"
		} else {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		args[i] = id
	}
	rows, err := db.conn.QueryContext(ctx, fmt.Sprintf("SELECT DISTINCT account_id FROM account_group_members WHERE group_id IN (%s) ORDER BY account_id", strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) ListAccountGroupMemberships(ctx context.Context) (map[int64][]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id, group_id FROM account_group_members ORDER BY account_id, group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]int64)
	for rows.Next() {
		var accountID, groupID int64
		if err := rows.Scan(&accountID, &groupID); err != nil {
			return nil, err
		}
		out[accountID] = append(out[accountID], groupID)
	}
	return out, rows.Err()
}

func (db *DB) VerifyAccountGroupIDs(ctx context.Context, ids []int64) ([]int64, error) {
	ids = normalizeIDSlice(ids)
	if len(ids) == 0 {
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
	rows, err := db.conn.QueryContext(ctx, fmt.Sprintf("SELECT id FROM account_groups WHERE id IN (%s)", strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exists := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		exists[id] = struct{}{}
	}
	missing := make([]int64, 0)
	for _, id := range ids {
		if _, ok := exists[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing, rows.Err()
}

func (db *DB) UpdateAccountTags(ctx context.Context, id int64, tags []string) error {
	payload := encodeTagsJSON(tags)
	query := `UPDATE accounts SET tags = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	if !db.isSQLite() {
		query = `UPDATE accounts SET tags = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	}
	res, err := db.conn.ExecContext(ctx, query, payload, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) UpdateAccountProxyURL(ctx context.Context, id int64, proxyURL string) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE accounts SET proxy_url = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, strings.TrimSpace(proxyURL), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func normalizeIDSlice(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

var ErrDuplicateAccountGroupName = fmt.Errorf("account group name already exists")
var ErrAccountGroupNotEmpty = fmt.Errorf("account group still has members")

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key") || strings.Contains(msg, "23505")
}
