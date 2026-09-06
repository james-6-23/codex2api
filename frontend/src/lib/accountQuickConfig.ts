import type {
  CodexFingerprintMode,
  UpdateAccountSchedulerRequest,
} from "../types";

export type QuickConfigLoadStatus = "loading" | "ready" | "error";

export type QuickConfigSaveError =
  | "not_ready"
  | "invalid_headers"
  | "invalid_score"
  | "invalid_concurrency"
  | "invalid_priority";

export type QuickConfigReadySaveError = Exclude<QuickConfigSaveError, "not_ready">;

export interface QuickConfigAccountSource {
  upstream_request_id_header?: string | null;
  id: number;
  detail_loaded?: boolean;
  codex_fingerprint_mode?: string | null;
  score_bias_override?: number | null;
  base_concurrency_override?: number | null;
  scheduler_priority?: number | null;
  skip_warm_tier?: boolean;
  proxy_url?: string | null;
  custom_headers?: Record<string, string> | null;
  tags?: string[] | null;
  group_ids?: number[] | null;
}

export interface QuickConfigFormState {
  upstreamRequestIdHeader: string;
  accountId: number;
  fingerprintMode: CodexFingerprintMode;
  scoreMode: "default" | "custom";
  scoreInput: string;
  concurrencyMode: "default" | "custom";
  concurrencyInput: string;
  schedulerPriorityInput: string;
  skipWarmTier: boolean;
  proxyUrl: string;
  customHeadersText: string;
  tags: string[];
  groupIds: number[];
}

export function accountHasQuickConfigDetails(
  account: { detail_loaded?: boolean } | null | undefined,
): boolean {
  return Boolean(account?.detail_loaded);
}

export function isAbortError(error: unknown): boolean {
  return (
    (typeof DOMException !== "undefined" &&
      error instanceof DOMException &&
      error.name === "AbortError") ||
    (error instanceof Error && error.name === "AbortError")
  );
}

export function normalizeCodexFingerprintMode(
  value: unknown,
): CodexFingerprintMode {
  if (
    value === "off" ||
    value === "device" ||
    value === "session" ||
    value === "full"
  ) {
    return value;
  }
  return "off";
}

export function parseCustomHeadersText(value: string): {
  ok: boolean;
  value: Record<string, string> | null;
} {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, value: null };
  try {
    const parsed = JSON.parse(trimmed) as unknown;
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { ok: false, value: null };
    }
    const result: Record<string, string> = {};
    for (const [key, item] of Object.entries(parsed)) {
      if (typeof item !== "string") return { ok: false, value: null };
      result[key] = item;
    }
    return { ok: true, value: result };
  } catch {
    return { ok: false, value: null };
  }
}

export function formatCustomHeadersText(
  headers?: Record<string, string> | null,
): string {
  if (!headers || Object.keys(headers).length === 0) return "";
  return JSON.stringify(headers, null, 2);
}

export function formStateFromAccount(
  account: QuickConfigAccountSource,
): QuickConfigFormState {
  return {
    accountId: account.id,
    upstreamRequestIdHeader: account.upstream_request_id_header ?? "",
    fingerprintMode: normalizeCodexFingerprintMode(account.codex_fingerprint_mode),
    scoreMode: account.score_bias_override != null ? "custom" : "default",
    scoreInput:
      account.score_bias_override != null ? String(account.score_bias_override) : "",
    concurrencyMode:
      account.base_concurrency_override != null ? "custom" : "default",
    concurrencyInput:
      account.base_concurrency_override != null
        ? String(account.base_concurrency_override)
        : "",
    schedulerPriorityInput:
      account.scheduler_priority != null ? String(account.scheduler_priority) : "",
    skipWarmTier: account.skip_warm_tier ?? false,
    proxyUrl: account.proxy_url ?? "",
    customHeadersText: formatCustomHeadersText(account.custom_headers),
    tags: account.tags ?? [],
    groupIds: account.group_ids ?? [],
  };
}

export function isQuickConfigFormCurrent(
  form: QuickConfigFormState | null,
  accountId: number | null | undefined,
): boolean {
  return form != null && accountId != null && form.accountId === accountId;
}

export function canSaveQuickConfig(options: {
  status: QuickConfigLoadStatus;
  saving: boolean;
  form: QuickConfigFormState | null;
  accountId: number | null | undefined;
}): boolean {
  return (
    options.status === "ready" &&
    !options.saving &&
    isQuickConfigFormCurrent(options.form, options.accountId)
  );
}

export function buildQuickConfigSavePayload(
  form: QuickConfigFormState | null,
  detailsReady: boolean,
):
  | { ok: true; payload: UpdateAccountSchedulerRequest }
  | { ok: false; error: QuickConfigSaveError } {
  if (!detailsReady || !form) {
    return { ok: false, error: "not_ready" };
  }

  const parsedHeaders = parseCustomHeadersText(form.customHeadersText);
  if (!parsedHeaders.ok) {
    return { ok: false, error: "invalid_headers" };
  }

  let parsedScoreBias: number | null = null;
  if (form.scoreMode === "custom") {
    const value = parseInt(form.scoreInput.trim(), 10);
    if (Number.isNaN(value) || value < -200 || value > 200) {
      return { ok: false, error: "invalid_score" };
    }
    parsedScoreBias = value;
  }

  let parsedBaseConcurrency: number | null = null;
  if (form.concurrencyMode === "custom") {
    const value = parseInt(form.concurrencyInput.trim(), 10);
    if (Number.isNaN(value) || value < 1) {
      return { ok: false, error: "invalid_concurrency" };
    }
    parsedBaseConcurrency = value;
  }

  let parsedSchedulerPriority: number | null = null;
  if (form.schedulerPriorityInput.trim()) {
    const value = parseInt(form.schedulerPriorityInput.trim(), 10);
    if (Number.isNaN(value) || value < -100 || value > 100) {
      return { ok: false, error: "invalid_priority" };
    }
    parsedSchedulerPriority = value;
  }

  return {
    ok: true,
    payload: {
      score_bias_override: form.scoreMode === "custom" ? parsedScoreBias : null,
      base_concurrency_override:
        form.concurrencyMode === "custom" ? parsedBaseConcurrency : null,
      scheduler_priority: parsedSchedulerPriority,
      skip_warm_tier: form.skipWarmTier,
      proxy_url: form.proxyUrl.trim() || null,
      custom_headers: parsedHeaders.value,
      upstream_request_id_header: form.upstreamRequestIdHeader.trim(),
      codex_fingerprint_mode: form.fingerprintMode,
      tags: form.tags,
      group_ids: form.groupIds,
    },
  };
}
