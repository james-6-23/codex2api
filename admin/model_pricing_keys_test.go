package admin

import (
	"reflect"
	"testing"
)

func TestModelPricingManagementKeysKeepsIndependentAlias(t *testing.T) {
	got := modelPricingManagementKeys([]string{
		"gpt-5.4",
		"codex-auto-review",
		"gpt-5.4-openai-compact",
	})
	want := []string{"gpt-5.4", "codex-auto-review"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelPricingManagementKeys() = %v, want %v", got, want)
	}
}
