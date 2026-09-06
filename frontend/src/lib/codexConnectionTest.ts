// Codex/Responses 测连诊断的类型与纯展示逻辑。与 Claude 的 claudeConnectionTest.ts
// 平行但独立:字段语义按 Responses API 与 x-codex-* 用量头定义。

export interface CodexTestWindow {
  used_percent?: number;
  window_minutes?: number;
  reset_after_seconds?: number;
}

export interface CodexTestUsage {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
  reasoning_output_tokens?: number;
}

export interface CodexTestDiagnostics {
  http_status?: number;
  duration_ms?: number;
  headers_ms?: number;
  first_content_ms?: number;
  model: string;
  response_model?: string;
  transport?: string;
  request_id?: string;
  response_id?: string;
  cf_ray?: string;
  plan_type?: string;
  safety_buffering_enabled?: boolean;
  safety_buffering_faster_model?: string;
  safety_buffered?: boolean;
  response_status?: string;
  incomplete_reason?: string;
  error_type?: string;
  error_code?: string;
  primary_window?: CodexTestWindow;
  secondary_window?: CodexTestWindow;
  usage?: CodexTestUsage;
  response_headers?: Array<{ name: string; value: string }>;
  response_body?: string;
  body_truncated?: boolean;
}

export type CodexTestWindowKind = "5h" | "7d" | "short" | "unknown";

// 与后端 windowMinutesToCooldown 同口径:>= 1 天算 7d,>= 1 小时算 5h。
export function codexTestWindowKind(window?: CodexTestWindow): CodexTestWindowKind {
  const minutes = window?.window_minutes;
  if (typeof minutes !== "number" || !Number.isFinite(minutes) || minutes <= 0) return "unknown";
  if (minutes >= 1440) return "7d";
  if (minutes >= 60) return "5h";
  return "short";
}

export function formatCodexTestReset(seconds?: number): string {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds < 0) return "";
  const total = Math.round(seconds);
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`;
  if (minutes > 0) return `${minutes}m`;
  return `${total}s`;
}

export function clampCodexTestPercent(value?: number): number | null {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return null;
  return Math.min(100, value);
}

export const codexTestTokenKeys = ["input_tokens", "output_tokens", "cached_input_tokens", "reasoning_output_tokens"] as const;
export type CodexTestTokenKey = (typeof codexTestTokenKeys)[number];

export function codexTestTokenMetrics(usage?: CodexTestUsage) {
  const metrics = codexTestTokenKeys.map((key) => {
    const raw = usage?.[key];
    return { key, value: typeof raw === "number" && Number.isFinite(raw) && raw >= 0 ? raw : null };
  });
  const max = Math.max(1, ...metrics.map(({ value }) => value ?? 0));
  return metrics.map((item) => ({ ...item, percent: item.value === null ? 0 : (item.value / max) * 100 }));
}

// 首帧诊断只有响应头信息;带 duration_ms 的才是流结束后的最终帧。
export function isFinalCodexTestDiagnostics(diagnostics?: CodexTestDiagnostics | null): boolean {
  return typeof diagnostics?.duration_ms === "number";
}

export function formatCodexTestMS(value?: number): string {
  return typeof value === "number" && Number.isFinite(value) ? `${value.toLocaleString()} ms` : "—";
}
