import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { CloudOff } from "lucide-react";
import { cn } from "@/lib/utils";
import { formatBeijingTime } from "../utils/time";
import type { SubscriptionStatus } from "../types";

type Tone = "neutral" | "amber" | "orange" | "red" | "blue" | "gray";

const TONE_CLASS: Record<Tone, string> = {
  neutral:
    "bg-emerald-50 text-emerald-700 ring-emerald-500/20 dark:bg-emerald-500/10 dark:text-emerald-300 dark:ring-emerald-400/20",
  amber:
    "bg-amber-100 text-amber-700 ring-amber-500/30 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/30",
  orange:
    "bg-orange-100 text-orange-700 ring-orange-500/30 dark:bg-orange-500/20 dark:text-orange-300 dark:ring-orange-400/30",
  red: "bg-red-100 text-red-700 ring-red-500/30 dark:bg-red-500/20 dark:text-red-300 dark:ring-red-400/30",
  blue: "bg-sky-100 text-sky-700 ring-sky-500/30 dark:bg-sky-500/20 dark:text-sky-300 dark:ring-sky-400/30",
  gray: "bg-zinc-200 text-zinc-700 ring-zinc-400/30 dark:bg-zinc-700/50 dark:text-zinc-300 dark:ring-zinc-500/30",
};

/** 显示时区的 YYYY-MM-DD(徽章里只放日期,精确时刻放悬停提示)。 */
export function formatDate(iso: string | undefined): string {
  if (!iso) return "-";
  if (Number.isNaN(Date.parse(iso))) return "-";
  return formatBeijingTime(iso).slice(0, 10);
}

/** 决定徽章文案与颜色;返回 null 表示这条账号不需要展示徽章。 */
export function describeSubscription(
  sub: SubscriptionStatus | undefined,
  t: (key: string, opts?: Record<string, unknown>) => string,
  showUnknown: boolean,
): { label: string; tone: Tone; emphasis?: boolean } | null {
  if (!sub) return null;
  const pendingRenewal =
    sub.sync_state === "pending" && !!sub.renewal_detected_at;
  switch (sub.business_status) {
    case "active": {
      const days = sub.days_remaining;
      return {
        label: t("accounts.subscriptionStatusActive", {
          date: formatDate(sub.expires_at),
        }),
        tone: days <= 3 ? "red" : days <= 9 ? "amber" : "neutral",
      };
    }
    case "expiring_today":
      return {
        label: t("accounts.subscriptionStatusExpiringToday"),
        tone: "orange",
        emphasis: true,
      };
    case "expired":
      return {
        label: t("accounts.subscriptionStatusExpired", {
          days: sub.days_overdue,
        }),
        tone: "red",
        emphasis: true,
      };
    case "grace_period":
      return {
        label: t("accounts.subscriptionStatusGrace", {
          days: sub.days_overdue,
        }),
        tone: "orange",
      };
    default:
      if (pendingRenewal) {
        return {
          label: t("accounts.subscriptionStatusPendingRenewal"),
          tone: "blue",
        };
      }
      if (sub.sync_state === "pending") {
        return { label: t("accounts.subscriptionStatusPending"), tone: "blue" };
      }
      // 从未拿到任何到期信息:只在能手动刷新的页面给一个入口,别在 Claude 等无数据源的页面刷一排"未知"。
      if (!showUnknown) return null;
      return { label: t("accounts.subscriptionStatusUnknown"), tone: "gray" };
  }
}

export function SubscriptionStatusChip({
  subscription,
  showUnknown = false,
  title: tooltip,
  className,
  children,
}: {
  subscription?: SubscriptionStatus;
  showUnknown?: boolean;
  title?: string;
  className?: string;
  children?: ReactNode;
}) {
  const { t } = useTranslation();
  const described = describeSubscription(subscription, t, showUnknown);
  if (!subscription || !described) return null;
  const stale = subscription.sync_state === "failed";
  return (
    <span
      className={cn(
        "inline-flex max-w-full items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] ring-1 ring-inset",
        described.emphasis ? "font-semibold" : "font-medium",
        TONE_CLASS[described.tone],
        className,
      )}
      title={tooltip}
      data-subscription-status={subscription.business_status}
      data-subscription-sync={subscription.sync_state}
    >
      {stale && (
        <CloudOff
          className="size-3 shrink-0 opacity-60"
          aria-label={t("accounts.subscriptionStale")}
        />
      )}
      <span className="truncate">{described.label}</span>
      {stale && (
        <span className="sr-only">{t("accounts.subscriptionStale")}</span>
      )}
      {children}
    </span>
  );
}
