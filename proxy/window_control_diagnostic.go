package proxy

import (
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const windowControlDiagnosticContextKey = "window_control_diagnostic"

func windowControlDiagnostic(request *gin.Context) *database.WindowControlDiagnostic {
	value, _ := request.Get(windowControlDiagnosticContextKey)
	diagnostic, _ := value.(*database.WindowControlDiagnostic)
	return diagnostic
}

func (handler *Handler) beginWindowControlDiagnostic(request *gin.Context, input windowControlRequest, identity verifiedNewAPIPolicyContext, subject string, now time.Time, limit, seconds, active int) *database.WindowControlDiagnostic {
	diagnostic := &database.WindowControlDiagnostic{
		ObservedAt: now, AllowExpansion: input.AllowExpansion, ExtraLimit: input.ExtraLimit, Multiplier: input.Multiplier,
		UserLimit: limit, WindowSeconds: seconds, ActiveWindows: active, RootWindowState: "unresolved",
	}
	if identity.Meta.RootSessionState == newAPIPolicyRootSessionResolved && identity.Meta.RootSessionFingerprint != "" {
		diagnostic.RootHash = hashRiskIdentity(identity.Meta.RootSessionFingerprint)
		diagnostic.RootWindowState = "missing"
		handler.promptSessionLimitMu.Lock()
		expiry, found := handler.promptSessionLimits[subject][diagnostic.RootHash]
		handler.promptSessionLimitMu.Unlock()
		if found {
			diagnostic.RootWindowExpiresAt = expiry
			diagnostic.RootWindowState = "active"
			if !expiry.After(now) {
				diagnostic.RootWindowState = "expired"
			}
		}
	}
	request.Set(windowControlDiagnosticContextKey, diagnostic)
	return diagnostic
}

func observeWindowGrant(grant *database.UserWindowGrant, now time.Time) *database.WindowGrantDiagnostic {
	result := &database.WindowGrantDiagnostic{State: "missing"}
	if grant == nil {
		return result
	}
	result.Confirmed, result.Expanded = grant.Confirmed, grant.Expanded
	result.CreatedAt, result.ExpiresAt, result.PendingUntil = grant.CreatedAt, grant.ExpiresAt, grant.PendingUntil
	result.OwnerAccountID = grant.OwnerAccountID
	result.State = "active"
	if !grant.ExpiresAt.After(now) {
		result.State = "expired"
	} else if !grant.Confirmed {
		result.State = "pending"
		if !grant.PendingUntil.After(now) {
			result.State = "reservation_expired"
		}
	}
	return result
}
