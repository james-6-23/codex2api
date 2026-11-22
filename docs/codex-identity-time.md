# UUIDv7 identity creation times

`CODEX_IDENTITY_V2_FROM_UNIX_MS` opts into source-time-preserving UUIDv7
mapping. It is a fixed Unix millisecond cutoff loaded at process startup;
empty or `0` retains the complete legacy behavior. Invalid values prevent
startup. `/health` exposes the effective value as
`codex_identity_v2_from_unix_ms`.

For `single_machine_multi_window`, source UUIDv7s created at or after the
cutoff retain their original 48-bit millisecond timestamp; the remaining
bits are derived in the existing account namespace. Session/thread and
parent/fork references share one mapping, while context windows retain their
separate namespace and their own creation time. Explicit session-derived
upstream cache identities preserve source time in their existing API-key
namespace as well. Account installation UUIDs, turn IDs/timestamps, caller
cache/continuation policy, and key-only fallbacks remain unchanged.

The approach follows `scoped_id` in codex-proxy-rs v3.48.0. This gateway keeps
the account record namespace instead of using installation ID alone: two
workspaces may intentionally share a pinned installation while retaining
independent conversation mappings. No extra dependency or per-session mapping
database is required.

## Activation

1. Deploy a version supporting this option with the option unset. Verify
   existing traffic and prepare a rollback version that also supports v2.
2. Choose one future UTC instant, convert it to Unix milliseconds, and persist
   it in the service environment. Configure every replica before that instant.
3. Keep this exact value across restarts, deployments, restores and failover.
   Existing pre-cutoff IDs retain byte-for-byte legacy mappings. A new child
   of an old parent maps each referenced ID by its own creation time.
4. Verify a new conversation and a pre-cutoff conversation over the real
   outbound HTTP/WS paths. Restart and verify identity stability and continued
   access with the same owner. API-key ownership checks still apply.

An old conversation can legitimately have an old creation time. Do not replace
that time with the current request time. Source timestamps classify IDs, not
the gateway's first observation: old IDs first submitted after activation
still use legacy mapping. Severe client clock skew can misclassify IDs.

Opaque strings, UUIDv4 and missing identities retain their existing behavior;
they have no reliable UUIDv7 creation time. Some fallback/cache IDs will
therefore still encode the historical fixed epoch. This release does not
promise to migrate all custom clients or make different accounts impossible
to correlate by shared time information.

## Rollback

After activation, never change/remove the cutoff or roll back to a binary
without v2 support: that would change already-used v2 IDs. Roll back to a
compatible build retaining the mapping and the same environment value.
Restoring application data does not remove the need to preserve this value.
Back up the environment together with the database and installation pins.

Before activation, the original image and unset cutoff remain a valid fallback.
The operator must finish staging both normal and rollback binaries before
choosing the production activation instant.
