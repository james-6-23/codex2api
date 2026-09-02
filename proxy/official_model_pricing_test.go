package proxy

import "testing"

func TestParseOpenAIOfficialPricingMarkdownIncludesFastAndLongContext(t *testing.T) {
	body := []byte(`
### Standard pricing data

| Model | Short context input | Short context cached input | Short context cache writes | Short context output | Long context input | Long context cached input | Long context cache writes | Long context output |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| gpt-5.6-sol | $5.00 | $0.50 | $6.25 | $30.00 | $10.00 | $1.00 | $12.50 | $45.00 |
| gpt-5.5 (<272K context length) | $5.00 | $0.50 | - | $30.00 | $10.00 | $1.00 | - | $45.00 |

### Fast pricing data

| Model | Short context input | Short context cached input | Short context cache writes | Short context output | Long context input | Long context cached input | Long context cache writes | Long context output |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| gpt-5.6-sol | $10.00 | $1.00 | $12.50 | $60.00 | $20.00 | $2.00 | $25.00 | $90.00 |
| gpt-5.5 (<272K context length) | $12.50 | $1.25 | - | $75.00 | - | - | - | - |
`)
	got, err := ParseOpenAIOfficialPricingMarkdown(body)
	if err != nil {
		t.Fatalf("parse official OpenAI pricing: %v", err)
	}
	sol := got["gpt-5.6-sol"]
	if sol.Input != 5 || sol.CachedInput != 0.5 || sol.Output != 30 {
		t.Fatalf("standard prices = %+v", sol)
	}
	if sol.InputPriority != 10 || sol.CachedInputPriority != 1 || sol.OutputPriority != 60 {
		t.Fatalf("fast prices = %+v", sol)
	}
	if sol.InputLong != 10 || sol.CachedInputLong != 1 || sol.OutputLong != 45 {
		t.Fatalf("long prices = %+v", sol)
	}
	if sol.InputLongPriority != 20 || sol.CachedInputLongPriority != 2 || sol.OutputLongPriority != 90 {
		t.Fatalf("long fast prices = %+v", sol)
	}
	if sol.LongContextThresholdTokens != 272000 {
		t.Fatalf("OpenAI long context threshold = %d, want 272000", sol.LongContextThresholdTokens)
	}
	if _, ok := got["gpt-5.5"]; !ok {
		t.Fatalf("model annotation should be stripped: %v", got)
	}
}

func TestParseXAIOfficialPricingMarkdown(t *testing.T) {
	body := []byte(`
### Text API Pricing

| Model | Context | Input / 1M tokens | Cached input / 1M tokens | Output / 1M tokens |
| --- | --- | --- | --- | --- |
| grok-4.6 (< 200k prompt tokens) | 500k | $2.00 | $0.50 | $6.00 |
| grok-4.6 (≥ 200k prompt tokens) | 500k | $4.00 | $1.00 | $12.00 |
| grok-4.5 (< 200k prompt tokens) | 500k | $2.00 | $0.30 | $6.00 |
| grok-4.5 (≥ 200k prompt tokens) | 500k | $4.00 | $0.60 | $12.00 |
`)
	all, err := ParseXAIOfficialPricingMarkdown(body)
	if err != nil {
		t.Fatalf("parse xAI official pricing: %v", err)
	}
	got := all["grok-4.6"]
	if got.Input != 2 || got.CachedInput != 0.5 || got.Output != 6 ||
		got.InputLong != 4 || got.CachedInputLong != 1 || got.OutputLong != 12 ||
		got.LongContextThresholdTokens != 200000 {
		t.Fatalf("xAI prices = %+v", got)
	}
}

func TestParseAnthropicOfficialPricingHTMLIncludesCacheWriteAndRead(t *testing.T) {
	body := []byte(`<table><tr><th>Model</th><th>Input</th><th>5m Cache Write</th><th>1h Cache Write</th><th>Cache Read</th><th>Output</th></tr>
<tr><td>Claude Fable 5.1</td><td>$10 / MTok</td><td>$12.50 / MTok</td><td>$20 / MTok</td><td>$0.25 / MTok</td><td>$50 / MTok</td></tr>
<tr><td>Claude Fable 5</td><td>$10 / MTok</td><td>$12.50 / MTok</td><td>$20 / MTok</td><td>$1 / MTok</td><td>$50 / MTok</td></tr></table>`)
	got, err := ParseAnthropicOfficialPricingHTML(body)
	if err != nil {
		t.Fatalf("ParseAnthropicOfficialPricingHTML: %v", err)
	}
	fable51 := got["claude-fable-5-1"]
	if fable51.Input != 10 || fable51.CachedInput != 0.25 || fable51.CacheWrite5m != 12.5 || fable51.CacheWrite1h != 20 || fable51.Output != 50 {
		t.Fatalf("Fable 5.1 pricing = %+v", fable51)
	}
	fable5 := got["claude-fable-5"]
	if fable5.Input != 10 || fable5.CachedInput != 1 || fable5.CacheWrite5m != 12.5 || fable5.CacheWrite1h != 20 || fable5.Output != 50 {
		t.Fatalf("Fable 5 pricing = %+v", fable5)
	}
}
