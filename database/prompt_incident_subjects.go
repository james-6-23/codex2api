package database

import (
	"context"
	"strings"
)

// PromptRiskIncidentSubject 是一条 CY 记录关联到的风险画像主体（newapi_user / session /
// api_key / client_ip / upstream_account），附带已核实的 NewAPI 身份信息，供 CY 详情页
// 直接跳到对应画像。
type PromptRiskIncidentSubject struct {
	SubjectType        string `json:"subject_type"`
	SubjectKey         string `json:"subject_key"`
	SubjectDisplay     string `json:"subject_display"`
	Platform           string `json:"platform,omitempty"`
	IsPerson           bool   `json:"is_person"`
	IdentityConfidence int    `json:"identity_confidence"`
	NewAPIUserID       string `json:"newapi_user_id,omitempty"`
	NewAPIUserName     string `json:"newapi_user_name,omitempty"`
	NewAPIUserEmail    string `json:"newapi_user_email,omitempty"`
	NewAPIUserGroup    string `json:"newapi_user_group,omitempty"`
	EventCount         int    `json:"event_count"`
}

// ListPromptRiskSubjectsForIncident 返回挂在该 CY 上的全部画像主体（按主体去重），
// 并用 prompt_risk_identities 补齐 NewAPI 用户 ID / 名称 / 邮箱 / 分组。
func (db *DB) ListPromptRiskSubjectsForIncident(ctx context.Context, incidentID string) ([]PromptRiskIncidentSubject, error) {
	incidentID = strings.TrimSpace(incidentID)
	if db == nil || db.conn == nil || incidentID == "" {
		return []PromptRiskIncidentSubject{}, nil
	}
	if err := db.ensurePromptRiskEventsTable(ctx); err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT e.subject_type, e.subject_key, MAX(COALESCE(e.subject_display, '')), MAX(COALESCE(e.platform, '')),
		       MAX(CASE WHEN e.is_person THEN 1 ELSE 0 END), MAX(COALESCE(e.identity_confidence, 0)), COUNT(*),
		       COALESCE(MAX(i.external_user_id), ''), COALESCE(MAX(i.user_name), ''), COALESCE(MAX(i.user_email), ''), COALESCE(MAX(i.user_group), '')
		FROM prompt_risk_events e
		LEFT JOIN prompt_risk_identities i ON i.subject_type = e.subject_type AND i.subject_key = e.subject_key
		WHERE e.incident_id = $1
		GROUP BY e.subject_type, e.subject_key
		ORDER BY CASE e.subject_type
			WHEN 'newapi_user' THEN 0 WHEN 'session' THEN 1 WHEN 'api_key' THEN 2 WHEN 'client_ip' THEN 3 ELSE 4 END, e.subject_key`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	subjects := make([]PromptRiskIncidentSubject, 0, 5)
	for rows.Next() {
		var s PromptRiskIncidentSubject
		var isPerson int
		if err := rows.Scan(&s.SubjectType, &s.SubjectKey, &s.SubjectDisplay, &s.Platform, &isPerson, &s.IdentityConfidence, &s.EventCount,
			&s.NewAPIUserID, &s.NewAPIUserName, &s.NewAPIUserEmail, &s.NewAPIUserGroup); err != nil {
			return nil, err
		}
		s.IsPerson = isPerson == 1
		subjects = append(subjects, s)
	}
	return subjects, rows.Err()
}
