# Dispatch diagnostics for NewAPI

Responses, compact Responses, Chat Completions, and Responses WebSocket account-selection failures use two separate error surfaces:

- Clients receive `service_unavailable`, a generic message, and a request ID. HTTP status remains 503; an already-started stream uses its existing error-event format.
- A verified NewAPI binding receives an encrypted selection diagnostic. Ordinary API keys, unsigned identity headers, and unverified policy metadata do not authorize diagnostics.

This does not change root ownership, passive model exemptions, continuation routing, or the status contracts of existing quota, session-limit, policy, and upstream errors. Other provider-specific error paths keep their existing contracts.

## Observation semantics

Selection traces record rejected gates during actual selection and waiting. They reset between selection rounds and are frozen before the existing post-failure quota/capacity probes. Shadow scheduler probes are excluded. No second account-pool scan is performed to manufacture a cause after failure.

The diagnostic contains `stage`, `reason`, a bounded `reasons` list, an optional strict `root_account`, `retry`, and `incomplete`. It contains no credentials, proxy URLs, prompt text, fingerprints, or user-supplied explanations.

Examples of observed gates include account pause/disable/cooldown, credentials, API-key account scope, model/provider eligibility, model cooldown, affinity groups, egress, scope budgets, concurrency, and session capacity. A model/provider gate is deliberately broader than a model whitelist: it must not claim a whitelist mismatch when another check in that gate rejected the account. Passive exemptions are evaluated before attributing a model rejection.

`root_owner_unavailable` identifies an observed strict owner failure; it is not permission to move the request elsewhere. Multiple observed reasons produce `mixed_constraints`. Indexed misses and unresolved refresh/acquisition state explicitly mark the diagnosis incomplete instead of guessing a pool-wide cause.

Retry advice:

| Value | Meaning |
| --- | --- |
| `stop` | Do not repeat this dispatch failure automatically through another NewAPI channel. |
| `backoff_same_route` | A temporary constraint was observed. Do not use channel failover as a substitute for waiting/retrying the same route. |
| `default` | Diagnosis is incomplete; preserve existing retry rules. |

No new sleep loop, automatic account migration, or policy strike is introduced.

## Wire protocol v1

The envelope is `v1.` followed by unpadded base64url of `nonce || ciphertext || tag`.

- Key: HMAC-SHA256 of the existing verified binding secret over `codex2api:dispatch-diagnostic:v1`.
- Cipher: AES-256-GCM, random 12-byte nonce, 16-byte authentication tag.
- Authenticated data: the domain label, verified request ID, decimal user ID, and platform ID, joined by newline characters.
- Plaintext: a two-byte big-endian JSON byte length, the JSON payload, and zero padding to exactly 2048 bytes.
- Payload adds `request_id`, signed `channel_id`, `status: 503`, and `issued_at` to the selection diagnostic.
- Receiver validates authentication, request/channel binding, a 60-second maximum age with 10 seconds of future clock tolerance, enumerated fields, and size limits. The clocks on both services must be synchronized.

Transport carriers are deliberately separate from policy violations:

| Transport | Protected carrier |
| --- | --- |
| HTTP / failed WS handshake | `X-Codex2API-Dispatch-Diagnostic` |
| Already-started SSE | A `: codex2api_dispatch <envelope>` comment immediately before the terminal public error event |
| WS error frame | `error.details.codex2api_dispatch` |

NewAPI strips these carriers before forwarding, even when verification fails. SSE processing is bounded and preserves ordinary long data lines. Only a matching failure event authorizes a stream diagnostic; diagnostic-looking text inside normal output is not treated as metadata. The shared `dispatch_diagnostic_v1.txt` fixture checks cross-repository protocol compatibility.

## Deployment and retention

Deploy the receiving NewAPI change first, then codex2api. Both sides must have the same enabled, signed NewAPI binding, including signed policy metadata and channel ID. No public debug switch or new secret is required.

NewAPI stores validated details only in the administrator performance-error table. That table already enforces a 48-hour query window and periodic expiry cleanup; this feature does not change ordinary request-log retention. Performance metrics must be enabled for this storage path. When they are disabled or the binding cannot be verified, detailed diagnostics are not retained there.

codex2api console output contains only the correlation ID, status, and whether a protected diagnostic was produced. It does not print the detailed diagnostic or encrypted payload. NewAPI associates diagnostics with a specific dispatch attempt so delayed responses from a previous retry cannot explain a later failure.
