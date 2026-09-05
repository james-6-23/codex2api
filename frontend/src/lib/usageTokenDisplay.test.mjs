import assert from "node:assert/strict";
import test from "node:test";

import { getUsageTokenBreakdown } from "./usageTokenDisplay.ts";

test("Claude screenshot rows show uncached input separately from both cache buckets", () => {
  const rows = [
    [46_601, 46_507, 0, 75, 94],
    [46_601, 46_334, 173, 65, 94],
    [46_336, 46_097, 237, 173, 2],
    [46_099, 45_786, 311, 161, 2],
    [45_788, 45_341, 445, 215, 2],
    [45_343, 44_681, 660, 405, 2],
  ];

  for (const [input, read, write, output, uncached] of rows) {
    const result = getUsageTokenBreakdown({
      channel: "claude",
      input_tokens: input,
      output_tokens: output,
      cached_tokens: read,
      cache_write_5m_tokens: write,
    });
    assert.equal(result.inputTokens, uncached);
    assert.equal(result.totalInputTokens, input);
    assert.equal(result.cacheReadTokens, read);
    assert.equal(result.cacheWriteTokens, write);
    assert.equal(result.outputTokens, output);
  }
});

test("Claude cache creation combines 5m and 1h while retaining their breakdown", () => {
  assert.deepEqual(
    getUsageTokenBreakdown({
      channel: " CLAUDE ",
      input_tokens: 1_000,
      output_tokens: 25,
      cached_tokens: 700,
      cache_write_5m_tokens: 100,
      cache_write_1h_tokens: 150,
    }),
    {
      isClaude: true,
      totalInputTokens: 1_000,
      inputTokens: 50,
      outputTokens: 25,
      cacheReadTokens: 700,
      cacheWriteTokens: 250,
      cacheWrite5mTokens: 100,
      cacheWrite1hTokens: 150,
    },
  );
});

test("fully cached Claude input displays zero and inconsistent cache totals never display negative input", () => {
  for (const input of [1_000, 999]) {
    const result = getUsageTokenBreakdown({
      channel: "claude",
      input_tokens: input,
      output_tokens: 12,
      cached_tokens: 700,
      cache_write_5m_tokens: 100,
      cache_write_1h_tokens: 200,
    });
    assert.equal(result.inputTokens, 0);
    assert.equal(result.totalInputTokens, input);
  }
});

test("other channels keep total input even when a model name contains Claude", () => {
  for (const channel of ["codex", "openai", "grok", "antigravity", "claude-api"]) {
    const result = getUsageTokenBreakdown({
      channel,
      model: "claude-sonnet-4-6",
      input_tokens: 1_000,
      output_tokens: 25,
      cached_tokens: 700,
      cache_write_5m_tokens: 100,
    });
    assert.equal(result.isClaude, false);
    assert.equal(result.inputTokens, 1_000);
    assert.equal(result.cacheReadTokens, 700);
    assert.equal(result.cacheWriteTokens, 100);
  }
});

test("missing and empty channels never infer Claude from the model", () => {
  for (const channel of [undefined, "", " ", null]) {
    const result = getUsageTokenBreakdown({
      channel,
      model: "claude-opus-4-6",
      input_tokens: 1_000,
      output_tokens: 25,
      cached_tokens: 700,
      cache_write_1h_tokens: 100,
    });
    assert.equal(result.isClaude, false);
    assert.equal(result.inputTokens, 1_000);
  }
});

test("missing, negative, and non-finite token values normalize to zero without coercion", () => {
  for (const invalid of [undefined, null, -1, -Infinity, Infinity, NaN, "42"]) {
    assert.deepEqual(
      getUsageTokenBreakdown({
        channel: "claude",
        input_tokens: invalid,
        output_tokens: invalid,
        cached_tokens: invalid,
        cache_write_5m_tokens: invalid,
        cache_write_1h_tokens: invalid,
      }),
      {
        isClaude: true,
        totalInputTokens: 0,
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
        cacheWrite5mTokens: 0,
        cacheWrite1hTokens: 0,
      },
    );
  }

  assert.equal(
    getUsageTokenBreakdown({
      channel: "claude",
      input_tokens: 100,
      output_tokens: 5,
      cached_tokens: -20,
      cache_write_5m_tokens: NaN,
      cache_write_1h_tokens: Infinity,
    }).inputTokens,
    100,
  );
});

test("display conversion never mutates the source usage log", () => {
  const log = Object.freeze({
    channel: " CLAUDE ",
    input_tokens: 46_601,
    output_tokens: 65,
    cached_tokens: 46_334,
    cache_write_5m_tokens: 173,
    cache_write_1h_tokens: 0,
  });
  const original = { ...log };
  const first = getUsageTokenBreakdown(log);
  const second = getUsageTokenBreakdown(log);
  assert.deepEqual(log, original);
  assert.deepEqual(first, second);
  assert.notEqual(first, second);
  assert.equal(first.inputTokens, 94);
});
