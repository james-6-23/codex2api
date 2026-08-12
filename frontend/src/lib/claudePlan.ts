import type { AccountRow } from "../types";

/**
 * Claude Code 套餐解析。
 *
 * 后端把 bootstrap 的 `organization_rate_limit_tier`（形如 `default_claude_max_20x`）
 * 映射成 plan_type 键写进凭据（auth.ClaudePlanFromRateLimitTier），但探针失败时
 * plan_type 会留空，只剩原始档位串。两个来源都要认，且都可能缺失。
 */
export const CLAUDE_KNOWN_PLAN_KEYS = [
  "free",
  "pro",
  "max5",
  "max20",
  "team",
  "enterprise",
] as const;

export type ClaudeKnownPlanKey = (typeof CLAUDE_KNOWN_PLAN_KEYS)[number];
export type ClaudePlanFilter = "all" | ClaudeKnownPlanKey | "other";

export interface ClaudePlanInfo {
  key: string;
  display: string;
  /** 付费订阅档（Pro/Max/Team/Enterprise）；Free 为 false */
  paid: boolean;
}

const PLANS_BY_KEY: Record<ClaudeKnownPlanKey, ClaudePlanInfo> = {
  free: { key: "free", display: "Free", paid: false },
  pro: { key: "pro", display: "Pro", paid: true },
  max5: { key: "max5", display: "Max 5×", paid: true },
  max20: { key: "max20", display: "Max 20×", paid: true },
  team: { key: "team", display: "Team", paid: true },
  enterprise: { key: "enterprise", display: "Enterprise", paid: true },
};

const KNOWN_PLAN_KEY_SET = new Set<string>(CLAUDE_KNOWN_PLAN_KEYS);

/**
 * resolveClaudePlan 接受 plan_type 键或原始限额档位串，返回展示信息。
 * 判定顺序与后端 ClaudePlanFromRateLimitTier 一致：max20 → max5 → team →
 * enterprise → pro → free（`max_20x` 里也含 `max`，先粗后细会误判）。
 */
export function resolveClaudePlan(value: unknown): ClaudePlanInfo | null {
  if (typeof value !== "string") return null;
  const raw = value.trim().toLowerCase();
  if (!raw) return null;
  if (KNOWN_PLAN_KEY_SET.has(raw)) {
    return PLANS_BY_KEY[raw as ClaudeKnownPlanKey];
  }
  if (raw.includes("max_20x") || raw.includes("max20")) return PLANS_BY_KEY.max20;
  if (raw.includes("max_5x") || raw.includes("max5")) return PLANS_BY_KEY.max5;
  if (raw.includes("team")) return PLANS_BY_KEY.team;
  if (raw.includes("enterprise")) return PLANS_BY_KEY.enterprise;
  if (raw.includes("pro")) return PLANS_BY_KEY.pro;
  if (raw.includes("free")) return PLANS_BY_KEY.free;
  return null;
}

export function resolveAccountClaudePlan(
  account: Pick<AccountRow, "plan_type" | "claude_rate_limit_tier">,
): ClaudePlanInfo | null {
  return (
    resolveClaudePlan(account.plan_type) ??
    resolveClaudePlan(account.claude_rate_limit_tier)
  );
}

export function claudePlanFilterCategory(
  account: Pick<AccountRow, "plan_type" | "claude_rate_limit_tier">,
): Exclude<ClaudePlanFilter, "all"> {
  const plan = resolveAccountClaudePlan(account);
  if (plan && KNOWN_PLAN_KEY_SET.has(plan.key)) {
    return plan.key as ClaudeKnownPlanKey;
  }
  return "other";
}
