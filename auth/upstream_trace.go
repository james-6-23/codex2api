package auth

import (
	"fmt"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const UpstreamRequestIDHeaderCredentialKey = "upstream_request_id_header"

func ValidateUpstreamRequestIDHeader(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > 64 || !httpguts.ValidHeaderFieldName(value) {
		return fmt.Errorf("upstream_request_id_header must be a valid HTTP header name of at most 64 bytes")
	}
	switch strings.ToLower(value) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key":
		return fmt.Errorf("upstream_request_id_header cannot name a credential header")
	}
	return nil
}

func (a *Account) GetUpstreamRequestIDHeader() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UpstreamRequestIDHeader
}

func (s *Store) ApplyAccountUpstreamRequestIDHeader(id int64, value string) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.UpstreamRequestIDHeader = strings.TrimSpace(value)
		a.mu.Unlock()
	}
}

type ProxyAuditLabel struct {
	ID   int64
	Name string
}

// ProxyAuditForURL returns a value snapshot. The URL is used for lookup only,
// and must never be included in a usage log (it can contain proxy credentials).
func (s *Store) ProxyAuditForURL(url string) ProxyAuditLabel {
	if strings.TrimSpace(url) == "" {
		return ProxyAuditLabel{Name: "direct/no_proxy"}
	}
	if s != nil {
		s.mu.RLock()
		label, ok := s.proxyAuditLabels[url]
		s.mu.RUnlock()
		if ok {
			return label
		}
	}
	return ProxyAuditLabel{Name: "unmanaged"}
}
