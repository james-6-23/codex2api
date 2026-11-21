import type { AccountRow, UpdateAccountSchedulerRequest } from "../types";
import {
  autoPauseThresholdPercentToRatio,
  formatAutoPauseThresholdPercent,
  isAutoPauseThresholdPercentInvalid,
} from "./autoPauseThreshold.ts";

// Antigravity 账号复用 Codex 的通用调度项(/accounts/:id/scheduler)。只放对它真实生效的字段:
// 设备指纹、时区、自定义请求头、忽略用量限流都是 Codex/Claude 专属,不在这里。
// 5h / 周自动暂停在后端按 Antigravity 模型族分别判断(某族到阈值只暂停该族请求)。
export interface AntigravitySchedulerDraft {
  scoreBias: string;
  concurrency: string;
  priority: string;
  dispatchLimit: string;
  autoPause5h: string;
  autoPause7d: string;
  autoPause5hDisabled: boolean;
  autoPause7dDisabled: boolean;
  skipWarmTier: boolean;
  keepConcurrency: boolean;
  allowedAPIKeyIDs: number[];
  tags: string[];
}

export type AntigravitySchedulerError = "score" | "concurrency" | "priority" | "dispatchLimit" | "autoPause";

export function schedulerDraftFromAccount(account: AccountRow): AntigravitySchedulerDraft {
  const text = (value?: number | null) => (typeof value === "number" ? String(Math.trunc(value)) : "");
  return {
    scoreBias: text(account.score_bias_override),
    concurrency: text(account.base_concurrency_override),
    priority: account.scheduler_priority ? text(account.scheduler_priority) : "",
    dispatchLimit: account.dispatch_count_limit && account.dispatch_count_limit > 0 ? text(account.dispatch_count_limit) : "",
    autoPause5h: formatAutoPauseThresholdPercent(account.auto_pause_5h_threshold),
    autoPause7d: formatAutoPauseThresholdPercent(account.auto_pause_7d_threshold),
    autoPause5hDisabled: account.auto_pause_5h_disabled === true,
    autoPause7dDisabled: account.auto_pause_7d_disabled === true,
    skipWarmTier: account.skip_warm_tier === true,
    keepConcurrency: account.keep_concurrency_on_degrade === true,
    allowedAPIKeyIDs: [...(account.allowed_api_key_ids ?? [])],
    tags: [...(account.tags ?? [])],
  };
}

// 空串 → null(跟随默认);非整数或越界 → undefined(校验失败)。
function parseInteger(value: string, min: number, max: number): number | null | undefined {
  const trimmed = value.trim();
  if (!trimmed) return null;
  if (!/^-?\d+$/.test(trimmed)) return undefined;
  const parsed = Number(trimmed);
  return parsed >= min && parsed <= max ? parsed : undefined;
}

function sameList<T>(a: readonly T[], b: readonly T[]): boolean {
  if (a.length !== b.length) return false;
  const left = [...a].sort();
  const right = [...b].sort();
  return left.every((value, index) => value === right[index]);
}

/** 只返回相对账号当前值有变化的字段;空对象表示无需请求。 */
export function buildSchedulerPatch(
  account: AccountRow,
  draft: AntigravitySchedulerDraft,
): { patch: UpdateAccountSchedulerRequest } | { error: AntigravitySchedulerError } {
  const score = parseInteger(draft.scoreBias, -200, 200);
  if (score === undefined) return { error: "score" };
  const concurrency = parseInteger(draft.concurrency, 1, 1_000_000);
  if (concurrency === undefined) return { error: "concurrency" };
  const priority = parseInteger(draft.priority, -100, 100);
  if (priority === undefined) return { error: "priority" };
  const dispatchLimit = parseInteger(draft.dispatchLimit, 0, 1_000_000);
  if (dispatchLimit === undefined) return { error: "dispatchLimit" };
  if (isAutoPauseThresholdPercentInvalid(draft.autoPause5h) || isAutoPauseThresholdPercentInvalid(draft.autoPause7d)) {
    return { error: "autoPause" };
  }

  const current = schedulerDraftFromAccount(account);
  const patch: UpdateAccountSchedulerRequest = {};
  if (score !== (account.score_bias_override ?? null)) patch.score_bias_override = score;
  if (concurrency !== (account.base_concurrency_override ?? null)) patch.base_concurrency_override = concurrency;
  // 0 与空都表示"默认/关闭",与 Codex 编辑页保存口径一致。
  const nextPriority = priority === 0 ? null : priority;
  if (nextPriority !== (account.scheduler_priority || null)) patch.scheduler_priority = nextPriority;
  const nextDispatchLimit = dispatchLimit === 0 ? null : dispatchLimit;
  const currentDispatchLimit = account.dispatch_count_limit && account.dispatch_count_limit > 0 ? account.dispatch_count_limit : null;
  if (nextDispatchLimit !== currentDispatchLimit) patch.dispatch_count_limit = nextDispatchLimit;
  const pause5h = autoPauseThresholdPercentToRatio(draft.autoPause5h);
  if (pause5h !== autoPauseThresholdPercentToRatio(current.autoPause5h)) patch.auto_pause_5h_threshold = pause5h;
  const pause7d = autoPauseThresholdPercentToRatio(draft.autoPause7d);
  if (pause7d !== autoPauseThresholdPercentToRatio(current.autoPause7d)) patch.auto_pause_7d_threshold = pause7d;
  if (draft.autoPause5hDisabled !== current.autoPause5hDisabled) patch.auto_pause_5h_disabled = draft.autoPause5hDisabled;
  if (draft.autoPause7dDisabled !== current.autoPause7dDisabled) patch.auto_pause_7d_disabled = draft.autoPause7dDisabled;
  if (draft.skipWarmTier !== current.skipWarmTier) patch.skip_warm_tier = draft.skipWarmTier;
  if (draft.keepConcurrency !== current.keepConcurrency) patch.keep_concurrency_on_degrade = draft.keepConcurrency;
  if (!sameList(draft.allowedAPIKeyIDs, current.allowedAPIKeyIDs)) patch.allowed_api_key_ids = draft.allowedAPIKeyIDs;
  if (!sameList(draft.tags, current.tags)) patch.tags = draft.tags;
  return { patch };
}

/** 最早结束的模型冷却,用于列表行的实时倒计时。 */
export function soonestModelCooldown(account: AccountRow, nowMs: number) {
  const active = (account.model_cooldowns ?? [])
    .map((cooldown) => ({ ...cooldown, resetMs: new Date(cooldown.reset_at).getTime() }))
    .filter((cooldown) => Number.isFinite(cooldown.resetMs) && cooldown.resetMs > nowMs)
    .sort((a, b) => a.resetMs - b.resetMs);
  return active.length ? { first: active[0], count: active.length } : null;
}
