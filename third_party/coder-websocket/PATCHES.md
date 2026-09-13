# Local transport compatibility patches

Source: github.com/coder/websocket v1.8.15. Original license and upstream README
are retained. Production source is copied unchanged except the changes listed
below; regression tests live in the application's proxy/wsrelay package.

- Keep the reader lock until a fragmented compressed read unwinds on peer Close.
  Reply and cleanup run outside that stack so the inflater and dictionary cannot
  be returned to their pools while still in use.
- Expose send-only Ping, separating control-frame write failure from missing Pong.
- Bound outgoing data frames to 16 KiB by default so control frames can interleave
  with large messages. DialOptions.DisableFragmentation allows single-frame Write
  messages. Compression still uses the negotiated cross-message dictionary.
- Use a 30-second control-write budget, matching the application's data-write
  budget; the read-side Ping callback does not inherit the control-read timeout.
- Offer `client_max_window_bits` and accept negotiated values 9..15 (reject 8). Smaller
  windows use klauspost/compress v1.17.6 NewWriterWindow; 15 or omission keeps
  the original standard-library encoder. DialOptions.CompressionLevel selects
  levels 1..9 for the 32 KiB encoder (default 1); smaller windows use the custom
  encoder's fixed strategy. Writers are pooled by window size and effective level.
  Context takeover and independent-message negotiation remain separate.

This is a pinned source dependency, not a module-cache patch. Builds on other
machines use the same code through the root go.mod replace directive. Re-evaluate
these patches when upgrading upstream; do not remove them without running the
ordinary fragmentation/Close, dictionary reuse, slow-write/control-frame and
continuation regression tests.
