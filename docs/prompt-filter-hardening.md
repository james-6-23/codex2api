# Prompt filter hardening

`prompt_filter_strict_terminal_enabled` is an opt-in compatibility-safe switch. Its default is `false`.

When enabled, any enabled rule with `strict: true` becomes terminal: the request is blocked immediately, defensive-context discounts are not applied, and a secondary Moderations-compatible review cannot downgrade the verdict. This also applies to configured sensitive words, which are represented internally as strict rules.

Recommended production settings:

```json
{
  "prompt_filter_enabled": true,
  "prompt_filter_mode": "block",
  "prompt_filter_strict_terminal_enabled": true,
  "prompt_filter_review_enabled": true,
  "prompt_filter_review_fail_closed": true
}
```

Use `strict: true` only for patterns with very low false-positive risk. Keep contextual or dual-use phrases non-strict and let scoring plus optional review handle them.

## Nginx request buffering

Nginx cannot safely inspect arbitrary JSON prompts with ordinary `map` or location rules. Buffer the complete request and let Codex2API parse and inspect it before any upstream model request is opened. Use the example in `deploy/nginx/codex2api-prompt-buffering.conf.example`.

Do not enable `proxy_cache` for prompt endpoints. Prompt bodies may contain secrets and user content; caching or body logging creates a separate disclosure risk. If requests may spill to `client_body_temp_path`, place that directory on encrypted storage or tmpfs and keep `client_max_body_size` no larger than the configured in-memory buffer.

## Optional NeMo Guardrails sidecar

NeMo Guardrails or another classifier can be added later as an additional review service, but it should only escalate an allow/warn verdict. It must never override `terminal_strict_hit=true`. Keeping the deterministic local terminal path independent also avoids making protection depend on a network service being available.

## Advanced protection configuration

All advanced layers are disabled by default and are stored in `prompt_filter_advanced_config`:

```json
{
  "enforcement": {
    "terminal_categories": ["reverse_engineering"]
  },
  "normalization": {
    "enabled": true,
    "decode_url": true,
    "decode_html": true,
    "decode_base64": true,
    "max_decode_runs": 1
  },
  "risk": {
    "enabled": true,
    "window_seconds": 600,
    "block_threshold": 100,
    "review_threshold": 60,
    "user_weight_percent": 50,
    "ip_weight_percent": 30,
    "session_weight_percent": 20
  },
  "sidecar": {
    "enabled": false,
    "base_url": "http://127.0.0.1:8091",
    "timeout_seconds": 3,
    "fail_closed": true,
    "min_score": 30
  },
  "output": {
    "enabled": true,
    "buffer_bytes": 4096,
    "overlap_bytes": 512,
    "strict_only": true
  },
  "intelligence": {
    "enabled": false,
    "interval_hours": 24,
    "queries": ["LLM jailbreak prompt injection", "Codex prompt injection jailbreak"],
    "max_search_results": 20,
    "model_enabled": false,
    "model": "gpt-5.4",
    "max_model_calls": 1,
    "auto_add": false
  }
}
```

`enforcement.terminal_categories` is empty by default. Adding `reverse_engineering` makes every rule in that category terminal, including generic requests mentioning reverse engineering, IDA, Ghidra, Frida, disassembly, decompilation, or unpacking. Category-terminal matches cannot be discounted or downgraded by a review service.

Risk state uses the configured runtime cache. Redis is recommended for multiple Codex2API replicas; the memory cache works for a single process. User identifiers, IP addresses, and session headers are hashed before they become cache keys. The score decays across the configured window.

The optional sidecar receives `POST /v1/guard/check`. Set its bearer token through `PROMPT_FILTER_SIDECAR_API_KEY`; it is deliberately not stored in the admin JSON. A sidecar may escalate `allow` to `warn` or `block`, but cannot downgrade a local block, and terminal strict matches bypass it entirely.

SSE output goes through the shared stream writer. Responses WebSocket messages use a message-preserving output buffer so JSON frames are not merged. With `strict_only=true`, output is stopped only on terminal strict rules; setting it to false applies the normal blocking verdict.

Prompt intelligence is also disabled by default. When enabled, it searches recently updated public GitHub repositories using the configured queries. `model_enabled` permits at most `max_model_calls` bounded internal Responses calls through the existing account pool to turn public metadata into reviewed RE2-compatible rule candidates. Keep `auto_add=false` unless unattended additions are explicitly desired. Search, model analysis, and rule additions are written to the normal audit table with sources `intel_search`, `intel_model`, and `intel_rule_add`.

Composite custom rules retain compatibility with the original `pattern` field:

```json
{
  "name": "credential_exfiltration_chain",
  "weight": 100,
  "category": "credential_theft",
  "strict": true,
  "all_patterns": [
    "(?i)(steal|dump|extract|harvest)",
    "(?i)(cookie|credential|password|session\\s*token)"
  ],
  "any_patterns": [
    "(?i)chrome",
    "(?i)browser",
    "(?i)keychain"
  ],
  "min_matches": 1,
  "exclude_patterns": ["(?i)awareness training only"]
}
```
