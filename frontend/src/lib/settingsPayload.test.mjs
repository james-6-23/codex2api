import assert from "node:assert/strict";
import test from "node:test";

import { buildWritableSettingsPayload } from "./settingsPayload.ts";
import {
  OUTPUT_GUARD_KEYS,
  omitOutputGuardFields,
} from "./outputGuardSettings.ts";

test("writable settings payload omits response cache generation regardless of value", () => {
  for (const generation of [7, 0, null, undefined]) {
    const settings = {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      response_cache_config_generation: generation,
      future_setting: "preserved",
      codex_images_main_model: "gpt-5.6-sol",
      codex_images_default_main_model: "gpt-5.6-luna",
      codex_egress: { mode: "resin", resin_enabled: true },
      codex_client_versions: [{ cli_version: "0.158.0-alpha.2.1" }],
    };

    const payload = buildWritableSettingsPayload(settings);

    assert.equal(
      Object.hasOwn(payload, "response_cache_config_generation"),
      false,
    );
    assert.deepEqual(payload, {
      site_name: "CodexProxy",
      response_cache_local_max_bytes: 64 * 1024 * 1024,
      response_cache_local_max_entry_bytes: 8 * 1024 * 1024,
      response_cache_reconstruct_max_bytes: 64 * 1024 * 1024,
      future_setting: "preserved",
      codex_images_main_model: "gpt-5.6-sol",
    });
    assert.equal(
      Object.hasOwn(settings, "response_cache_config_generation"),
      true,
      "the source settings object must not be mutated",
    );
    assert.equal(settings.response_cache_config_generation, generation);
  }
});

test("writable settings payload omits output guard generation regardless of value", () => {
  for (const generation of [42, 0, null, undefined]) {
    const settings = {
      site_name: "CodexProxy",
      output_guard_enabled: true,
      output_guard_max_tool_argument_bytes: 262144,
      output_guard_max_tool_argument_deltas: 4096,
      output_guard_max_channel_bytes: 2097152,
      output_guard_max_request_bytes: 4194304,
      output_guard_config_generation: generation,
    };

    const payload = buildWritableSettingsPayload(settings);

    assert.equal(
      Object.hasOwn(payload, "output_guard_config_generation"),
      false,
    );
    assert.equal(payload.output_guard_enabled, true);
    assert.equal(payload.output_guard_max_tool_argument_bytes, 262144);
    assert.equal(
      Object.hasOwn(settings, "output_guard_config_generation"),
      true,
      "the source settings object must not be mutated",
    );
    assert.equal(settings.output_guard_config_generation, generation);
  }
});

test("full-form save composition never writes guard settings", () => {
  const settings = {
    site_name: "CodexProxy",
    prompt_filter_custom_patterns: '[{"id":"r1"}]',
    output_guard_enabled: true,
    output_guard_max_tool_argument_bytes: 262144,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2097152,
    output_guard_max_request_bytes: 4194304,
    output_guard_config_generation: 23,
  };

  const payload = omitOutputGuardFields(buildWritableSettingsPayload(settings));

  for (const key of [...OUTPUT_GUARD_KEYS, "output_guard_config_generation"]) {
    assert.equal(
      Object.hasOwn(payload, key),
      false,
      `${key} must only be written by the guard auto-save path`,
    );
  }
  assert.deepEqual(payload, {
    site_name: "CodexProxy",
    prompt_filter_custom_patterns: '[{"id":"r1"}]',
  });
});
