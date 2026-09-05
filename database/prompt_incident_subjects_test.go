package database

import (
	"context"
	"testing"
)

func TestListPromptRiskSubjectsForIncident(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	db.mustExec(t, `INSERT INTO prompt_policy_incidents (incident_id, request_correlation_id) VALUES ('cy-9', 'corr-9')`)
	db.mustExec(t, `INSERT INTO prompt_risk_events (source_type, source_id, incident_id, subject_type, subject_key, subject_display, platform, is_person, identity_confidence, event_kind)
		VALUES ('prompt_policy_incident', 'cy-9', 'cy-9', 'newapi_user', 'hash-u1', '543924237@qq.com', 'buycodekey', 1, 90, 'upstream_cy'),
		       ('prompt_policy_incident', 'cy-9', 'cy-9', 'session', 'sess-1', 'session-1', 'buycodekey', 0, 0, 'upstream_cy'),
		       ('prompt_policy_incident', 'cy-9', 'cy-9', 'upstream_account', '239', 'acct@example.com', 'buycodekey', 0, 0, 'upstream_cy'),
		       ('prompt_policy_incident', 'other', 'other', 'newapi_user', 'hash-u2', 'someone', 'buycodekey', 1, 90, 'upstream_cy')`)
	db.mustExec(t, `INSERT INTO prompt_risk_identities (subject_type, subject_key, platform, external_user_id, user_name, user_email, user_group, source)
		VALUES ('newapi_user', 'hash-u1', 'buycodekey', '202', 'wtz', '543924237@qq.com', 'default', 'newapi')`)

	subjects, err := db.ListPromptRiskSubjectsForIncident(context.Background(), "cy-9")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subjects) != 3 {
		t.Fatalf("subjects = %+v, want 3", subjects)
	}
	if subjects[0].SubjectType != "newapi_user" || subjects[0].NewAPIUserID != "202" || subjects[0].NewAPIUserEmail != "543924237@qq.com" || !subjects[0].IsPerson {
		t.Fatalf("newapi_user subject should carry identity: %+v", subjects[0])
	}
	if subjects[1].SubjectType != "session" || subjects[2].SubjectType != "upstream_account" {
		t.Fatalf("subjects should be ordered person-first: %+v", subjects)
	}
	if empty, err := db.ListPromptRiskSubjectsForIncident(context.Background(), "missing"); err != nil || len(empty) != 0 {
		t.Fatalf("missing incident: %+v err=%v", empty, err)
	}
}
