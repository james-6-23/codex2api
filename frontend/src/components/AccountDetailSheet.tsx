import type { ReactNode } from "react";
import { useEffect } from "react";
import {
  AlertTriangle,
  BarChart3,
  ChevronLeft,
  ChevronRight,
  ExternalLink,
  FileJson,
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
} from "lucide-react";
import { useTranslation } from "react-i18next";
import type { AccountGroup, AccountHealthBucket, AccountRow } from "../types";
import AccountHealthBar from "./AccountHealthBar";
import StatusBadge from "./StatusBadge";
import { Button } from "@/components/ui/button";
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
import { formatLongUsageWindowLabel } from "../lib/usageFormat";

function isFutureTime(value?: string): boolean {
  if (!value) return false;
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) && timestamp > Date.now();
}

function getRateLimitWindow(account: AccountRow): "5h" | "7d" | null {
  const status = (account.status || "").toLowerCase();
  if (status === "rate_limited" || status === "quota_paused" || status === "usage_exhausted") {
    if (account.reset_5h_at && isFutureTime(account.reset_5h_at)) return "5h";
    if (account.reset_7d_at && isFutureTime(account.reset_7d_at)) return "7d";
    if (typeof account.usage_percent_5h === "number" && account.usage_percent_5h >= 100)
      return "5h";
    if (typeof account.usage_percent_7d === "number" && account.usage_percent_7d >= 100)
      return "7d";
  }
  return null;
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
  canGoPrev?: boolean;
  canGoNext?: boolean;
  refreshing?: boolean;
  authJsonExporting?: boolean;
  onClose: () => void;
  onPrev?: () => void;
  onNext?: () => void;
  onEdit: () => void;
  onUsage: () => void;
  onTest: () => void;
  onRefresh: () => void;
  onGenerateAuthJson: () => void;
  onToggleEnabled: () => void;
  onToggleLock: () => void;
  onResetStatus: () => void;
  onResetCredits: () => void;
  onDelete: () => void;
}

export default function AccountDetailSheet({
  account,
  groups,
  healthBuckets,
  sequence,
  usageSlot,
  canGoPrev = false,
  canGoNext = false,
  refreshing = false,
  authJsonExporting = false,
  onClose,
  onPrev,
  onNext,
  onEdit,
  onUsage,
  onTest,
  onRefresh,
  onGenerateAuthJson,
  onToggleEnabled,
  onToggleLock,
  onResetStatus,
  onResetCredits,
  onDelete,
}: AccountDetailSheetProps) {
  const { t } = useTranslation();
  const open = Boolean(account);

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
  const rateWindow = account ? getRateLimitWindow(account) : null;
  const refreshDisabled = Boolean(
    account &&
      (refreshing || account.at_only || account.openai_responses_api),
  );
  const authJsonDisabled = Boolean(
    account &&
      (authJsonExporting || account.at_only || account.openai_responses_api),
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
                <SheetTitle className="break-all text-[17px] leading-snug">
                  {displayName}
                </SheetTitle>
                {account.chatgpt_account_id ? (
                  <SheetDescription className="mt-1 break-all font-mono text-[11px]">
                    {account.chatgpt_account_id}
                  </SheetDescription>
                ) : (
                  <SheetDescription className="mt-1">
                    {t("accounts.detailSubtitle")}
                  </SheetDescription>
                )}
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
                    status={account.status}
                    detail={rateWindow ?? undefined}
                    errorMessage={account.error_message}
                  />
                  {(account.active_requests ?? 0) > 0 && (
                    <span className="inline-flex items-center gap-1 rounded-md bg-blue-500/10 px-1.5 py-0.5 text-[11px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
                      <span className="size-1.5 animate-pulse rounded-full bg-blue-500" />
                      {t("accounts.activeRequestsTooltip", {
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
                    model {account.model_cooldowns?.[0]?.model}
                    {(account.model_cooldowns?.length ?? 0) > 1
                      ? ` +${(account.model_cooldowns?.length ?? 1) - 1}`
                      : ""}
                  </div>
                ) : null}
              </div>
            </Section>

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
                  <div className="flex items-baseline gap-1.5 tabular-nums">
                    <span className="font-semibold text-emerald-600 dark:text-emerald-400">
                      {account.success_requests ?? 0}
                    </span>
                    <span className="text-muted-foreground">/</span>
                    <span className="font-semibold text-red-500">
                      {account.error_requests ?? 0}
                    </span>
                  </div>
                  {((account.retry_error_requests ?? 0) > 0 ||
                    (account.rate_limit_attempts ?? 0) > 0) && (
                    <div className="mt-1 text-[11px] text-muted-foreground">
                      retry {account.retry_error_requests ?? 0} · 429{" "}
                      {account.rate_limit_attempts ?? 0}
                    </div>
                  )}
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
              account.base_url) && (
              <Section title={t("accounts.detailTechnical")}>
                <div className="space-y-2 rounded-xl border border-border bg-card p-3 text-[12px]">
                  {account.at_only && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.detailAuthType")}
                      </span>
                      <span className="font-medium text-foreground">
                        Access Token
                      </span>
                    </div>
                  )}
                  {account.openai_responses_api && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">API</span>
                      <span className="font-medium text-foreground">
                        Responses
                      </span>
                    </div>
                  )}
                  {account.base_url && (
                    <div className="flex justify-between gap-3">
                      <span className="shrink-0 text-muted-foreground">
                        Base URL
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
                  {resetCredits > 0 && (
                    <div className="flex justify-between gap-3">
                      <span className="text-muted-foreground">
                        {t("accounts.resetCreditsButton")}
                      </span>
                      <span className="font-medium tabular-nums text-foreground">
                        {resetCredits}
                      </span>
                    </div>
                  )}
                </div>
              </Section>
            )}
          </SheetBody>

          <SheetFooter>
            <div className="grid grid-cols-2 gap-2">
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
                {t("accounts.actionRefreshAT")}
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={authJsonDisabled}
                onClick={onGenerateAuthJson}
              >
                <FileJson className="size-3.5" />
                {t("accounts.actionAuthJson")}
              </Button>
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
