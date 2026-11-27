import { Check, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { OperationProgressState } from "../hooks/useOperationProgress";
import { PixelLoader, ShimmerText } from "@/components/ui/beautiful";

// 批量操作进度浮层（右上角）。Codex / Grok 账号页共用，保证两处视觉一致。
export default function OperationProgressToast({
  progress,
  onClose,
  closable = true,
}: {
  progress: OperationProgressState | null;
  onClose: () => void;
  // 后台导入进行中不提供关闭，避免误以为任务已经停掉。
  closable?: boolean;
}) {
  const { t } = useTranslation();
  if (!progress?.show) return null;

  const percent =
    progress.total > 0
      ? Math.min(
          100,
          Math.max(0, Math.round((progress.current / progress.total) * 100)),
        )
      : 0;
  const metrics =
    progress.action === "batch_delete" || progress.action === "clean"
      ? [
          {
            label: t("accounts.operationProgressDeleted"),
            value: progress.deleted || progress.success,
            tone: "text-emerald-600 dark:text-emerald-400",
          },
          {
            label: t("accounts.operationProgressFailed"),
            value: progress.failed,
            tone: "text-red-600 dark:text-red-400",
          },
        ]
      : progress.action === "batch_refresh" || progress.action === "grok_import"
        ? [
            {
              label: t("accounts.operationProgressSuccess"),
              value: progress.success,
              tone: "text-emerald-600 dark:text-emerald-400",
            },
            {
              label: t("accounts.operationProgressFailed"),
              value: progress.failed,
              tone: "text-red-600 dark:text-red-400",
            },
          ]
        : [
            {
              label: t("accounts.operationProgressSuccess"),
              value: progress.success,
              tone: "text-emerald-600 dark:text-emerald-400",
            },
            {
              label: t("accounts.operationProgressBanned"),
              value: progress.banned,
              tone: "text-red-600 dark:text-red-400",
            },
            {
              label: t("accounts.operationProgressRateLimited"),
              value: progress.rateLimited,
              tone: "text-amber-600 dark:text-amber-400",
            },
            {
              label: t("accounts.operationProgressFailed"),
              value: progress.failed,
              tone: "text-red-600 dark:text-red-400",
            },
          ];

  return (
    <div
      className="fixed right-4 top-4 z-[80] w-[min(392px,calc(100vw-2rem))] rounded-2xl border border-border/80 bg-card/98 p-4 text-card-foreground backdrop-blur-md dark:border-white/12"
      style={{ boxShadow: "var(--bui-shadow-overlay)" }}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            {progress.done ? (
              <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-emerald-500/15 text-emerald-600 ring-1 ring-inset ring-emerald-500/25 dark:text-emerald-400">
                <Check className="size-3 stroke-[2.5]" />
              </span>
            ) : (
              <PixelLoader mode="orbit" size="sm" />
            )}
            <div className="truncate text-sm font-semibold">
              {progress.done ? progress.title : <ShimmerText>{progress.title}</ShimmerText>}
            </div>
          </div>
          <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
            <span>
              {progress.done
                ? t("accounts.operationProgressDone")
                : t("accounts.operationProgressRunning")}
            </span>
            <span className="text-border">·</span>
            <span className="font-mono tabular-nums text-foreground/85">
              {progress.current}/{progress.total || 0}
            </span>
            <span className="font-mono text-[11px] tabular-nums text-muted-foreground/80">
              ({percent}%)
            </span>
          </div>
        </div>
        {closable ? (
          <button
            type="button"
            className="rounded-lg p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            onClick={onClose}
            aria-label={t("common.close")}
          >
            <X className="size-4" />
          </button>
        ) : (
          <span className="size-6 shrink-0" />
        )}
      </div>

      <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-muted/80">
        <div
          className={`h-full rounded-full transition-all duration-300 ease-[cubic-bezier(0.23,1,0.32,1)] ${
            progress.done ? "bg-emerald-500" : "bg-primary"
          }`}
          style={{ width: `${percent}%` }}
        />
      </div>

      <div className="mt-3 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
        {metrics.map((metric) => (
          <div
            key={metric.label}
            className="rounded-lg border border-border/60 bg-muted/35 px-2.5 py-1.5 dark:border-white/[0.06] dark:bg-white/[0.03]"
          >
            <div className="truncate text-[11px] text-muted-foreground">{metric.label}</div>
            <div className={`mt-0.5 font-mono text-sm font-semibold tabular-nums ${metric.tone}`}>
              {metric.value}
            </div>
          </div>
        ))}
      </div>

      {progress.message ? (
        <div className="mt-3 max-h-10 overflow-hidden break-words rounded-lg border border-border/50 bg-muted/25 px-2.5 py-1.5 font-mono text-[11px] text-muted-foreground">
          {progress.message}
        </div>
      ) : null}
    </div>
  );
}
