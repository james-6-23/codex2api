import assert from "node:assert/strict";
import test from "node:test";

import {
  mergeConfirmedSettingsValues,
  mergeConfirmedSettingsWithVersions,
  resolveAutoSaveRollbackValues,
} from "./settingsAutoSave.ts";

test("auto-save rollback restores the last server-confirmed value instead of the typed draft", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
  };
  // blur 提交前按下的草稿值已经同步进 settingsFormRef，但服务器从未确认过它。
  const typedDraft = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
    site_name: "CodexProxy",
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(confirmed, typedDraft, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
  );
});

test("auto-save rollback only touches the failing request's fields", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
    max_concurrency: 16,
  };
  const submitted = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
    site_name: "Renamed",
    max_concurrency: 16,
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(confirmed, submitted, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
    "unrelated form edits must not leak into the rollback patch",
  );
});

test("auto-save rollback falls back to the submitted form before the first confirmed snapshot", () => {
  const submitted = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(null, submitted, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 3 * 1024 * 1024 },
  );
});

test("confirmed snapshot takes over only the acknowledged request's fields", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
  };

  const merged = mergeConfirmedSettingsValues(confirmed, {
    output_guard_max_channel_bytes: 4 * 1024 * 1024,
  });

  assert.deepEqual(merged, {
    output_guard_max_channel_bytes: 4 * 1024 * 1024,
    site_name: "CodexProxy",
  });
  assert.deepEqual(
    confirmed,
    {
      output_guard_max_channel_bytes: 2 * 1024 * 1024,
      site_name: "CodexProxy",
    },
    "the previous snapshot must not be mutated",
  );
  assert.equal(
    mergeConfirmedSettingsValues(null, {
      output_guard_max_channel_bytes: 4 * 1024 * 1024,
    }),
    null,
    "no snapshot is created before the initial load confirms one",
  );
});

test("confirmed settings merge with field versions ignores stale out-of-order responses", () => {
  const initial = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    output_guard_max_request_bytes: 4 * 1024 * 1024,
    site_name: "InitialSite",
  };
  let confirmedState = {
    confirmed: initial,
    confirmedVersions: {
      output_guard_max_channel_bytes: 1,
      output_guard_max_request_bytes: 1,
      site_name: 1,
    },
  };

  // 请求 2 先到达并确认（例如 channel_bytes 变为 8MB）
  confirmedState = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    { output_guard_max_channel_bytes: 8 * 1024 * 1024 },
    { output_guard_max_channel_bytes: 3 }, // version 3
  );

  assert.equal(
    confirmedState.confirmed?.output_guard_max_channel_bytes,
    8 * 1024 * 1024,
  );
  assert.equal(
    confirmedState.confirmedVersions.output_guard_max_channel_bytes,
    3,
  );

  // 请求 1（旧响应，例如 version 2，带旧值 4MB）后到达
  const afterStaleResponse = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    { output_guard_max_channel_bytes: 4 * 1024 * 1024 },
    { output_guard_max_channel_bytes: 2 }, // version 2 < 3
  );

  // 必须保持为新值 8MB，不能被旧响应 4MB 覆盖
  assert.equal(
    afterStaleResponse.confirmed?.output_guard_max_channel_bytes,
    8 * 1024 * 1024,
    "stale response must not overwrite newer confirmed baseline",
  );
  assert.equal(
    afterStaleResponse.confirmedVersions.output_guard_max_channel_bytes,
    3,
  );

  // 另一个字段（如 request_bytes，version 2 > 1）正常合并
  const multiFieldResponse = mergeConfirmedSettingsWithVersions(
    afterStaleResponse.confirmed,
    afterStaleResponse.confirmedVersions,
    {
      output_guard_max_channel_bytes: 4 * 1024 * 1024, // 旧的 version 2，仍被忽略
      output_guard_max_request_bytes: 16 * 1024 * 1024, // 新的 version 2 > 1，被接受
    },
    {
      output_guard_max_channel_bytes: 2,
      output_guard_max_request_bytes: 2,
    },
  );

  assert.equal(
    multiFieldResponse.confirmed?.output_guard_max_channel_bytes,
    8 * 1024 * 1024,
  );
  assert.equal(
    multiFieldResponse.confirmed?.output_guard_max_request_bytes,
    16 * 1024 * 1024,
  );
  assert.equal(
    multiFieldResponse.confirmedVersions.output_guard_max_request_bytes,
    2,
  );
});

test("confirmed settings merge with field versions handles null initial confirmed snapshot safely", () => {
  const result = mergeConfirmedSettingsWithVersions(
    null,
    {},
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
    { output_guard_max_channel_bytes: 1 },
  );

  assert.equal(result.confirmed, null);
  assert.equal(result.confirmedVersions.output_guard_max_channel_bytes, 1);
});

test("confirmed settings merge uses output_guard_config_generation to resolve out-of-order responses", () => {
  const initial = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    output_guard_config_generation: 1,
  };
  let confirmedState = {
    confirmed: initial,
    confirmedVersions: {
      output_guard_max_channel_bytes: 1,
    },
  };

  // 场景：两个 channel-bytes 保存重叠。
  // 请求 A (先发, client version 2) 后写，服务器提交后生成 version 2 (4MiB, generation 3)
  // 请求 B (后发, client version 3) 先写，服务器提交后生成 version 1 (8MiB, generation 2)
  // 客户端先收到请求 B 的响应 (8MiB, generation 2)
  confirmedState = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    {
      output_guard_max_channel_bytes: 8 * 1024 * 1024,
      output_guard_config_generation: 2,
    },
    { output_guard_max_channel_bytes: 3 }, // client requestedVersion 3
  );

  assert.equal(confirmedState.confirmed?.output_guard_max_channel_bytes, 8 * 1024 * 1024);
  assert.equal(confirmedState.confirmed?.output_guard_config_generation, 2);

  // 客户端后收到请求 A 的响应 (4MiB, generation 3, client requestedVersion 2 < 3)
  // 虽然 client version 2 < 3，但服务器端的 output_guard_config_generation 3 > 2，证明请求 A 是服务器最终提交值
  confirmedState = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    {
      output_guard_max_channel_bytes: 4 * 1024 * 1024,
      output_guard_config_generation: 3,
    },
    { output_guard_max_channel_bytes: 2 }, // client requestedVersion 2
  );

  // 客户端基线必须采纳服务器最终提交值 4MiB，而不是被请求顺序丢弃保留 8MiB
  assert.equal(
    confirmedState.confirmed?.output_guard_max_channel_bytes,
    4 * 1024 * 1024,
    "client baseline must equal server final committed value when server generation is newer",
  );
  assert.equal(confirmedState.confirmed?.output_guard_config_generation, 3);

  // 反向验证：如果再收到一个旧 generation 的响应（例如 generation 1），则必须丢弃
  const afterOlderGen = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    {
      output_guard_max_channel_bytes: 2 * 1024 * 1024,
      output_guard_config_generation: 1,
    },
    { output_guard_max_channel_bytes: 4 }, // client version 即使更大也不接受
  );

  assert.equal(afterOlderGen.confirmed?.output_guard_max_channel_bytes, 4 * 1024 * 1024);
  assert.equal(afterOlderGen.confirmed?.output_guard_config_generation, 3);
});


