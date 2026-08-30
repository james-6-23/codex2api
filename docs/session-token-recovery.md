# Codex OAuth recovery after account changes

ChatGPT can revoke an OAuth access/refresh-token family after a web-account
change such as a plan upgrade. Codex2API cannot keep that revoked credential
family valid. It can, however, recover without an interactive login when the
account was provisioned with an independent ChatGPT Web `session_token`.

When an official Codex OAuth account with a stored Web Session receives an
upstream 401, the request path now attempts one forced refresh through the Web
Session before cooling or rotating away from the account. The normal refresh
lock still serializes the operation, and the Web Session is never exposed in
API responses or logs.

## Requirements

- The account must be an official Codex OAuth account, not a relay, Grok, or
  Antigravity account.
- `session_token` must be an independent, still-valid ChatGPT Web Session. It
  must not be the OAuth refresh token and must belong to the same account.
- The session token must be imported before the account change. The existing
  credential import accepts `session_token`; keep it in the same account entry
  as the refresh token.

If the Web Session is missing, expired, challenged by Cloudflare, or belongs to
another account, the recovery attempt fails safely and the existing 401
cooldown behavior is retained. Interactive reauthorization is still required
in that case. This is a best-effort recovery path, not a guarantee that an
upstream credential revocation can be undone.
