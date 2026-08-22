import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import type { AccountRow } from "../types";
import ModelLogo, { resolveLobeIconId } from "./ModelLogo";
import {
  buildErrorStatusBreakdown,
  buildModelCountBreakdown,
  formatErrorStatusPercent,
} from "../lib/requestErrorStatus";

export default function RequestCountPills({
  account,
  compact = false,
}: {
  account: AccountRow;
  compact?: boolean;
}) {
  const { t } = useTranslation();
  const success = account.success_requests ?? 0;
  const errors = account.error_requests ?? 0;
  const retryErrors = account.retry_error_requests ?? 0;
  const rateLimits = account.rate_limit_attempts ?? 0;
  const modelBreakdown = buildModelCountBreakdown(account.success_model_counts, success);
  const errorBreakdown = buildErrorStatusBreakdown(account.error_status_counts, errors);
  const idle = success === 0 && errors === 0;
  const pad = compact ? "h-6 min-w-[2.25rem] px-2 text-[11px]" : "h-7 min-w-[2.5rem] px-2.5 text-[12px]";

  const successPill = (
    <span
      className={cn(
        "inline-flex items-center justify-center gap-1 rounded-full font-mono font-semibold tabular-nums",
        pad,
        success > 0
          ? "bg-emerald-500/12 text-emerald-700 transition-colors hover:bg-emerald-500/18 dark:bg-emerald-400/12 dark:text-emerald-300 dark:hover:bg-emerald-400/18"
          : "text-muted-foreground/70",
        success > 0 && "cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-400/50",
      )}
      title={success > 0 ? undefined : t("accounts.requestSuccess")}
      tabIndex={success > 0 ? 0 : undefined}
      aria-label={
        success > 0
          ? t("accounts.requestSuccessTooltipAria", { count: success })
          : undefined
      }
    >
      <span
        className={cn(
          "size-1.5 shrink-0 rounded-full",
          success > 0 ? "bg-emerald-500" : "bg-muted-foreground/30",
        )}
        aria-hidden
      />
      {success.toLocaleString()}
    </span>
  );

  return (
    <div className="flex flex-col items-start gap-1">
      <div
        className={cn(
          "inline-flex items-center rounded-full p-0.5 ring-1 ring-inset ring-border/70",
          idle ? "bg-muted/50" : "bg-muted/30",
        )}
      >
        {success > 0 ? (
          <CountBreakdownTooltip
            title={t("accounts.successModelTooltipTitle")}
            empty={t("accounts.successModelEmpty")}
            total={success}
            rows={modelBreakdown.map((row) => ({
              key: row.key,
              label: row.key === "unknown" ? t("accounts.unknownModel") : row.key,
              count: row.count,
              percent: row.percent,
            }))}
            showModelIcon
            barClassName="bg-gradient-to-r from-emerald-400 to-lime-300"
          >
            {successPill}
          </CountBreakdownTooltip>
        ) : (
          successPill
        )}

        {errors > 0 ? (
          <CountBreakdownTooltip
            title={t("accounts.errorStatusTooltipTitle")}
            empty={t("accounts.errorStatusEmpty")}
            total={errors}
            rows={errorBreakdown.map((row) => ({
              key: row.code,
              label: row.code,
              count: row.count,
              percent: row.percent,
            }))}
            barClassName="bg-gradient-to-r from-red-400 to-rose-300"
          >
            <span
              tabIndex={0}
              className={cn(
                "inline-flex cursor-help items-center justify-center gap-1 rounded-full bg-red-500/12 font-mono font-semibold tabular-nums text-red-700 transition-colors hover:bg-red-500/18 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-red-400/50 dark:bg-red-400/12 dark:text-red-300 dark:hover:bg-red-400/18",
                pad,
              )}
              aria-label={t("accounts.requestErrorTooltipAria", { count: errors })}
            >
              <span className="size-1.5 shrink-0 rounded-full bg-red-500" aria-hidden />
              {errors.toLocaleString()}
            </span>
          </CountBreakdownTooltip>
        ) : (
          <span
            className={cn(
              "inline-flex items-center justify-center gap-1 rounded-full font-mono font-semibold tabular-nums text-muted-foreground/70",
              pad,
            )}
            title={t("accounts.requestError")}
          >
            <span className="size-1.5 shrink-0 rounded-full bg-muted-foreground/30" aria-hidden />
            0
          </span>
        )}
      </div>

      {(retryErrors > 0 || rateLimits > 0) && (
        <div className="flex flex-wrap items-center gap-1 pl-0.5 text-[10px] font-medium text-muted-foreground">
          {retryErrors > 0 ? (
            <span className="rounded-full bg-muted/70 px-1.5 py-0.5">
              retry {retryErrors}
            </span>
          ) : null}
          {rateLimits > 0 ? (
            <span className="rounded-full bg-amber-500/10 px-1.5 py-0.5 text-amber-700 dark:text-amber-300">
              429 {rateLimits}
            </span>
          ) : null}
        </div>
      )}
    </div>
  );
}

export function CountBreakdownTooltip({
  title,
  empty,
  total,
  rows,
  barClassName,
  showModelIcon = false,
  tone = "default",
  children,
}: {
  title: string;
  empty: string;
  total: number;
  rows: Array<{
    key: string;
    label: string;
    count: number;
    percent: number;
    successRate?: number;
    avgFirstTokenMs?: number;
  }>;
  barClassName: string;
  showModelIcon?: boolean;
  tone?: "default" | "today";
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const isToday = tone === "today";
  return (
    <TooltipProvider delayDuration={120}>
      <Tooltip>
        <TooltipTrigger asChild>{children}</TooltipTrigger>
        <TooltipContent
          side="top"
          sideOffset={10}
          className={cn(
            "min-w-[240px] max-w-[320px] rounded-xl px-3.5 py-3 text-left text-xs shadow-2xl",
            isToday
              ? "min-w-[268px] max-w-[360px] border border-sky-200/25 bg-sky-950/80 text-sky-50 shadow-sky-950/25 backdrop-blur-2xl supports-[backdrop-filter]:bg-sky-950/40"
              : "border border-white/10 bg-zinc-950/95 text-zinc-50 backdrop-blur-md",
          )}
          arrowClassName={
            isToday
              ? "bg-sky-950/55 fill-sky-950/55 backdrop-blur-2xl"
              : "bg-zinc-950 fill-zinc-950"
          }
        >
          <div className="mb-2 flex items-baseline justify-between gap-3">
            <span
              className={cn(
                "text-[11px] font-semibold tracking-wide",
                isToday ? "text-sky-100/90" : "text-zinc-300",
              )}
            >
              {title}
            </span>
            <span
              className={cn(
                "font-mono text-[11px] tabular-nums",
                isToday ? "text-sky-200/55" : "text-zinc-500",
              )}
            >
              {total.toLocaleString()}
            </span>
          </div>
          {rows.length === 0 ? (
            <div className={isToday ? "text-sky-200/55" : "text-zinc-500"}>{empty}</div>
          ) : (
            <div className="space-y-2">
              {rows.map((row) => (
                <div key={row.key} className="space-y-1">
                  <div className="flex items-center justify-between gap-3 font-mono tabular-nums">
                    <span
                      className={cn(
                        "inline-flex min-w-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-semibold",
                        isToday ? "bg-white/12 text-sky-50" : "bg-white/8 text-zinc-100",
                      )}
                      title={row.label}
                    >
                      {showModelIcon ? <ModelNameIcon model={row.key} /> : null}
                      <span className="truncate">{row.label}</span>
                    </span>
                    <span className={cn("shrink-0", isToday ? "text-sky-50" : "text-zinc-200")}>
                      {row.count.toLocaleString()}
                    </span>
                    <span className={cn("shrink-0", isToday ? "text-sky-200/55" : "text-zinc-500")}>
                      {formatErrorStatusPercent(row.percent)}
                    </span>
                    {row.successRate != null ? (
                      <span
                        className="shrink-0 text-emerald-300"
                        title={t("accounts.todayModelSuccessRate")}
                      >
                        {t("accounts.todayModelSuccessRateShort", {
                          rate: formatErrorStatusPercent(row.successRate),
                        })}
                      </span>
                    ) : null}
                  </div>
                  {row.avgFirstTokenMs != null && row.avgFirstTokenMs > 0 ? (
                    <div className={cn("text-right text-[10px] tabular-nums", isToday ? "text-amber-200/85" : "text-zinc-400")}>
                      {t("accounts.todayModelAvgFirstToken")}: {formatAverageFirstTokenMs(row.avgFirstTokenMs)}
                    </div>
                  ) : null}
                  <div className={cn("h-1 overflow-hidden rounded-full", isToday ? "bg-white/12" : "bg-white/8")}>
                    <div
                      className={cn("h-full rounded-full", barClassName)}
                      style={{ width: `${Math.max(row.percent, 4)}%` }}
                    />
                  </div>
                </div>
              ))}
            </div>
          )}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

function formatAverageFirstTokenMs(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "-";
  if (value < 1000) return `${Math.round(value)}ms`;
  return `${(value / 1000).toFixed(value >= 10_000 ? 0 : 1)}s`;
}

function ModelNameIcon({ model }: { model: string }) {
  const icon = resolveLobeIconId(model);
  const invertMono = icon.id === "openai" || icon.id === "grok";
  return (
    <ModelLogo
      model={model}
      variant="plain"
      size={12}
      className={cn(invertMono && "[&_img]:invert [&_img]:opacity-90")}
      title=""
    />
  );
}
