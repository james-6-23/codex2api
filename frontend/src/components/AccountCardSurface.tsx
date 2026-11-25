import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import { Gauge, ExternalLink } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { ComponentProps, ReactNode } from "react";
import { cn } from "@/lib/utils";
import "./account-cards.css";

// Shared by management cards and the read-only API key account portal. Actions
// are supplied by each caller; this surface never invokes a management API.
export function AccountCardSurface({
  className,
  ...props
}: ComponentProps<"article">) {
  return <article className={cn("codex-account-card", className)} {...props} />;
}

export function AccountCardHeader({
  identity,
  topline,
}: {
  identity: ReactNode;
  topline: ReactNode;
}) {
  return (
    <header className="codex-account-card__header">
      <div className="codex-account-card__identity">{identity}</div>
      <div className="codex-account-card__topline">{topline}</div>
    </header>
  );
}

export function AccountCardBody({
  usage,
  usageAction,
  metrics,
}: {
  usage?: ReactNode;
  usageAction?: ReactNode;
  metrics?: ReactNode;
}) {
  const { t } = useTranslation();
  if (!usage && !metrics) return null;
  return (
    <div className="codex-account-card__body">
      {usage && (
        <section
          className="codex-account-card__usage"
          aria-label={t("accounts.cardUsageTitle")}
        >
          <div className="codex-account-card__section-title">
            <span className="inline-flex items-center gap-1.5 font-medium">
              <Gauge className="size-3.5 text-primary/80" />
              {t("accounts.cardUsageTitle")}
            </span>
            {usageAction}
          </div>
          {usage}
        </section>
      )}
      {metrics && (
        <div className="codex-account-card__metrics-slot">
          <div className="codex-account-card__metrics">{metrics}</div>
        </div>
      )}
    </div>
  );
}

export function AccountCardDetailLink(props: ComponentProps<"button">) {
  const { t } = useTranslation();
  return (
    <button type="button" className="codex-account-card__usage-link" {...props}>
      {t("accounts.cardDetails")}
      <ExternalLink className="size-3" />
    </button>
  );
}

export function AccountCardMetric({
  label,
  icon,
  children,
}: {
  label: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="codex-account-card__metric">
      <div className="codex-account-card__metric-label">
        {icon}
        <span>{label}</span>
      </div>
      <div className="codex-account-card__metric-value">{children}</div>
    </div>
  );
}

export function AccountCardTimestamp({
  label,
  icon,
  value,
  dateOnly,
  fallback,
}: {
  label: string;
  icon: ReactNode;
  value?: string;
  dateOnly?: boolean;
  fallback?: string;
}) {
  const full = value && formatBeijingTime(value, "");
  return (
    <div>
      <dt>
        {icon}
        <span>{label}</span>
      </dt>
      <dd>
        {value && full ? (
          <time dateTime={value} title={full}>
            {dateOnly ? full.slice(0, 10) : formatRelativeTime(value)}
          </time>
        ) : (
          (fallback ?? "—")
        )}
      </dd>
    </div>
  );
}
