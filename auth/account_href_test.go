package auth

import "testing"

func TestNormalizeAccountHref(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty clears", input: "", want: ""},
		{name: "blank clears", input: "   ", want: ""},
		{name: "https url", input: "https://platform.openai.com/usage", want: "https://platform.openai.com/usage"},
		{name: "trailing slash trimmed", input: "https://platform.openai.com/", want: "https://platform.openai.com"},
		{name: "http url", input: "http://localhost:3000/usage", want: "http://localhost:3000/usage"},
		{name: "rejects javascript scheme", input: "javascript:alert(1)", wantErr: true},
		{name: "rejects bare host", input: "platform.openai.com/usage", wantErr: true},
		{name: "rejects scheme only", input: "https://", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeAccountHref(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeAccountHref(%q) want error, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAccountHref(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeAccountHref(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
