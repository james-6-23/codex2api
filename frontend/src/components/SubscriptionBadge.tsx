import { SubscriptionStatusChip, formatDate } from "./SubscriptionStatusChip";
export { describeSubscription } from "./SubscriptionStatusChip";
import { useEffect, useState, type MouseEvent } from "react";
import { getTimezone } from "../utils/time";
import { useTranslation } from "react-i18next";
import { RefreshCw } from "lucide-react";
import { api, AdminAPIError } from "../api";
import { useToast } from "../hooks/useToast";
import { cn } from "@/lib/utils";
import type { SubscriptionRefreshResponse, SubscriptionStatus } from "../types";

// SubscriptionBadge 订阅状态徽章(Codex/Claude 账号页共用)。
// 只吃服务端按业务时区算好的 subscription 对象,不再在本机按 24h 粒度推算
// "今天到期/已过期",避免前后端时区不一致。业务状态决定文案与颜色;同步状态
// (failed/pending)只叠加标记,不改变文案含义——所有状态都有文字,不只靠颜色。
//
// 展示规则:
//   active          到期/续费日期(>9 天绿、4~9 天黄、≤3 天红)
//   expiring_today  今日到期(橙)
//   expired         已超时 N 天(红)
//   grace_period    宽限期 · 已超时 N 天(橙)
//   unknown+pending 已续费 · 待确认(蓝;陈旧到期时间刚被清理、等权威同步)
//   unknown         状态未知(灰;仅 canRefresh 时展示,给用户一个手动查询入口)
//   sync=failed     徽章前置一个弱化的"云断开"小图标(不用警示三角,避免误读成账号异常)+ 悬停"数据可能已过期"

export interface SubscriptionBadgeProps {
  accountId: number;
  subscription?: SubscriptionStatus;
  /** 是否提供"刷新订阅状态"按钮(只有 Codex/ChatGPT 账号有订阅提供方可查)。 */
  canRefresh?: boolean;
  className?: string;
}

function formatDateTime(iso: string | undefined, locale: string): string {
  if (!iso) return "-";
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) return iso;
  return new Date(ts).toLocaleString(locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    timeZone: getTimezone(),
  });
}

function sourceLabel(source: string | undefined, t: (key: string) => string): string {
  switch (source) {
    case "provider_api":
      return t("accounts.subscriptionSourceProviderApi");
    case "plan_header":
      return t("accounts.subscriptionSourcePlanHeader");
    case "jwt":
      return t("accounts.subscriptionSourceJwt");
    default:
      return source || "-";
  }
}

function enumLabel(prefix: string, value: string | undefined, t: (key: string) => string): string {
  if (!value) return "-";
  const key = `accounts.${prefix}${value.charAt(0).toUpperCase()}${value.slice(1)}`;
  const label = t(key);
  return label === key ? value : label;
}

function statusLabel(sub: SubscriptionStatus, status: string | undefined, t: (key: string, opts?: Record<string, unknown>) => string): string {
  switch (status) {
    case "active":
      return t("accounts.subscriptionStatusActive", { date: formatDate(sub.expires_at) });
    case "expiring_today":
      return t("accounts.subscriptionStatusExpiringToday");
    case "expired":
      return t("accounts.subscriptionStatusExpired", { days: sub.days_overdue });
    case "grace_period":
      return t("accounts.subscriptionStatusGrace", { days: sub.days_overdue });
    default:
      return t("accounts.subscriptionStatusUnknown");
  }
}

export function buildSubscriptionTooltip(
  sub: SubscriptionStatus,
  t: (key: string, opts?: Record<string, unknown>) => string,
  locale: string,
): string {
  const lines: string[] = [];
  if (sub.expires_at) lines.push(t("accounts.subscriptionTipExpiresAt", { value: formatDateTime(sub.expires_at, locale) }));
  if (sub.grace_until) lines.push(t("accounts.subscriptionTipGraceUntil", { value: formatDateTime(sub.grace_until, locale) }));
  lines.push(
    sub.last_checked_at
      ? t("accounts.subscriptionTipLastChecked", { value: formatDateTime(sub.last_checked_at, locale) })
      : t("accounts.subscriptionTipNeverChecked"),
  );
  lines.push(t("accounts.subscriptionTipSync", { value: enumLabel("subscriptionSync", sub.sync_state, t) }));
  if (sub.source) lines.push(t("accounts.subscriptionTipSource", { value: sourceLabel(sub.source, t) }));
  lines.push(t("accounts.subscriptionTipAutoRenew", { value: enumLabel("subscriptionAutoRenew", sub.auto_renew, t) }));
  if (sub.business_status === "unknown" && sub.last_known_status) {
    lines.push(t("accounts.subscriptionTipLastKnown", { value: statusLabel(sub, sub.last_known_status, t) }));
  }
  if (sub.renewal_detected_at) {
    lines.push(t("accounts.subscriptionTipRenewalDetected", { value: formatDateTime(sub.renewal_detected_at, locale) }));
  }
  if (sub.error) {
    lines.push(
      t("accounts.subscriptionTipError", {
        value: sub.error.includes("anti-bot challenge") ? t("accounts.subscriptionRefreshBlocked") : sub.error,
      }),
    );
  }
  if (sub.timezone) lines.push(t("accounts.subscriptionTipTimezone", { value: sub.timezone }));
  return lines.join("\n");
}

export default function SubscriptionBadge({ accountId, subscription, canRefresh = false, className }: SubscriptionBadgeProps) {
  const { t, i18n } = useTranslation();
  const { showToast } = useToast();
  // 刷新结果先落在本地覆盖层:列表整体重载前也能立刻看到新状态;父级数据更新后以父级为准。
  const [override, setOverride] = useState<SubscriptionStatus | undefined>(undefined);
  const [refreshing, setRefreshing] = useState(false);
  useEffect(() => {
    setOverride(undefined);
  }, [subscription]);

  const sub = override ?? subscription;
  if (!sub) return null;
  const tooltip = buildSubscriptionTooltip(sub, t, i18n.language);

  const handleRefresh = async (event: MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    event.stopPropagation();
    if (refreshing) return;
    setRefreshing(true);
    try {
      const res: SubscriptionRefreshResponse = await api.refreshAccountSubscription(accountId);
      if (res.subscription) setOverride(res.subscription);
      switch (res.outcome) {
        case "updated":
          showToast(
            t("accounts.subscriptionRefreshUpdated", {
              date: formatDateTime(res.subscription?.expires_at ?? res.subscription_expires_at, i18n.language),
            }),
            "success",
          );
          break;
        case "unchanged":
          showToast(t("accounts.subscriptionRefreshUnchanged"), "info");
          break;
        case "no_subscription":
          showToast(t("accounts.subscriptionRefreshNoSubscription"), "info");
          break;
        case "unsupported":
          showToast(t("accounts.subscriptionRefreshUnsupported"), "info");
          break;
        default:
          if (res.error && res.error.includes("anti-bot challenge")) {
            showToast(t("accounts.subscriptionRefreshBlocked"), "warning", 8000);
          } else {
            showToast(t("accounts.subscriptionRefreshFailed", { error: res.error || res.outcome }), "error");
          }
      }
    } catch (err) {
      if (err instanceof AdminAPIError && err.status === 429) {
        const match = /(\d+)/.exec(err.message);
        showToast(t("accounts.subscriptionRefreshRateLimited", { seconds: match ? match[1] : 30 }), "info");
      } else {
        showToast(t("accounts.subscriptionRefreshFailed", { error: err instanceof Error ? err.message : String(err) }), "error");
      }
    } finally {
      setRefreshing(false);
    }
  };

  return (
    <SubscriptionStatusChip subscription={sub} showUnknown={canRefresh} title={tooltip} className={className}>
      {canRefresh && (
        <button
          type="button"
          onClick={handleRefresh}
          disabled={refreshing}
          className="ml-0.5 inline-flex shrink-0 items-center rounded p-0.5 opacity-70 transition hover:opacity-100 focus-visible:opacity-100 disabled:cursor-wait"
          title={refreshing ? t("accounts.subscriptionRefreshing") : t("accounts.subscriptionRefresh")}
          aria-label={t("accounts.subscriptionRefresh")}
        >
          <RefreshCw className={cn("size-3", refreshing && "animate-spin")} />
        </button>
      )}
    </SubscriptionStatusChip>
  );
}
