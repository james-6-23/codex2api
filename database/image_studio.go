package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	ImageJobQueued    = "queued"
	ImageJobRunning   = "running"
	ImageJobSucceeded = "succeeded"
	ImageJobFailed    = "failed"
)

type ImagePromptTemplate struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Prompt       string    `json:"prompt"`
	Model        string    `json:"model"`
	Size         string    `json:"size"`
	Quality      string    `json:"quality"`
	OutputFormat string    `json:"output_format"`
	Background   string    `json:"background"`
	Style        string    `json:"style"`
	Tags         []string  `json:"tags"`
	Favorite     bool      `json:"favorite"`
	UsageCount   int       `json:"usage_count"`
	LastUsedAt   time.Time `json:"last_used_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ImagePromptTemplateInput struct {
	Name         string
	Prompt       string
	Model        string
	Size         string
	Quality      string
	OutputFormat string
	Background   string
	Style        string
	Tags         []string
	Favorite     bool
}

type ImageGenerationJob struct {
	ID           int64        `json:"id"`
	Status       string       `json:"status"`
	Prompt       string       `json:"prompt"`
	ParamsJSON   string       `json:"params_json"`
	APIKeyID     int64        `json:"api_key_id"`
	APIKeyName   string       `json:"api_key_name"`
	APIKeyMasked string       `json:"api_key_masked"`
	ErrorMessage string       `json:"error_message"`
	DurationMs   int          `json:"duration_ms"`
	CreatedAt    time.Time    `json:"created_at"`
	StartedAt    *time.Time   `json:"started_at,omitempty"`
	CompletedAt  *time.Time   `json:"completed_at,omitempty"`
	Assets       []ImageAsset `json:"assets,omitempty"`
}

type ImageGenerationJobInput struct {
	Prompt       string
	ParamsJSON   string
	APIKeyID     int64
	APIKeyName   string
	APIKeyMasked string
}

type ImageAsset struct {
	ID            int64     `json:"id"`
	JobID         int64     `json:"job_id"`
	TemplateID    int64     `json:"template_id"`
	Filename      string    `json:"filename"`
	StoragePath   string    `json:"-"`
	ProxyURL      string    `json:"proxy_url,omitempty"`
	ThumbnailURL  string    `json:"thumbnail_url,omitempty"`
	MimeType      string    `json:"mime_type"`
	Bytes         int       `json:"bytes"`
	Width         int       `json:"width"`
	Height        int       `json:"height"`
	Model         string    `json:"model"`
	RequestedSize string    `json:"requested_size"`
	ActualSize    string    `json:"actual_size"`
	Quality       string    `json:"quality"`
	OutputFormat  string    `json:"output_format"`
	RevisedPrompt string    `json:"revised_prompt"`
	CreatedAt     time.Time `json:"created_at"`
	CacheB64JSON  string    `json:"cache_b64_json,omitempty"`
}

type ImageAssetInput struct {
	JobID         int64
	TemplateID    int64
	Filename      string
	StoragePath   string
	MimeType      string
	Bytes         int
	Width         int
	Height        int
	Model         string
	RequestedSize string
	ActualSize    string
	Quality       string
	OutputFormat  string
	RevisedPrompt string
}

type ImageAssetPage struct {
	Assets []ImageAsset `json:"assets"`
	Total  int64        `json:"total"`
}

type ImageJobPage struct {
	Jobs  []ImageGenerationJob `json:"jobs"`
	Total int64                `json:"total"`
}

func normalizeImageTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len([]rune(tag)) > 40 {
			tag = string([]rune(tag)[:40])
		}
		key := strings.ToLower(tag)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, tag)
		if len(result) >= 12 {
			break
		}
	}
	return result
}

func encodeImageTags(tags []string) string {
	data, err := json.Marshal(normalizeImageTags(tags))
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeImageTags(raw string) []string {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	return normalizeImageTags(tags)
}

func (db *DB) InsertImagePromptTemplate(ctx context.Context, input ImagePromptTemplateInput) (int64, error) {
	return db.insertRowID(ctx,
		`INSERT INTO image_prompt_templates (name, prompt, model, size, quality, output_format, background, style, tags, favorite)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		`INSERT INTO image_prompt_templates (name, prompt, model, size, quality, output_format, background, style, tags, favorite)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		input.Name, input.Prompt, input.Model, input.Size, input.Quality, input.OutputFormat, input.Background, input.Style, encodeImageTags(input.Tags), input.Favorite,
	)
}

func (db *DB) UpdateImagePromptTemplate(ctx context.Context, id int64, input ImagePromptTemplateInput) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_prompt_templates
		SET name=$1, prompt=$2, model=$3, size=$4, quality=$5, output_format=$6, background=$7, style=$8,
			tags=$9, favorite=$10, updated_at=CURRENT_TIMESTAMP
		WHERE id=$11
	`, input.Name, input.Prompt, input.Model, input.Size, input.Quality, input.OutputFormat, input.Background, input.Style, encodeImageTags(input.Tags), input.Favorite, id)
	return err
}

func (db *DB) DeleteImagePromptTemplate(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM image_prompt_templates WHERE id=$1`, id)
	return err
}

func (db *DB) GetImagePromptTemplate(ctx context.Context, id int64) (*ImagePromptTemplate, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, name, prompt, model, size, quality, output_format, background, style, tags, favorite,
			usage_count, last_used_at, created_at, updated_at
		FROM image_prompt_templates WHERE id=$1
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanImagePromptTemplate(rows)
}

func (db *DB) ListImagePromptTemplates(ctx context.Context, search string, tag string) ([]ImagePromptTemplate, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, name, prompt, model, size, quality, output_format, background, style, tags, favorite,
			usage_count, last_used_at, created_at, updated_at
		FROM image_prompt_templates
		ORDER BY favorite DESC, updated_at DESC, id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	search = strings.ToLower(strings.TrimSpace(search))
	tag = strings.ToLower(strings.TrimSpace(tag))
	result := make([]ImagePromptTemplate, 0)
	for rows.Next() {
		tpl, err := scanImagePromptTemplate(rows)
		if err != nil {
			return nil, err
		}
		if search != "" {
			haystack := strings.ToLower(tpl.Name + "\n" + tpl.Prompt + "\n" + strings.Join(tpl.Tags, "\n"))
			if !strings.Contains(haystack, search) {
				continue
			}
		}
		if tag != "" {
			matched := false
			for _, item := range tpl.Tags {
				if strings.ToLower(item) == tag {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		result = append(result, *tpl)
	}
	return result, rows.Err()
}

func scanImagePromptTemplate(scanner interface {
	Scan(dest ...interface{}) error
}) (*ImagePromptTemplate, error) {
	var tpl ImagePromptTemplate
	var tagsRaw string
	var lastUsedRaw, createdRaw, updatedRaw interface{}
	if err := scanner.Scan(
		&tpl.ID, &tpl.Name, &tpl.Prompt, &tpl.Model, &tpl.Size, &tpl.Quality, &tpl.OutputFormat,
		&tpl.Background, &tpl.Style, &tagsRaw, &tpl.Favorite, &tpl.UsageCount, &lastUsedRaw, &createdRaw, &updatedRaw,
	); err != nil {
		return nil, err
	}
	lastUsed, err := parseDBNullTimeValue(lastUsedRaw)
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		tpl.LastUsedAt = lastUsed.Time
	}
	tpl.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, err
	}
	tpl.UpdatedAt, err = parseDBTimeValue(updatedRaw)
	if err != nil {
		return nil, err
	}
	tpl.Tags = decodeImageTags(tagsRaw)
	return &tpl, nil
}

func (db *DB) IncrementImagePromptTemplateUsage(ctx context.Context, id int64) error {
	if id <= 0 {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_prompt_templates
		SET usage_count = usage_count + 1, last_used_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id=$1
	`, id)
	return err
}

func (db *DB) InsertImageGenerationJob(ctx context.Context, input ImageGenerationJobInput) (int64, error) {
	if strings.TrimSpace(input.ParamsJSON) == "" {
		input.ParamsJSON = "{}"
	}
	return db.insertRowID(ctx,
		`INSERT INTO image_generation_jobs (status, prompt, params_json, api_key_id, api_key_name, api_key_masked)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		`INSERT INTO image_generation_jobs (status, prompt, params_json, api_key_id, api_key_name, api_key_masked)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		ImageJobQueued, input.Prompt, input.ParamsJSON, input.APIKeyID, input.APIKeyName, input.APIKeyMasked,
	)
}

func (db *DB) MarkImageJobRunning(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_generation_jobs
		SET status=$1, started_at=CURRENT_TIMESTAMP, error_message=''
		WHERE id=$2
	`, ImageJobRunning, id)
	return err
}

func (db *DB) MarkImageJobSucceeded(ctx context.Context, id int64, durationMs int) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_generation_jobs
		SET status=$1, duration_ms=$2, completed_at=CURRENT_TIMESTAMP, error_message=''
		WHERE id=$3
	`, ImageJobSucceeded, durationMs, id)
	return err
}

func (db *DB) MarkImageJobFailed(ctx context.Context, id int64, message string, durationMs int) error {
	if len([]rune(message)) > 2000 {
		message = string([]rune(message)[:2000])
	}
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_generation_jobs
		SET status=$1, error_message=$2, duration_ms=$3, completed_at=CURRENT_TIMESTAMP
		WHERE id=$4
	`, ImageJobFailed, message, durationMs, id)
	return err
}

func (db *DB) UpdateImageGenerationJobParamsJSON(ctx context.Context, id int64, paramsJSON string) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_generation_jobs
		SET params_json=$1
		WHERE id=$2
	`, paramsJSON, id)
	return err
}

func (db *DB) MarkInterruptedImageJobs(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE image_generation_jobs
		SET status=$1, error_message=$2, completed_at=CURRENT_TIMESTAMP
		WHERE status IN ($3,$4)
	`, ImageJobFailed, "服务重启，未完成的生图任务已中断", ImageJobQueued, ImageJobRunning)
	return err
}

func (db *DB) GetImageGenerationJob(ctx context.Context, id int64) (*ImageGenerationJob, error) {
	job, err := scanImageGenerationJob(db.conn.QueryRowContext(ctx, `
		SELECT id, status, prompt, params_json, api_key_id, api_key_name, api_key_masked, error_message,
			duration_ms, created_at, started_at, completed_at
		FROM image_generation_jobs WHERE id=$1
	`, id))
	if err != nil {
		return nil, err
	}
	assets, err := db.ListImageAssetsByJobID(ctx, id)
	if err != nil {
		return nil, err
	}
	job.Assets = assets
	return job, nil
}

// ListImageGenerationJobs returns image jobs newest-first.
// When apiKeyID > 0, only jobs for that API key are returned (portal isolation).
// When apiKeyID == 0, all jobs are returned (admin global list).
func (db *DB) ListImageGenerationJobs(ctx context.Context, page, pageSize int, apiKeyID int64) (*ImageJobPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	var total int64
	var rows *sql.Rows
	var err error
	if apiKeyID > 0 {
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM image_generation_jobs WHERE api_key_id=$1`, apiKeyID).Scan(&total); err != nil {
			return nil, err
		}
		rows, err = db.conn.QueryContext(ctx, `
			SELECT id, status, prompt, params_json, api_key_id, api_key_name, api_key_masked, error_message,
				duration_ms, created_at, started_at, completed_at
			FROM image_generation_jobs
			WHERE api_key_id=$1
			ORDER BY created_at DESC, id DESC
			LIMIT $2 OFFSET $3
		`, apiKeyID, pageSize, (page-1)*pageSize)
	} else {
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM image_generation_jobs`).Scan(&total); err != nil {
			return nil, err
		}
		rows, err = db.conn.QueryContext(ctx, `
			SELECT id, status, prompt, params_json, api_key_id, api_key_name, api_key_masked, error_message,
				duration_ms, created_at, started_at, completed_at
			FROM image_generation_jobs
			ORDER BY created_at DESC, id DESC
			LIMIT $1 OFFSET $2
		`, pageSize, (page-1)*pageSize)
	}
	if err != nil {
		return nil, err
	}
	jobs := make([]ImageGenerationJob, 0)
	for rows.Next() {
		job, err := scanImageGenerationJob(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for idx := range jobs {
		assets, err := db.ListImageAssetsByJobID(ctx, jobs[idx].ID)
		if err != nil {
			return nil, err
		}
		jobs[idx].Assets = assets
	}
	return &ImageJobPage{Jobs: jobs, Total: total}, nil
}

func (db *DB) DeleteImageGenerationJob(ctx context.Context, id int64) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM image_assets WHERE job_id=$1`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM image_generation_jobs WHERE id=$1`, id)
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

func scanImageGenerationJob(scanner interface {
	Scan(dest ...interface{}) error
}) (*ImageGenerationJob, error) {
	var job ImageGenerationJob
	var createdRaw, startedRaw, completedRaw interface{}
	if err := scanner.Scan(
		&job.ID, &job.Status, &job.Prompt, &job.ParamsJSON, &job.APIKeyID, &job.APIKeyName, &job.APIKeyMasked,
		&job.ErrorMessage, &job.DurationMs, &createdRaw, &startedRaw, &completedRaw,
	); err != nil {
		return nil, err
	}
	var err error
	job.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, err
	}
	startedAt, err := parseDBNullTimeValue(startedRaw)
	if err != nil {
		return nil, err
	}
	if startedAt.Valid {
		value := startedAt.Time
		job.StartedAt = &value
	}
	completedAt, err := parseDBNullTimeValue(completedRaw)
	if err != nil {
		return nil, err
	}
	if completedAt.Valid {
		value := completedAt.Time
		job.CompletedAt = &value
	}
	return &job, nil
}

func (db *DB) InsertImageAsset(ctx context.Context, input ImageAssetInput) (int64, error) {
	return db.insertRowID(ctx,
		`INSERT INTO image_assets (
			job_id, template_id, filename, storage_path, mime_type, bytes, width, height,
			model, requested_size, actual_size, quality, output_format, revised_prompt
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		`INSERT INTO image_assets (
			job_id, template_id, filename, storage_path, mime_type, bytes, width, height,
			model, requested_size, actual_size, quality, output_format, revised_prompt
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		input.JobID, input.TemplateID, input.Filename, input.StoragePath, input.MimeType, input.Bytes, input.Width, input.Height,
		input.Model, input.RequestedSize, input.ActualSize, input.Quality, input.OutputFormat, input.RevisedPrompt,
	)
}

func (db *DB) GetImageAsset(ctx context.Context, id int64) (*ImageAsset, error) {
	rows, err := db.conn.QueryContext(ctx, imageAssetSelectSQL("")+` WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanImageAsset(rows)
}

// ListImageAssets returns assets newest-first.
// When apiKeyID > 0, only assets belonging to jobs of that API key are returned.
// When apiKeyID == 0, all assets are returned (admin global list).
func (db *DB) ListImageAssets(ctx context.Context, page, pageSize int, apiKeyID int64) (*ImageAssetPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	var total int64
	var rows *sql.Rows
	var err error
	if apiKeyID > 0 {
		if err := db.conn.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM image_assets a
			INNER JOIN image_generation_jobs j ON j.id = a.job_id
			WHERE j.api_key_id=$1
		`, apiKeyID).Scan(&total); err != nil {
			return nil, err
		}
		rows, err = db.conn.QueryContext(ctx, imageAssetSelectSQL("a")+`
			INNER JOIN image_generation_jobs j ON j.id = a.job_id
			WHERE j.api_key_id=$1
			ORDER BY a.created_at DESC, a.id DESC
			LIMIT $2 OFFSET $3
		`, apiKeyID, pageSize, (page-1)*pageSize)
	} else {
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM image_assets`).Scan(&total); err != nil {
			return nil, err
		}
		rows, err = db.conn.QueryContext(ctx, imageAssetSelectSQL("")+`
			ORDER BY created_at DESC, id DESC
			LIMIT $1 OFFSET $2
		`, pageSize, (page-1)*pageSize)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets, err := scanImageAssets(rows)
	if err != nil {
		return nil, err
	}
	return &ImageAssetPage{Assets: assets, Total: total}, nil
}

func (db *DB) ListImageAssetsByJobID(ctx context.Context, jobID int64) ([]ImageAsset, error) {
	rows, err := db.conn.QueryContext(ctx, imageAssetSelectSQL("")+`
		WHERE job_id=$1
		ORDER BY id ASC
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanImageAssets(rows)
}

// GetImageAssetJobAPIKeyID returns the owning API key id for an asset via its job.
// Returns 0 when the asset has no job linkage.
func (db *DB) GetImageAssetJobAPIKeyID(ctx context.Context, assetID int64) (int64, error) {
	var apiKeyID int64
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(j.api_key_id, 0)
		FROM image_assets a
		LEFT JOIN image_generation_jobs j ON j.id = a.job_id
		WHERE a.id=$1
	`, assetID).Scan(&apiKeyID)
	if err != nil {
		return 0, err
	}
	return apiKeyID, nil
}

func (db *DB) DeleteImageAsset(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM image_assets WHERE id=$1`, id)
	return err
}

// imageAssetSelectSQL builds the asset SELECT. When tableAlias is non-empty (e.g. "a"),
// columns are qualified and FROM uses that alias.
func imageAssetSelectSQL(tableAlias string) string {
	alias := strings.TrimSpace(tableAlias)
	prefix := ""
	from := "image_assets"
	if alias != "" {
		prefix = alias + "."
		from = "image_assets " + alias
	}
	return `SELECT ` + prefix + `id, ` + prefix + `job_id, ` + prefix + `template_id, ` + prefix + `filename, ` + prefix + `storage_path, ` + prefix + `mime_type, ` + prefix + `bytes, ` + prefix + `width, ` + prefix + `height,
		` + prefix + `model, ` + prefix + `requested_size, ` + prefix + `actual_size, ` + prefix + `quality, ` + prefix + `output_format, ` + prefix + `revised_prompt, ` + prefix + `created_at
		FROM ` + from
}

func scanImageAssets(rows *sql.Rows) ([]ImageAsset, error) {
	assets := make([]ImageAsset, 0)
	for rows.Next() {
		asset, err := scanImageAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, *asset)
	}
	return assets, rows.Err()
}

func scanImageAsset(scanner interface {
	Scan(dest ...interface{}) error
}) (*ImageAsset, error) {
	var asset ImageAsset
	var createdRaw interface{}
	if err := scanner.Scan(
		&asset.ID, &asset.JobID, &asset.TemplateID, &asset.Filename, &asset.StoragePath, &asset.MimeType,
		&asset.Bytes, &asset.Width, &asset.Height, &asset.Model, &asset.RequestedSize, &asset.ActualSize,
		&asset.Quality, &asset.OutputFormat, &asset.RevisedPrompt, &createdRaw,
	); err != nil {
		return nil, err
	}
	var err error
	asset.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, err
	}
	return &asset, nil
}

func normalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

func (db *DB) GetAPIKeyByID(ctx context.Context, id int64) (*APIKeyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+apiKeySelectColumns+` FROM api_keys WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanAPIKeyRow(rows)
}

func (db *DB) FirstAPIKey(ctx context.Context) (*APIKeyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+apiKeySelectColumns+` FROM api_keys ORDER BY id LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanAPIKeyRow(rows)
}

func scanAPIKeyRow(scanner interface {
	Scan(dest ...interface{}) error
}) (*APIKeyRow, error) {
	row := &APIKeyRow{}
	var createdAtRaw, expiresAtRaw, lastResetAtRaw, allowedGroupsRaw, limitsRaw interface{}
	if err := scanner.Scan(&row.ID, &row.Name, &row.Key, &createdAtRaw, &row.QuotaLimit, &row.QuotaUsed, &row.TotalUsed, &row.ResetCount, &lastResetAtRaw, &expiresAtRaw, &allowedGroupsRaw, &limitsRaw); err != nil {
		return nil, err
	}
	createdAt, err := parseDBTimeValue(createdAtRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 API Key 创建时间失败: %w", err)
	}
	expiresAt, err := parseDBNullTimeValue(expiresAtRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 API Key 过期时间失败: %w", err)
	}
	lastResetAt, err := parseDBNullTimeValue(lastResetAtRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 API Key 重置时间失败: %w", err)
	}
	row.CreatedAt = createdAt
	row.ExpiresAt = expiresAt
	row.LastResetAt = lastResetAt
	row.AllowedGroupIDs = decodeInt64SliceValue(allowedGroupsRaw)
	row.Limits = decodeAPIKeyLimits(limitsRaw)
	return row, nil
}

// decodeAPIKeyLimits 把 SQL 取出来的 JSONB / TEXT 值解到 APIKeyLimits。
// 空值 / 非法 JSON 一律返回零值,避免 nil panic。
func decodeAPIKeyLimits(raw interface{}) APIKeyLimits {
	data := bytesFromDBValue(raw)
	if len(data) == 0 {
		return APIKeyLimits{}
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return APIKeyLimits{}
	}
	var out APIKeyLimits
	if err := json.Unmarshal(data, &out); err != nil {
		return APIKeyLimits{}
	}
	return out
}

func encodeAPIKeyLimits(l APIKeyLimits) string {
	if l.IsZero() {
		return "{}"
	}
	b, err := json.Marshal(l)
	if err != nil {
		return "{}"
	}
	return string(b)
}
