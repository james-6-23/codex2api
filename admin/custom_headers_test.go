package admin

import (
	"encoding/json"
	"testing"
)

func TestParseOptionalCustomHeadersFieldNormalizesHeaders(t *testing.T) {
	raw := json.RawMessage(`{"authorization":"Bearer override","X-Custom-Header":"value"}`)

	parsed, err := parseOptionalCustomHeadersField(raw)
	if err != nil {
		t.Fatalf("parseOptionalCustomHeadersField() error = %v", err)
	}
	if !parsed.Set {
		t.Fatal("custom headers should be marked as set")
	}
	if got := parsed.Values["Authorization"]; got != "Bearer override" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := parsed.Values["X-Custom-Header"]; got != "value" {
		t.Fatalf("X-Custom-Header = %q", got)
	}
}

func TestParseOptionalCustomHeadersFieldRejectsInvalidValues(t *testing.T) {
	if _, err := parseOptionalCustomHeadersField(json.RawMessage(`{"Bad Header":"value"}`)); err == nil {
		t.Fatal("expected invalid header name error")
	}
	if _, err := parseOptionalCustomHeadersField(json.RawMessage(`{"X-Test":"line1\nline2"}`)); err == nil {
		t.Fatal("expected newline value error")
	}
}

func TestParseCustomHeadersFormNormalizesHeaders(t *testing.T) {
	headers, err := parseCustomHeadersForm(`{"authorization":"Bearer override"}`)
	if err != nil {
		t.Fatalf("parseCustomHeadersForm() error = %v", err)
	}
	if got := headers["Authorization"]; got != "Bearer override" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestParseCustomHeadersFormRejectsInvalidJSON(t *testing.T) {
	if _, err := parseCustomHeadersForm(`[]`); err == nil {
		t.Fatal("expected non-object custom_headers error")
	}
}
