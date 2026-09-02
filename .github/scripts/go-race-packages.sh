#!/usr/bin/env bash
# Print the `go test` selector args for one test-race shard so heavy packages
# do not share a 2-core runner. The admin and database packages can each take
# ~12min under -race on a 2-core runner, so both are split by test-name initial.
# Admin uses complementary run filters. Database uses one run filter plus one
# inverse skip filter so any future examples or fuzz seed tests stay covered.
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
  database-a-p)
    echo "-run ^Test[A-P] ./database"
    ;;
  database-other)
    echo "-skip ^Test[A-P] ./database"
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
