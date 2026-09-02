package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newCodexInviteRecipientTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "invite-recipients.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCodexInviteRecipientDDLVariants(t *testing.T) {
	postgres := strings.Join(codexInviteRecipientDDL(false), "\n")
	sqlite := strings.Join(codexInviteRecipientDDL(true), "\n")
	for name, ddl := range map[string]string{"postgres": postgres, "sqlite": sqlite} {
		for _, fragment := range []string{
			"CREATE TABLE IF NOT EXISTS codex_invite_recipients",
			"email_key TEXT PRIMARY KEY",
			"'reserved','sent','known_invited','unknown'",
			"idx_codex_invite_recipients_reservation",
		} {
			if !strings.Contains(ddl, fragment) {
				t.Fatalf("%s DDL missing %q: %s", name, fragment, ddl)
			}
		}
	}
	if !strings.Contains(postgres, "TIMESTAMPTZ") {
		t.Fatalf("postgres DDL must use TIMESTAMPTZ: %s", postgres)
	}
	if strings.Contains(sqlite, "TIMESTAMPTZ") || !strings.Contains(sqlite, "TIMESTAMP") {
		t.Fatalf("sqlite DDL must use portable TIMESTAMP: %s", sqlite)
	}
}

func TestReserveCodexInviteRecipientsNormalizesAndPersists(t *testing.T) {
	ctx := context.Background()
	db := newCodexInviteRecipientTestDB(t)

	reserved, err := db.ReserveCodexInviteRecipients(ctx, "reservation-a", 42, "program", "persistent", []string{
		" Alice@Example.COM ",
		"alice@example.com",
		"bob@example.com",
	})
	if err != nil {
		t.Fatalf("ReserveCodexInviteRecipients: %v", err)
	}
	if len(reserved) != 2 {
		t.Fatalf("reserved = %+v, want two unique rows", reserved)
	}
	for _, item := range reserved {
		if item.State != CodexInviteRecipientStateReserved || item.ReservationID != "reservation-a" {
			t.Fatalf("unexpected reservation row: %+v", item)
		}
	}

	got, err := db.ListCodexInviteRecipientsByEmails(ctx, []string{" ALICE@example.com ", "BOB@example.com", "missing@example.com"})
	if err != nil {
		t.Fatalf("ListCodexInviteRecipientsByEmails: %v", err)
	}
	if len(got) != 2 || got[0].EmailKey != "alice@example.com" || got[1].EmailKey != "bob@example.com" {
		t.Fatalf("normalized lookup = %+v", got)
	}
}

func TestReserveCodexInviteRecipientsConflictRollsBackWholeBatch(t *testing.T) {
	ctx := context.Background()
	db := newCodexInviteRecipientTestDB(t)
	if _, err := db.ReserveCodexInviteRecipients(ctx, "owner", 1, "p", "e", []string{"taken@example.com"}); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}

	_, err := db.ReserveCodexInviteRecipients(ctx, "loser", 2, "p", "e", []string{
		"new-before@example.com",
		" TAKEN@EXAMPLE.COM ",
		"new-after@example.com",
	})
	var conflict *CodexInviteRecipientConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want CodexInviteRecipientConflictError", err)
	}
	if len(conflict.Emails) != 1 || NormalizeCodexInviteRecipientEmail(conflict.Emails[0]) != "taken@example.com" {
		t.Fatalf("conflict emails = %v", conflict.Emails)
	}

	got, err := db.ListCodexInviteRecipientsByEmails(ctx, []string{"new-before@example.com", "new-after@example.com"})
	if err != nil {
		t.Fatalf("lookup rolled-back rows: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("conflicting batch committed partial rows: %+v", got)
	}
}

func TestReserveCodexInviteRecipientsConcurrentWinner(t *testing.T) {
	ctx := context.Background()
	db := newCodexInviteRecipientTestDB(t)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, reservationID := range []string{"r-one", "r-two"} {
		reservationID := reservationID
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.ReserveCodexInviteRecipients(ctx, reservationID, 1, "p", "e", []string{"race@example.com"})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	successes, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		var conflict *CodexInviteRecipientConflictError
		if errors.As(err, &conflict) {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent reserve error: %v", err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}
}

func TestCodexInviteRecipientReservationFenceFinalizeAndRelease(t *testing.T) {
	ctx := context.Background()
	db := newCodexInviteRecipientTestDB(t)
	if _, err := db.ReserveCodexInviteRecipients(ctx, "batch", 7, "program", "entry", []string{
		"known@example.com", "sent@example.com",
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if _, err := db.FinalizeCodexInviteRecipients(ctx, "wrong-owner", "req-wrong", 200, nil, time.Now()); !errors.Is(err, ErrCodexInviteRecipientReservationLost) {
		t.Fatalf("wrong-owner finalize error = %v, want reservation lost", err)
	}

	knownAt := time.Now().UTC().Add(-time.Minute)
	known, err := db.MarkCodexInviteRecipientsKnownInvited(ctx, "batch", "req-known", 403, []CodexInviteRecipientEvidence{{
		Email: "KNOWN@example.com", UpstreamRecipientStatus: "already_invited", InvitedAt: knownAt,
	}}, time.Now())
	if err != nil {
		t.Fatalf("mark known invited: %v", err)
	}
	if len(known) != 1 || known[0].State != CodexInviteRecipientStateKnownInvited {
		t.Fatalf("known rows = %+v", known)
	}

	released, err := db.ReleaseCodexInviteRecipients(ctx, "batch")
	if err != nil || released != 1 {
		t.Fatalf("release remaining reserved = %d, %v; want 1", released, err)
	}
	if _, err := db.ReserveCodexInviteRecipients(ctx, "sent-batch", 8, "program", "entry", []string{"sent@example.com"}); err != nil {
		t.Fatalf("released email was not reusable: %v", err)
	}

	finalized, err := db.FinalizeCodexInviteRecipients(ctx, "sent-batch", "req-ok", 200, []CodexInviteRecipientEvidence{{
		Email: "sent@example.com", ReferralID: "ref-1", InviteURL: "https://example.test/invite",
	}}, time.Now())
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(finalized) != 1 || finalized[0].State != CodexInviteRecipientStateSent || finalized[0].ReferralID != "ref-1" {
		t.Fatalf("finalized rows = %+v", finalized)
	}
	if released, err = db.ReleaseCodexInviteRecipients(ctx, "sent-batch"); err != nil || released != 0 {
		t.Fatalf("release must not delete sent row: %d, %v", released, err)
	}
	if _, err := db.ReserveCodexInviteRecipients(ctx, "duplicate", 9, "program", "entry", []string{"sent@example.com"}); err == nil {
		t.Fatal("sent email became reservable again")
	}
}

func TestCodexInviteRecipientUnknownBlocksReleaseAndRetry(t *testing.T) {
	ctx := context.Background()
	db := newCodexInviteRecipientTestDB(t)
	if _, err := db.ReserveCodexInviteRecipients(ctx, "uncertain", 5, "p", "e", []string{"unknown@example.com"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	affected, err := db.MarkCodexInviteRecipientsUnknown(ctx, "uncertain", "req-timeout", 0)
	if err != nil || affected != 1 {
		t.Fatalf("mark unknown = %d, %v", affected, err)
	}
	if released, err := db.ReleaseCodexInviteRecipients(ctx, "uncertain"); err != nil || released != 0 {
		t.Fatalf("unknown row was released: %d, %v", released, err)
	}
	if _, err := db.ReserveCodexInviteRecipients(ctx, "retry", 6, "p", "e", []string{" UNKNOWN@example.com "}); err == nil {
		t.Fatal("unknown email must conservatively block retry")
	}
	got, err := db.ListCodexInviteRecipientsByEmails(ctx, []string{"unknown@example.com"})
	if err != nil || len(got) != 1 || got[0].State != CodexInviteRecipientStateUnknown {
		t.Fatalf("unknown lookup = %+v, %v", got, err)
	}
}

func TestUpsertCodexInviteRecipientsFromTracking(t *testing.T) {
	ctx := context.Background()
	db := newCodexInviteRecipientTestDB(t)
	if _, err := db.ReserveCodexInviteRecipients(ctx, "unknown", 1, "p", "e", []string{"unknown@example.com"}); err != nil {
		t.Fatalf("reserve unknown: %v", err)
	}
	if _, err := db.MarkCodexInviteRecipientsUnknown(ctx, "unknown", "", 0); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if _, err := db.ReserveCodexInviteRecipients(ctx, "sent", 1, "p", "e", []string{"sent@example.com"}); err != nil {
		t.Fatalf("reserve sent: %v", err)
	}
	if _, err := db.FinalizeCodexInviteRecipients(ctx, "sent", "req-sent", 200, nil, time.Now()); err != nil {
		t.Fatalf("finalize sent: %v", err)
	}

	createdAt := time.Now().UTC().Add(-time.Hour)
	got, err := db.UpsertCodexInviteRecipientsFromTracking(ctx, 77, "tracked-program", "", 200, []CodexInviteRecipientEvidence{
		{Email: "unknown@example.com", ReferralID: "ref-u", UpstreamRecipientStatus: "redeemed", InvitedAt: createdAt},
		{Email: "fresh@example.com", ReferralID: "ref-f", InviteURL: "https://example.test/f", UpstreamRecipientStatus: "expired", InvitedAt: createdAt},
		{Email: "sent@example.com", ReferralID: "ref-s", UpstreamRecipientStatus: "redeemed", InvitedAt: createdAt},
	})
	if err != nil {
		t.Fatalf("tracking upsert: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("tracking rows = %+v", got)
	}
	byEmail := make(map[string]CodexInviteRecipient, len(got))
	for _, item := range got {
		byEmail[item.EmailKey] = item
	}
	if item := byEmail["unknown@example.com"]; item.State != CodexInviteRecipientStateKnownInvited || item.ReferralID != "ref-u" {
		t.Fatalf("unknown was not reconciled: %+v", item)
	}
	if item := byEmail["fresh@example.com"]; item.State != CodexInviteRecipientStateKnownInvited || item.SenderAccountID != 77 {
		t.Fatalf("fresh tracking row = %+v", item)
	}
	if item := byEmail["sent@example.com"]; item.State != CodexInviteRecipientStateSent || item.ReferralID != "ref-s" {
		t.Fatalf("sent row was downgraded or not enriched: %+v", item)
	}
}

func TestCodexInviteRecipientLedgerSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "durable-invite-recipients.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatalf("first database.New: %v", err)
	}
	if _, err := db.ReserveCodexInviteRecipients(ctx, "durable", 12, "p", "e", []string{"durable@example.com"}); err != nil {
		t.Fatalf("reserve durable: %v", err)
	}
	if _, err := db.FinalizeCodexInviteRecipients(ctx, "durable", "req", 200, nil, time.Now()); err != nil {
		t.Fatalf("finalize durable: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first DB: %v", err)
	}

	reopened, err := New("sqlite", path)
	if err != nil {
		t.Fatalf("reopen database.New: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.ListCodexInviteRecipientsByEmails(ctx, []string{"DURABLE@example.com"})
	if err != nil || len(got) != 1 || got[0].State != CodexInviteRecipientStateSent {
		t.Fatalf("durable row after reopen = %+v, %v", got, err)
	}
}
