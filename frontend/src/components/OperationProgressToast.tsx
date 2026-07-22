import { Check, Hourglass, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { OperationProgressState } from "../hooks/useOperationProgress";

// 批量操作进度浮层（右上角）。Codex / Grok 账号页共用，保证两处视觉一致。
export default function OperationProgressToast({
  progress,
  onClose,
}: {
  progress: OperationProgressState | null;
  onClose: () => void;
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
    progress.action === "batch_delete"
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
      : progress.action === "batch_refresh"
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
    <div className="fixed right-4 top-4 z-[80] w-[min(380px,calc(100vw-2rem))] rounded-lg border border-border bg-card/98 p-4 text-card-foreground shadow-xl backdrop-blur">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            {progress.done ? (
              <Check className="size-4 shrink-0 text-emerald-600 dark:text-emerald-400" />
            ) : (
              <Hourglass className="size-4 shrink-0 animate-pulse text-primary" />
            )}
            <div className="truncate text-sm font-semibold">{progress.title}</div>
          </div>
          <div className="mt-1 text-xs text-muted-foreground">
            {progress.done
              ? t("accounts.operationProgressDone")
              : t("accounts.operationProgressRunning")}
            {" · "}
            {progress.current}/{progress.total || 0}
          </div>
        </div>
        <button
          type="button"
          className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
          onClick={onClose}
          aria-label={t("common.close")}
        >
          <X className="size-4" />
        </button>
      </div>

      <div className="mt-3 h-2 overflow-hidden rounded-full bg-muted">
        <div
          className={`h-full rounded-full transition-all duration-300 ease-out ${
            progress.done ? "bg-emerald-500" : "bg-primary"
          }`}
          style={{ width: `${percent}%` }}
        />
      </div>

      <div className="mt-3 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
        {metrics.map((metric) => (
          <div key={metric.label} className="rounded-md bg-muted/40 px-2 py-1.5">
            <div className="text-muted-foreground">{metric.label}</div>
            <div className={`mt-0.5 font-semibold ${metric.tone}`}>
              {metric.value}
            </div>
          </div>
        ))}
      </div>

      {progress.message ? (
        <div className="mt-3 max-h-10 overflow-hidden break-words text-xs text-muted-foreground">
          {progress.message}
        </div>
      ) : null}
    </div>
  );
}
