package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageSearchScopeValidation(test *testing.T) {
	for _, scope := range []string{"", "all", "session", "request", "error", "model", "endpoint", "account", "newapi_user", "ip", "user_agent", "api_key", "unknown", "error,model", "' OR 1=1 --"} {
		test.Run(scope, func(test *testing.T) {
			recorder := httptest.NewRecorder()
			request, _ := gin.CreateTestContext(recorder)
			request.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?q=test&search_scope="+url.QueryEscape(scope), nil)
			filter, valid := parseUsageLogsFilter(request, time.Now().Add(-time.Hour), time.Now())
			want := scope != "unknown" && scope != "error,model" && scope != "' OR 1=1 --"
			require.Equal(test, want, valid)
			if want {
				require.Equal(test, scope, filter.SearchScope)
			} else {
				require.Equal(test, http.StatusBadRequest, recorder.Code)
			}
		})
	}
}
