#!/usr/bin/env bash
# Print the `go test` selector args for one test-race shard so heavy packages
# do not share a 2-core runner. The admin package alone takes ~12min under
# -race on a 2-core runner (long tail of 1.5-5s tests, no single hot spot),
# which collided with the 12m go-test timeout — so it is further split in two
# by test-name initial (^Test[A-L] ≈ 45% of measured runtime, ^Test[^A-L] the
# rest; the two regexes are complementary, no test can be silently skipped).
# The `rest` shard is everything except the dedicated shards.
set -euo pipefail

shard="${1:-}"
case "$shard" in
  admin-a-l)
    echo "-run ^Test[A-L] ./admin"
    ;;
  admin-m-z)
    echo "-run ^Test[^A-L] ./admin"
    ;;
  database)
    echo ./database
    ;;
  proxy)
    echo ./proxy/...
    ;;
  promptfilter)
    echo ./security/promptfilter
    ;;
  rest)
    go list ./... | grep -Ev '/(admin|database)($|/)' | grep -Ev '/proxy($|/)' | grep -Ev '/security/promptfilter$'
    ;;
  *)
    echo "unknown test-race shard: ${shard}" >&2
    exit 1
    ;;
esac
