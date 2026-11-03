#!/usr/bin/env bash
# scripts/run_acceptance_suite.sh
# 自动化执行 Codex2API 模型输出失控防护验收测试套件
set -euo pipefail

export GOCACHE="/tmp/go-build"
export GOPATH="/tmp/go"

TIMEOUT="25m"
LONG_RUN="${RUN_OUTPUTGUARD_LONG_RUN:-0}"

echo "========================================================================"
echo "  Codex2API Model Output Runaway Guard - Acceptance & Benchmark Suite"
echo "  Timeout: ${TIMEOUT} | -race: ENABLED | LongRunGate: ${LONG_RUN}"
echo "========================================================================"

echo "[1/3] Running Statistical Utilities Unit Tests (Task 11.1)..."
go test -v -race -timeout "${TIMEOUT}" ./proxy/perfstat

echo "[2/3] Running Benchmark, Scaling & Sanitization Tests (Spec §9.3 Scenarios 24-28)..."
RUN_OUTPUTGUARD_LONG_RUN="${LONG_RUN}" go test -v -race -timeout "${TIMEOUT}" ./proxy -run "TestAcceptanceScenario(24|25|26|27|28).*"

echo "[3/3] Checking Git Status and Binary Cleanliness..."
CHANGES=$(git status --porcelain)
if [[ -n "${CHANGES}" ]]; then
    echo "ERROR: Working tree is dirty after acceptance suite:" >&2
    echo "${CHANGES}" >&2
    exit 1
fi

echo "========================================================================"
echo "  Acceptance and Performance Suite Completed Successfully!"
echo "========================================================================"
