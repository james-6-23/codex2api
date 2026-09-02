package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCollectInviteEmails(t *testing.T) {
	t.Run("dedup and trim from list + text", func(t *testing.T) {
		got, err := collectInviteEmails(
			[]string{"A@example.com", " b@example.com "},
			"a@example.com\nc@example.com, d@example.com",
			10,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// A@ 与 a@ 视为重复（忽略大小写），保留首次出现的大小写形式。
		want := []string{"A@example.com", "b@example.com", "c@example.com", "d@example.com"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d]=%q, want %q (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("rejects invalid email", func(t *testing.T) {
		if _, err := collectInviteEmails([]string{"not-an-email"}, "", 10); err == nil {
			t.Fatal("expected error for invalid email")
		}
	})

	t.Run("empty input errors", func(t *testing.T) {
		if _, err := collectInviteEmails(nil, "  ", 10); err == nil {
			t.Fatal("expected error for empty input")
		}
	})

	t.Run("exceeds cap", func(t *testing.T) {
		if _, err := collectInviteEmails([]string{"a@x.com", "b@x.com", "c@x.com"}, "", 2); err == nil {
			t.Fatal("expected error when exceeding cap")
		}
	})
}

func newInviteCacheTestHandler(t *testing.T, withCache bool) *Handler {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "invite-cache.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db}
	if withCache {
		h.cache = cache.NewMemory(1)
		t.Cleanup(func() { _ = h.cache.Close() })
	}
	return h
}

type inviteCacheProbe struct {
	Value string `json:"value"`
}

func TestInviteCacheReadWrite(t *testing.T) {
	ctx := context.Background()
	const scope = "prog|persistent"

	t.Run("snapshot path serves after restart-equivalent cold cache", func(t *testing.T) {
		// cache=nil 等价于「进程重启、运行态缓存全空」：数据必须还能从库里读回来。
		h := newInviteCacheTestHandler(t, false)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 3, scope, 200, time.Minute, inviteCacheProbe{Value: "from-upstream"})

		var got inviteCacheProbe
		meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 3, scope, &got)
		if meta == nil {
			t.Fatal("expected snapshot hit, got miss")
		}
		if meta.Source != "snapshot" {
			t.Fatalf("expected source=snapshot, got %q", meta.Source)
		}
		if got.Value != "from-upstream" {
			t.Fatalf("payload mismatch: %+v", got)
		}
	})

	t.Run("runtime cache short-circuits the database", func(t *testing.T) {
		h := newInviteCacheTestHandler(t, true)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 3, scope, 200, time.Minute, inviteCacheProbe{Value: "hot"})

		var got inviteCacheProbe
		meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 3, scope, &got)
		if meta == nil || meta.Source != "runtime" {
			t.Fatalf("expected source=runtime, got %+v", meta)
		}
		if got.Value != "hot" {
			t.Fatalf("payload mismatch: %+v", got)
		}
	})

	t.Run("credential generation change misses", func(t *testing.T) {
		// 重新授权后资格属于另一份身份，旧快照不能继续端出来。
		h := newInviteCacheTestHandler(t, true)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 3, scope, 200, time.Minute, inviteCacheProbe{Value: "old-identity"})

		var got inviteCacheProbe
		if meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 4, scope, &got); meta != nil {
			t.Fatalf("expected miss after generation bump, got %+v (%+v)", meta, got)
		}
	})

	t.Run("expired snapshot misses", func(t *testing.T) {
		h := newInviteCacheTestHandler(t, false)
		h.writeInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			42, 1, scope, 200, -time.Minute, inviteCacheProbe{Value: "stale"})

		var got inviteCacheProbe
		if meta := h.readInviteCache(ctx, inviteTrackingCacheNamespace,
			database.CodexInviteSnapshotTracking, 42, 1, scope, &got); meta != nil {
			t.Fatalf("expected miss for expired snapshot, got %+v", meta)
		}
	})

	t.Run("invalidate clears both layers", func(t *testing.T) {
		h := newInviteCacheTestHandler(t, true)
		programID, entrypoint := proxy.NormalizeInviteProgram("", "")
		eligScope := inviteEligibilityScope(programID, entrypoint)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 1, eligScope, 200, time.Minute, inviteCacheProbe{Value: "pre-send"})

		h.invalidateInviteCache(ctx, 42, 1, programID, entrypoint)

		var got inviteCacheProbe
		if meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 1, eligScope, &got); meta != nil {
			t.Fatalf("expected miss after invalidation, got %+v (%+v)", meta, got)
		}
	})
}

// 归一化后的参数才进作用域，否则「不传」与「传默认值」会各存一份相同内容。
func TestInviteCacheScopeNormalization(t *testing.T) {
	programA, entryA := proxy.NormalizeInviteProgram("", "")
	programB, entryB := proxy.NormalizeInviteProgram(proxy.DefaultProgramID, proxy.DefaultEntrypoint)
	if inviteEligibilityScope(programA, entryA) != inviteEligibilityScope(programB, entryB) {
		t.Fatal("empty and explicit-default program params must share one scope")
	}

	periodA, limitA := proxy.NormalizeInviteTracking("", 0)
	periodB, limitB := proxy.NormalizeInviteTracking(periodA, limitA)
	if inviteTrackingScope(programA, periodA, limitA) != inviteTrackingScope(programB, periodB, limitB) {
		t.Fatal("tracking defaults must normalize to one scope")
	}
	if inviteTrackingScope(programA, periodA, 10) == inviteTrackingScope(programA, periodA, limitA) {
		t.Fatal("different limits must not share a scope")
	}
}

func newInviteRecipientTestHandler(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "invite-recipients.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{
		DBID:        7,
		AccessToken: "test-access-token",
		AccountID:   "workspace-7",
		Email:       "sender@example.com",
	})
	return &Handler{db: db, store: store}, db
}

func invokeInviteJSON(t *testing.T, method, target string, body any, params gin.Params, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, target, bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	handler(c)
	return recorder
}

func TestSendInvitePersistsSuccessAndRejectsDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newInviteRecipientTestHandler(t)
	calls := 0
	h.sendCodexInvite = func(_ context.Context, _ *auth.Account, _, programID, entrypoint string, emails []string) (*proxy.CodexInviteResult, error) {
		calls++
		return &proxy.CodexInviteResult{
			OK: true, StatusCode: http.StatusOK, RequestID: "req-success",
			ProgramID: programID, Entrypoint: entrypoint, Emails: emails,
		}, nil
	}

	requestBody := map[string]any{"emails": []string{" Recipient@Example.com "}}
	first := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", requestBody,
		gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var response struct {
		OK             bool     `json:"ok"`
		RecordedEmails []string `json:"recorded_emails"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if !response.OK || len(response.RecordedEmails) != 1 {
		t.Fatalf("unexpected first response: %+v", response)
	}
	recipients, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"recipient@example.com"})
	if err != nil || len(recipients) != 1 {
		t.Fatalf("read ledger: %v %+v", err, recipients)
	}
	if recipients[0].State != database.CodexInviteRecipientStateSent || recipients[0].RequestID != "req-success" {
		t.Fatalf("recipient not finalized: %+v", recipients[0])
	}

	second := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", requestBody,
		gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
	if second.Code != http.StatusConflict {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream called %d times, want once", calls)
	}
}

func TestSendInviteReleasesDefinitiveFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newInviteRecipientTestHandler(t)
	calls := 0
	h.sendCodexInvite = func(_ context.Context, _ *auth.Account, _, programID, entrypoint string, emails []string) (*proxy.CodexInviteResult, error) {
		calls++
		return &proxy.CodexInviteResult{
			OK: false, StatusCode: http.StatusForbidden,
			ProgramID: programID, Entrypoint: entrypoint, Emails: emails,
			UpstreamMessage: "account is not eligible",
		}, nil
	}
	body := map[string]any{"emails": []string{"retry@example.com"}}
	for i := 0; i < 2; i++ {
		response := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", body,
			gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("upstream called %d times, want retry to be allowed", calls)
	}
	items, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"retry@example.com"})
	if err != nil || len(items) != 0 {
		t.Fatalf("definitive failures must release reservation: %v %+v", err, items)
	}
}

func TestSendInviteChallengeReleasesReservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newInviteRecipientTestHandler(t)
	calls := 0
	h.sendCodexInvite = func(_ context.Context, _ *auth.Account, _, programID, entrypoint string, emails []string) (*proxy.CodexInviteResult, error) {
		calls++
		return &proxy.CodexInviteResult{
			OK: false, StatusCode: http.StatusForbidden, Challenged: true,
			ProgramID: programID, Entrypoint: entrypoint, Emails: emails,
		}, nil
	}
	body := map[string]any{"emails": []string{"challenge@example.com"}}
	for i := 0; i < 2; i++ {
		response := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", body,
			gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("challenge should allow retry, upstream calls=%d", calls)
	}
	items, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"challenge@example.com"})
	if err != nil || len(items) != 0 {
		t.Fatalf("challenge reservation not released: %v %+v", err, items)
	}
}

func TestSendInviteKeepsAmbiguousTransportFailureBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newInviteRecipientTestHandler(t)
	calls := 0
	h.sendCodexInvite = func(context.Context, *auth.Account, string, string, string, []string) (*proxy.CodexInviteResult, error) {
		calls++
		return nil, errors.New("connection reset after write")
	}
	body := map[string]any{"emails": []string{"uncertain@example.com"}}
	first := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", body,
		gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	items, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"UNCERTAIN@example.com"})
	if err != nil || len(items) != 1 || items[0].State != database.CodexInviteRecipientStateUnknown {
		t.Fatalf("ambiguous result not blocked: %v %+v", err, items)
	}
	second := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", body,
		gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
	if second.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("second status=%d calls=%d body=%s", second.Code, calls, second.Body.String())
	}
}

func TestSendInviteRecordsRecipientAlreadyInvited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newInviteRecipientTestHandler(t)
	h.sendCodexInvite = func(_ context.Context, _ *auth.Account, _, programID, entrypoint string, emails []string) (*proxy.CodexInviteResult, error) {
		return &proxy.CodexInviteResult{
			OK: false, StatusCode: http.StatusForbidden, RequestID: "req-existing",
			ProgramID: programID, Entrypoint: entrypoint, Emails: emails,
			UpstreamMessage: "此人已收到推荐邀请", FailedEmails: []string{"existing@example.com"},
		}, nil
	}
	body := map[string]any{"emails": []string{"existing@example.com", "not-sent@example.com"}}
	response := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/7/invite", body,
		gin.Params{{Key: "id", Value: "7"}}, h.SendInvite)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	items, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"existing@example.com"})
	if err != nil || len(items) != 1 || items[0].State != database.CodexInviteRecipientStateKnownInvited {
		t.Fatalf("known upstream invite not retained: %v %+v", err, items)
	}
	released, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"not-sent@example.com"})
	if err != nil || len(released) != 0 {
		t.Fatalf("non-failed address from rejected batch should be released: %v %+v", err, released)
	}
}

func TestCheckInviteRecipientsReturnsOnlyRecordedEmails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, db := newInviteRecipientTestHandler(t)
	_, err := db.UpsertCodexInviteRecipientsFromTracking(context.Background(), 7,
		proxy.DefaultProgramID, proxy.DefaultEntrypoint, http.StatusOK,
		[]database.CodexInviteRecipientEvidence{{Email: "tracked@example.com", InvitedAt: time.Now()}})
	if err != nil {
		t.Fatalf("seed tracking recipient: %v", err)
	}

	response := invokeInviteJSON(t, http.MethodPost, "/api/admin/accounts/invite/recipients/check",
		map[string]any{"emails": []string{"TRACKED@example.com", "fresh@example.com"}}, nil,
		h.CheckInviteRecipients)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded struct {
		Recipients []database.CodexInviteRecipient `json:"recipients"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Recipients) != 1 || decoded.Recipients[0].Email != "tracked@example.com" {
		t.Fatalf("unexpected recipients: %+v", decoded.Recipients)
	}
}

func TestRememberInviteTrackingRecipientsBackfillsLedger(t *testing.T) {
	h, db := newInviteRecipientTestHandler(t)
	h.rememberInviteTrackingRecipients(context.Background(), 7, proxy.DefaultProgramID,
		proxy.DefaultEntrypoint, &proxy.CodexInviteTracking{
			OK: true, StatusCode: http.StatusOK,
			Items: []proxy.CodexInviteTrackingItem{{
				Email: "history@example.com", Status: "redeemed",
				CreatedAt: "2026-08-03T05:24:58.842913Z",
			}},
		})
	items, err := db.ListCodexInviteRecipientsByEmails(context.Background(), []string{"history@example.com"})
	if err != nil || len(items) != 1 {
		t.Fatalf("read backfill: %v %+v", err, items)
	}
	if items[0].State != database.CodexInviteRecipientStateKnownInvited || items[0].UpstreamRecipientStatus != "redeemed" {
		t.Fatalf("unexpected backfill: %+v", items[0])
	}
}
