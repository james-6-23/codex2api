# Follow synchronized CLI versions for a pinned account

Official Codex accounts may opt their custom `User-Agent` and `Version` headers
into the synchronized CLI version using `{{codex_cli_version}}`:

```json
{
  "User-Agent": "codex-tui/{{codex_cli_version}} (Windows 10.0.19045; x86_64) unknown (codex-tui; {{codex_cli_version}})",
  "Originator": "codex-tui",
  "Version": "{{codex_cli_version}}"
}
```

The account stores the template. HTTP requests, compact requests, new upstream
WebSocket handshakes, and maintenance requests expand it at send time from one
runtime version snapshot. Existing literal overrides keep their behavior.
Other headers and non-Codex relay accounts do not expand this token.

The selected version is the greater of the built-in CLI version and the valid
successfully synchronized CLI version. Failed synchronization retains the last
successful value. Updating the minimum CLI version is not required. Existing
WebSocket handshakes remain frozen for their connection lifetime.

Keep the operating system, architecture, terminal, originator, and installation
identity fixed when opting in. For Desktop or VS Code identities, use verified
application/CLI version pairing rather than replacing their CLI version with an
independent CLI release. This placeholder is intended for standalone CLI
identities such as `codex-tui`.

Focused validation:

```text
go test ./proxy ./proxy/wsrelay -run 'TestPinnedAccountVersion|TestOfficialWire|TestCodexClientIdentity|TestCodexMaintenance|TestWebsocketContinuationRejectsChangedClient|TestWebsocketClientPoolScopesFullVersionPair' -count=1
```
