import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import {
  AlertTriangle,
  BarChart3,
  Check,
  ChevronLeft,
  ChevronRight,
  Copy,
  ExternalLink,
  FileJson,
  Fingerprint,
  FlaskConical,
  Lock,
  Pencil,
  Power,
  PowerOff,
  RefreshCw,
  RotateCcw,
  Timer,
  Trash2,
  Unlock,
  X,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import type { AccountGroup, AccountHealthBucket, AccountRow } from "../types";
import AccountHealthBar from "./AccountHealthBar";
import ChannelLogo from "./ChannelLogo";
import ModelLogo from "./ModelLogo";
import StatusBadge from "./StatusBadge";
import RequestCountPills from "./RequestCountPills";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Sheet,
  SheetBody,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import { formatLongUsageWindowLabel, getAccountStatusBadgeStatus } from "../lib/usageFormat";

function isFutureTime(value?: string): boolean {
  if (!value) return false;
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) && timestamp > Date.now();
}

function getRateLimitWindow(account: AccountRow): "5h" | "7d" | null {
  const status = (account.status || "").toLowerCase();
  const reason = (account.cooldown_reason || "").toLowerCase();
  if (status === "rate_limited_5h") return "5h";
  if (status === "rate_limited_7d") return "7d";
  if (reason === "rate_limited_5h") return "5h";
  if (reason === "rate_limited_7d") return "7d";
  const explicitlyRateLimited =
    status === "rate_limited" ||
    status === "responses_rate_limited" ||
    status === "quota_paused" ||
    status === "usage_exhausted" ||
    reason === "responses_rate_limited";
  if (!explicitlyRateLimited) return null;

  if (
    typeof account.usage_percent_7d === "number" &&
    account.usage_percent_7d >= 100 &&
    (!account.reset_7d_at || isFutureTime(account.reset_7d_at))
  ) {
    return "7d";
  }
  if (
    typeof account.usage_percent_5h === "number" &&
    account.usage_percent_5h >= 100 &&
    (!account.reset_5h_at || isFutureTime(account.reset_5h_at))
  ) {
    return "5h";
  }

  const has5hWindow =
    typeof account.usage_percent_5h === "number" || !!account.reset_5h_at;
  const has7dWindow =
    typeof account.usage_percent_7d === "number" || !!account.reset_7d_at;
  return has7dWindow && !has5hWindow ? "7d" : "5h";
}

// 复制邮箱按钮。navigator.clipboard 在非安全上下文（局域网 http 访问）下不存在，
// 回退到隐藏 textarea + execCommand，否则内网部署里这个按钮会静默失效。
async function copyTextToClipboard(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.left = "-9999px";
  document.body.appendChild(ta);
  ta.select();
  document.execCommand("copy");
  document.body.removeChild(ta);
}

function CopyValueButton({ value, label }: { value: string; label: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = window.setTimeout(() => setCopied(false), 1500);
    return () => window.clearTimeout(timer);
  }, [copied]);

  const handleCopy = async () => {
    try {
      await copyTextToClipboard(value);
      setCopied(true);
    } catch {
      // 剪贴板权限被拒时不打断查看详情，保持静默。
    }
  };

  return (
    <button
      type="button"
      onClick={() => void handleCopy()}
      title={copied ? t("common.copied") : label}
      aria-label={label}
      className="inline-flex size-6 shrink-0 items-center justify-center rounded-md border border-transparent text-muted-foreground transition-colors hover:border-border hover:bg-muted hover:text-foreground"
    >
      {copied ? (
        <Check className="size-3.5 text-emerald-600 dark:text-emerald-400" />
      ) : (
        <Copy className="size-3.5" />
      )}
    </button>
  );
}

function Section({
  title,
  children,
  action,
}: {
  title: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <section className="space-y-2.5">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
          {title}
        </h3>
        {action}
      </div>
      {children}
    </section>
  );
}

function MetricCard({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <div className="min-w-0 rounded-lg border border-border bg-muted/20 px-3 py-2.5">
      <div className="mb-1 text-[11px] font-medium text-muted-foreground">
        {label}
      </div>
      <div className="min-w-0 text-sm text-foreground">{children}</div>
    </div>
  );
}

export interface AccountDetailSheetProps {
  account: AccountRow | null;
  groups: AccountGroup[];
  healthBuckets?: AccountHealthBucket[];
  sequence?: number;
  usageSlot?: ReactNode;
  providerSlot?: ReactNode;
  canGoPrev?: boolean;
  canGoNext?: boolean;
  refreshing?: boolean;
  authJsonExporting?: boolean;
  onClose: () => void;
  onPrev?: () => void;
  onNext?: () => void;
  onQuickConfig?: () => void;
  onEdit: () => void;
  onUsage: () => void;
  onTest: () => void;
  onRefresh: () => void;
  onGenerateAuthJson: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onSaveModelCooldownPolicy: (data: {
    mode: "off" | "fixed" | "adaptive" | null;
    seconds: number | null;
    backoff_enabled: boolean | null;
  }) => void;
  onClearModelCooldown: (model: string) => void;
  onClearAllModelCooldowns: () => void;
  onResetCredits: () => void;
  onDelete: () => void;
}

export default function AccountDetailSheet({
  account,
  groups,
  healthBuckets,
  sequence,
  usageSlot,
  providerSlot,
  canGoPrev = false,
  canGoNext = false,
  refreshing = false,
  authJsonExporting = false,
  onClose,
  onPrev,
  onNext,
  onQuickConfig,
  onEdit,
  onUsage,
  onTest,
  onRefresh,
  onGenerateAuthJson,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onSaveModelCooldownPolicy,
  onClearModelCooldown,
  onClearAllModelCooldowns,
  onResetCredits,
  onDelete,
}: AccountDetailSheetProps) {
  const { t } = useTranslation();
  const open = Boolean(account);
  const [cooldownMode, setCooldownMode] = useState<string>("inherit");
  const [cooldownSeconds, setCooldownSeconds] = useState(300);
  const [cooldownBackoff, setCooldownBackoff] = useState(true);

  useEffect(() => {
    if (!account) return;
    setCooldownMode(account.model_cooldown_mode_override ?? "inherit");
    setCooldownSeconds(
      account.model_cooldown_seconds_override ??
        account.model_cooldown_seconds_effective ??
        300,
    );
    setCooldownBackoff(
      account.model_cooldown_backoff_override ??
        account.model_cooldown_backoff_effective ??
        true,
    );
  }, [account]);

  useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "ArrowLeft" && canGoPrev) {
        event.preventDefault();
        onPrev?.();
      } else if (event.key === "ArrowRight" && canGoNext) {
        event.preventDefault();
        onNext?.();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [open, canGoPrev, canGoNext, onPrev, onNext]);

  const displayName = account
    ? account.openai_responses_api
      ? account.name || account.email || `#${account.id}`
      : account.email || account.name || `#${account.id}`
    : "";
  // 标题退化成 "#12" 这种 id 兜底时没有复制价值（旁边就有 ID 徽章），不显示按钮。
  const copyableName = Boolean(
    account && displayName && displayName !== `#${account.id}`,
  );
  const rateWindow = account ? getRateLimitWindow(account) : null;
  const isGrok = Boolean(account?.grok_api);
  const isClaude = Boolean(account?.claude_api);
  // Grok API Key 无 refresh_token；Codex AT-only / Responses 也不走 AT 刷新。
  const refreshDisabled = Boolean(
    account &&
      (refreshing ||
        account.at_only ||
        account.openai_responses_api ||
        (isGrok && account.grok_auth_kind !== "oauth")),
  );
  // 凭据导出由各 provider 自己决定格式；Claude 使用专用安全导出端点，
  // Grok 仍由其专用页面处理。旧的 Codex auth.json 行为保持不变。
  const showAuthJson = Boolean(account && !isGrok);
  const showResetCredits = Boolean(account && !isGrok && !isClaude);
  const authJsonDisabled = Boolean(
    account &&
      (authJsonExporting || (!isClaude && (account.at_only || account.openai_responses_api))),
  );
  const resetCredits = account?.rate_limit_reset_credits ?? 0;
  const healthLabel = (() => {
    switch (account?.health_tier) {
      case "healthy":
        return t("accounts.healthy");
      case "warm":
        return t("accounts.warm");
      case "risky":
        return t("accounts.risky");
      case "banned":
        return t("accounts.quarantine");
      default:
        return t("accounts.unknown");
    }
  })();
  const longWindowLabel = account
    ? formatLongUsageWindowLabel(account)
    : "7d";
  const h5 =
    typeof account?.billed_5h === "number"
      ? account.billed_5h.toFixed(2)
      : null;
  const d7 =
    typeof account?.billed_7d === "number"
      ? account.billed_7d.toFixed(2)
      : null;

  return (
    <Sheet
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose();
      }}
    >
      {account ? (
        <SheetContent
          side="right"
          className="w-[min(calc(100%-1.5rem),460px)] max-w-[min(calc(100%-1.5rem),460px)] sm:w-[min(calc(100%-2rem),460px)] sm:max-w-[min(calc(100%-2rem),460px)]"
        >
          <SheetHeader>
            <div className="flex items-start justify-between gap-3 pr-2">
              <div className="flex min-w-0 flex-1 items-start gap-3">
                <span className="mt-0.5 flex size-10 shrink-0 items-center justify-center overflow-hidden rounded-xl bg-card ring-1 ring-border shadow-sm">
                  {account.claude_api ? (
                    <ChannelLogo channel="claude" size={22} />
                  ) : account.grok_api ? (
                    <ChannelLogo channel="grok" size={22} />
                  ) : account.openai_responses_api ? (
                    <ModelLogo model="openai" variant="plain" size={22} />
                  ) : (
                    <ChannelLogo channel="codex" size={40} className="rounded-xl" />
                  )}
                </span>
                <div className="min-w-0 flex-1">
                <div className="mb-1.5 flex flex-wrap items-center gap-1.5">
                  {sequence != null && (
                    <span className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-[11px] font-semibold text-muted-foreground">
                      #{sequence}
                    </span>
                  )}
                  <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
                    ID {account.id}
                  </span>
                  {account.plan_type && (
                    <span className="rounded-md bg-primary/10 px-1.5 py-0.5 text-[11px] font-semibold text-primary">
                      {account.plan_type}
                    </span>
                  )}
                </div>
                <div className="flex items-start gap-1.5">
                  <SheetTitle className="break-all text-[17px] leading-snug">
                    {displayName}
                  </SheetTitle>
                  {copyableName && (
                    <span className="mt-0.5">
                      <CopyValueButton
                        value={displayName}
                        label={
                          displayName.includes("@")
                            ? t("accounts.detailCopyEmail")
                            : t("common.copy")
                        }
                      />
                    </span>
                  )}
                </div>
                {account.effective_workspace_id ? (
                  <div className="mt-1 space-y-0.5">
                    <SheetDescription className="break-all font-mono text-[11px]">
                      {account.workspace_id_override
                        ? `${t("accounts.workspaceRouteBadge")}: `
                        : ""}
                      {account.effective_workspace_id}
                    </SheetDescription>
                    {account.workspace_id_override &&
                    account.token_workspace_id &&
                    account.token_workspace_id !==
                      account.effective_workspace_id ? (
                      <SheetDescription className="break-all font-mono text-[10px] text-muted-foreground/70">
                        {t("accounts.tokenWorkspaceLabel")}:{" "}
                        {account.token_workspace_id}
                      </SheetDescription>
                    ) : null}
                  </div>
                ) : isGrok && account.email && account.name && account.email !== account.name ? (
                  <SheetDescription className="mt-1 break-all text-[12px]">
                    {account.email}
                  </SheetDescription>
                ) : (
                  <SheetDescription className="mt-1">
                    {t("accounts.detailSubtitle")}
                  </SheetDescription>
                )}
                </div>
              </div>
              <div className="flex shrink-0 items-center gap-0.5">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  disabled={!canGoPrev}
                  onClick={onPrev}
                  title={t("accounts.detailPrev")}
                  aria-label={t("accounts.detailPrev")}
                >
                  <ChevronLeft className="size-4" />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  disabled={!canGoNext}
                  onClick={onNext}
                  title={t("accounts.detailNext")}
                  aria-label={t("accounts.detailNext")}
                >
                  <ChevronRight className="size-4" />
                </Button>
              </div>
            </div>
          </SheetHeader>

          <SheetBody className="space-y-5">
            <Section title={t("accounts.status")}>
              <div className="space-y-3 rounded-xl border border-border bg-card p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <StatusBadge
                    status={getAccountStatusBadgeStatus(account)}
                    detail={rateWindow ?? undefined}
                    errorMessage={account.error_message}
                  />
                  {Math.max(account.active_requests ?? 0, account.occupied_requests ?? 0) > 0 && (
                    <span className="inline-flex items-center gap-1 rounded-md bg-blue-500/10 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
                      <span className="size-1.5 animate-pulse rounded-full bg-blue-500" />
                      {account.session_slot_buffer_enabled === true
                        ? t("accounts.occupiedRequestsTooltip", {
                            active: account.active_requests ?? 0,
                            occupied: account.occupied_requests ?? account.active_requests ?? 0,
                            buffered: (account.occupied_requests ?? account.active_requests ?? 0) - (account.active_requests ?? 0),
                          })
                        : t("accounts.activeRequestsTooltip", {
                            count: account.active_requests ?? 0,
                          })}
                    </span>
                  )}
                  {account.enabled === false && (
                    <span className="inline-flex items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
                      <PowerOff className="size-3" />
                      {t("accounts.disabled")}
                    </span>
                  )}
                  {account.locked && (
                    <span className="inline-flex items-center gap-1 rounded-md bg-blue-500/10 px-1.5 py-0.5 text-[11px] font-medium text-blue-700 dark:text-blue-300">
                      <Lock className="size-3" />
                      {t("accounts.lock")}
                    </span>
                  )}
                </div>

                <div className="text-[12px] text-muted-foreground">
                  {t("accounts.healthSummary", {
                    health: healthLabel,
                    score: Math.round(
                      account.dispatch_score ?? account.scheduler_score ?? 0,
                    ),
                    concurrency: account.dynamic_concurrency_limit ?? "-",
                  })}
                </div>

                <div className="space-y-1">
                  <div className="text-[10px] text-muted-foreground/80">
                    {t("accounts.healthBarLabel")}
                  </div>
                  <AccountHealthBar buckets={healthBuckets} />
                </div>

                {account.status === "error" && account.error_message ? (
                  <div className="flex items-start gap-2 rounded-lg bg-red-500/10 px-2.5 py-2 text-[12px] leading-snug text-red-600 dark:text-red-300">
                    <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                    <span className="break-words">{account.error_message}</span>
                  </div>
                ) : null}

                {(account.model_cooldowns?.length ?? 0) > 0 ? (
                  <div className="rounded-lg bg-amber-500/10 px-2.5 py-2 text-[12px] text-amber-700 dark:text-amber-300">
                    {t("accounts.modelCooldown", {
                      model: account.model_cooldowns?.[0]?.model ?? "",
                    })}
                    {(account.model_cooldowns?.length ?? 0) > 1
                      ? ` +${(account.model_cooldowns?.length ?? 1) - 1}`
                      : ""}
                  </div>
                ) : null}
              </div>
            </Section>

            {providerSlot}

            {!isGrok ? <Section
              title={t("accounts.modelCooldownPolicy")}
              action={
                (account.model_cooldowns?.length ?? 0) > 0 ? (
                  <Button
                    type="button"
                    variant="ghost"
                    size="xs"
                    onClick={onClearAllModelCooldowns}
                    className="h-7 text-[11px] text-amber-700 dark:text-amber-300"
                  >
                    <X className="size-3" />
                    {t("accounts.clearAllModelCooldowns")}
                  </Button>
                ) : null
              }
            >
              <div className="space-y-3 rounded-xl border border-border bg-card p-3">
                <div className="rounded-lg bg-muted/40 px-2.5 py-2 text-[11px] leading-relaxed text-muted-foreground">
                  {t("accounts.modelCooldownEffective", {
                    mode: t(
                      `settings.modelCooldownMode${(
                        account.model_cooldown_mode_effective ?? "adaptive"
                      )
                        .charAt(0)
                        .toUpperCase()}${(
                        account.model_cooldown_mode_effective ?? "adaptive"
                      ).slice(1)}`,
                    ),
                    seconds: account.model_cooldown_seconds_effective ?? 0,
                    backoff: account.model_cooldown_backoff_effective
                      ? t("common.enabled")
                      : t("common.disabled"),
                  })}
                </div>
                <div className="grid grid-cols-2 gap-2">
                  <div className="col-span-2">
                    <label className="mb-1 block text-[11px] font-medium text-muted-foreground">
                      {t("accounts.modelCooldownModeOverride")}
                    </label>
                    <Select
                      value={cooldownMode}
                      onValueChange={setCooldownMode}
                      options={[
                        { label: t("accounts.modelCooldownInherit"), value: "inherit" },
                        { label: t("settings.modelCooldownModeOff"), value: "off" },
                        { label: t("settings.modelCooldownModeFixed"), value: "fixed" },
                        { label: t("settings.modelCooldownModeAdaptive"), value: "adaptive" },
                      ]}
                    />
                  </div>
                  <div>
                    <label className="mb-1 block text-[11px] font-medium text-muted-foreground">
                      {t("settings.modelCooldownSeconds")}
                    </label>
                    <Input
                      type="number"
                      min={1}
                      max={1800}
                      value={cooldownSeconds}
                      disabled={cooldownMode === "off"}
                      onChange={(event) =>
                        setCooldownSeconds(
                          Math.max(1, Math.min(1800, Number(event.target.value) || 1)),
                        )
                      }
                    />
                  </div>
                  <div className="flex items-end">
                    <div className="flex h-10 w-full items-center justify-between rounded-md border border-input px-3">
                      <span className="text-xs text-muted-foreground">
                        {t("settings.modelCooldownBackoff")}
                      </span>
                      <Switch
                        checked={cooldownBackoff}
                        disabled={cooldownMode !== "adaptive"}
                        onCheckedChange={setCooldownBackoff}
                      />
                    </div>
                  </div>
                </div>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="w-full"
                  onClick={() =>
                    onSaveModelCooldownPolicy({
                      mode:
                        cooldownMode === "inherit"
                          ? null
                          : (cooldownMode as "off" | "fixed" | "adaptive"),
                      seconds:
                        cooldownMode === "inherit" ? null : cooldownSeconds,
                      backoff_enabled:
                        cooldownMode === "inherit" ? null : cooldownBackoff,
                    })
                  }
                >
                  {t("accounts.saveModelCooldownPolicy")}
                </Button>
                {(account.model_cooldowns?.length ?? 0) > 0 ? (
                  <div className="space-y-1.5 border-t border-border pt-3">
                    {account.model_cooldowns?.map((cooldown) => (
                      <div
                        key={cooldown.model}
                        className="flex items-center justify-between gap-2 rounded-lg bg-amber-500/10 px-2.5 py-2 text-xs"
                      >
                        <div className="min-w-0">
                          <div className="truncate font-medium text-amber-800 dark:text-amber-200">
                            {cooldown.model}
                          </div>
                          <div className="text-[10px] text-muted-foreground">
                            {cooldown.reason} · {cooldown.remaining_seconds}s
                          </div>
                        </div>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          onClick={() => onClearModelCooldown(cooldown.model)}
                          title={t("accounts.clearModelCooldown")}
                        >
                          <X className="size-3.5" />
                        </Button>
                      </div>
                    ))}
                  </div>
                ) : null}
              </div>
            </Section> : null}

            <Section
              title={t("accounts.usage")}
              action={
                <Button
                  type="button"
                  variant="ghost"
                  size="xs"
                  onClick={onUsage}
                  className="h-7 text-[11px]"
                >
                  <ExternalLink className="size-3" />
                  {t("accounts.actionUsageDetail")}
                </Button>
              }
            >
              <div className="rounded-xl border border-border bg-card p-3">
                {usageSlot ?? (
                  <span className="text-sm text-muted-foreground">—</span>
                )}
              </div>
            </Section>

            <Section title={t("accounts.detailMetrics")}>
              <div className="grid grid-cols-2 gap-2">
                <MetricCard label={t("accounts.requests")}>
                  <RequestCountPills account={account} />
                </MetricCard>
                <MetricCard label={t("accounts.billed")}>
                  {h5 === null && d7 === null ? (
                    <span className="text-muted-foreground">—</span>
                  ) : (
                    <div className="space-y-0.5 text-[12px] tabular-nums">
                      <div>5h: {h5 !== null ? `$${h5}` : "—"}</div>
                      <div>
                        {longWindowLabel}: {d7 !== null ? `$${d7}` : "—"}
                      </div>
                    </div>
                  )}
                </MetricCard>
                <MetricCard label={t("accounts.importTime")}>
                  <span className="text-[12px]">
                    {formatBeijingTime(account.created_at)}
                  </span>
                </MetricCard>
                <MetricCard label={t("accounts.updatedAt")}>
                  <div className="space-y-0.5 text-[12px]">
                    <div>{formatRelativeTime(account.updated_at)}</div>
                    {account.codex_usage_updated_at ? (
                      <div className="text-muted-foreground">
                        {t("accounts.usageUpdatedAtShort")}{" "}
                        {formatRelativeTime(account.codex_usage_updated_at)}
                      </div>
                    ) : null}
                  </div>
                </MetricCard>
              </div>
            </Section>

            {((account.tags ?? []).length > 0 || groups.length > 0) && (
              <Section title={t("accounts.detailOrganization")}>
                <div className="space-y-2 rounded-xl border border-border bg-card p-3">
                  {(account.tags ?? []).length > 0 && (
                    <div className="flex flex-wrap gap-1">
                      {(account.tags ?? []).map((tag) => (
                        <span
                          key={tag}
                          className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground"
                        >
                          {tag}
                        </span>
                      ))}
                    </div>
                  )}
                  {groups.length > 0 && (
                    <div className="flex flex-wrap gap-1">
                      {groups.map((group) => {
                        const color = /^#[0-9a-fA-F]{6}$/.test(group.color || "")
                          ? group.color!
                          : "#2563eb";
                        return (
                          <span
                            key={group.id}
                            className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium"
                            style={{
                              backgroundColor: `${color}14`,
                              color,
                              boxShadow: `inset 0 0 0 1px ${color}33`,
                            }}
                          >
                            <span className="size-1.5 rounded-full bg-current" />
                            {group.name}
                          </span>
                        );
                      })}
                    </div>
                  )}
                </div>
              </Section>
            )}

            {(account.proxy_url ||
              account.at_only ||
              account.openai_responses_api ||
              account.grok_api ||
              account.claude_api ||
              account.base_url ||
              (!account.openai_responses_api &&
                (account.models?.length ?? 0) > 0)) && (
              <Section title={t("accounts.detailTechnical")}>
                <div className="space-y-2 rounded-xl border border-border bg-card p-3 text-[12px]">
                  {account.at_only && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.detailAuthType")}
                      </span>
                      <span className="font-medium text-foreground">
                        {t("accounts.detailAuthTypeAT")}
                      </span>
                    </div>
                  )}
                  {account.openai_responses_api && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.detailApiLabel")}
                      </span>
                      <span className="font-medium text-foreground">
                        {t("accounts.detailApiResponses")}
                      </span>
                    </div>
                  )}
                  {isClaude && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.detailAuthType")}
                      </span>
                      <span className="font-medium text-foreground">{t("claude.authOAuth")}</span>
                    </div>
                  )}
                  {isGrok && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.detailAuthType")}
                      </span>
                      <span className="font-medium text-foreground">
                        {account.grok_auth_kind === "oauth"
                          ? t("grok.authKindOAuthShort")
                          : t("grok.authKindApiKey")}
                      </span>
                    </div>
                  )}
                  {account.base_url && (
                    <div className="flex justify-between gap-3">
                      <span className="shrink-0 text-muted-foreground">
                        {t("accounts.detailBaseUrl")}
                      </span>
                      <span className="min-w-0 break-all text-right font-mono text-[11px] text-foreground">
                        {account.base_url}
                      </span>
                    </div>
                  )}
                  {account.proxy_url && (
                    <div className="flex justify-between gap-3">
                      <span className="shrink-0 text-muted-foreground">
                        {t("accounts.detailProxy")}
                      </span>
                      <span className="min-w-0 break-all text-right font-mono text-[11px] text-foreground">
                        {account.proxy_url}
                      </span>
                    </div>
                  )}
                  {showResetCredits && resetCredits > 0 && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.resetCreditsButton")}
                      </span>
                      <span className="font-medium tabular-nums text-foreground">
                        {resetCredits}
                      </span>
                    </div>
                  )}
                  {(account.models?.length ?? 0) > 0 && (
                      <div className="flex justify-between gap-3">
                        <span className="shrink-0 text-muted-foreground">
                          {t("accounts.supportedModelsAction")}
                        </span>
                        <span className="font-medium tabular-nums text-foreground">
                          {t("accounts.supportedModelsCount", {
                            count: account.models?.length ?? 0,
                          })}
                        </span>
                      </div>
                    )}
                </div>
              </Section>
            )}
          </SheetBody>

          <SheetFooter>
            <div className="grid grid-cols-2 gap-2">
              {onQuickConfig ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={onQuickConfig}
                  className="col-span-2 border-primary/40 bg-primary/10 font-bold text-primary hover:bg-primary/20"
                >
                  <Fingerprint className="size-4 text-primary" />
                  <span>指纹与快捷配置</span>
                </Button>
              ) : null}
              <Button type="button" variant="default" size="sm" onClick={onEdit}>
                <Pencil className="size-3.5" />
                {t("accounts.editScheduler")}
              </Button>
              <Button type="button" variant="outline" size="sm" onClick={onUsage}>
                <BarChart3 className="size-3.5" />
                {t("accounts.usageDetail")}
              </Button>
              <Button type="button" variant="outline" size="sm" onClick={onTest}>
                <FlaskConical className="size-3.5" />
                {t("accounts.testConnection")}
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={refreshDisabled}
                onClick={onRefresh}
              >
                <RefreshCw
                  className={`size-3.5 ${refreshing ? "animate-spin" : ""}`}
                />
                {isClaude
                  ? t("claude.actionRefresh")
                  : isGrok
                  ? t("grok.actionRefresh")
                  : t("accounts.actionRefreshAT")}
              </Button>
              {showAuthJson && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={authJsonDisabled}
                  onClick={onGenerateAuthJson}
                >
                  <FileJson className="size-3.5" />
                  {isClaude ? t("claude.exportCredential") : t("accounts.actionAuthJson")}
                </Button>
              )}
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={onToggleEnabled}
              >
                {account.enabled === false ? (
                  <Power className="size-3.5" />
                ) : (
                  <PowerOff className="size-3.5" />
                )}
                {account.enabled === false
                  ? t("accounts.actionEnableScheduling")
                  : t("accounts.actionDisableScheduling")}
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={onToggleLock}
              >
                {account.locked ? (
                  <Unlock className="size-3.5" />
                ) : (
                  <Lock className="size-3.5" />
                )}
                {account.locked
                  ? t("accounts.actionUnlockAccount")
                  : t("accounts.actionLockAccount")}
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={onResetStatus}
              >
                <RotateCcw className="size-3.5" />
                {t("accounts.resetStatus")}
              </Button>
              {showResetCredits && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={resetCredits <= 0}
                  onClick={onResetCredits}
                >
                  <Timer className="size-3.5" />
                  {t("accounts.resetCreditsButton")}
                </Button>
              )}
              <Button
                type="button"
                variant="destructive"
                size="sm"
                onClick={onDelete}
              >
                <Trash2 className="size-3.5" />
                {t("accounts.deleteAccount")}
              </Button>
            </div>
          </SheetFooter>
        </SheetContent>
      ) : null}
    </Sheet>
  );
}
