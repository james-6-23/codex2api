import { useState } from "react";
import AccountResetCreditBadge from "./AccountResetCreditBadge";
import KeyUsageReauthDialog from "./KeyUsageReauthDialog";
import {
  AccountDetailContent,
  AccountDetailBody,
  AccountDetailSection,
  AccountDetailMetric,
} from "./AccountDetailContent";
import { useTranslation } from "react-i18next";
import {
  Clock3,
  RotateCcw,
  Eye,
  Activity,
  Wallet,
  FolderOpen,
  Coins,
  Layers,
} from "lucide-react";
import type { PublicKeyUsageAccount } from "../api";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  AccountCardSurface,
  AccountCardHeader,
  AccountCardBody,
  AccountCardMetric,
  AccountCardTimestamp,
} from "./AccountCardSurface";
import { Sheet, SheetDescription, SheetTrigger } from "@/components/ui/sheet";
import { formatBeijingTime } from "../utils/time";
import AccountUsageBar from "./AccountUsageBar";
import PlanBadge from "./AccountPlanBadge";
import StatusBadge from "./StatusBadge";
import { formatLongUsageWindowLabel } from "../lib/usageFormat";

export default function KeyUsageAccountCard({
  account,
  sequence,
  groups,
  busy,
  pending,
  onRedeem,
  apiKey,
  onRefresh,
}: {
  account: PublicKeyUsageAccount;
  sequence: number;
  groups: { id: number; name: string }[];
  busy: boolean;
  pending: boolean;
  onRedeem: () => void;
  apiKey: string;
  onRefresh: () => void;
}) {
  const { t } = useTranslation();
  const [detailsOpen, setDetailsOpen] = useState(false);
  const displayName = account.email || account.name || `#${account.id}`;
  const updated = [account.usage_updated_at_5h, account.usage_updated_at_7d]
    .filter(
      (value): value is string => !!value && Number.isFinite(Date.parse(value)),
    )
    .sort((a, b) => Date.parse(b) - Date.parse(a))[0];
  const credit =
    account.credits_valid &&
    (account.credits_unlimited
      ? "∞"
      : (account.credits_balance ??
        (account.credits_has_credits ? "✓" : undefined)));
  const healthLabels: Record<string, string> = {
    healthy: "accounts.healthy",
    warm: "accounts.warm",
    risky: "accounts.risky",
    banned: "accounts.quarantine",
  };
  const windowPair = (short?: number, week?: number, currency = false) => {
    const format = (value?: number) =>
      value == null
        ? "—"
        : currency
          ? `$${value.toFixed(2)}`
          : value.toLocaleString();
    return (
      <span className="inline-flex flex-wrap gap-x-2 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[11px] tabular-nums">
        <span>5h: {format(short)}</span>
        <span>7d: {format(week)}</span>
      </span>
    );
  };
  return (
    <AccountCardSurface
      data-state={account.status}
      aria-label={account.name || displayName}
    >
      <AccountCardHeader
        identity={
          <>
            <div className="codex-account-card__avatar" aria-hidden="true">
              {displayName[0].toUpperCase()}
            </div>
            <div className="min-w-0 flex-1">
              <div
                className="codex-account-card__name truncate"
                title={displayName}
              >
                {displayName}
              </div>
              {account.name && account.name !== displayName && (
                <p
                  className="mt-1 truncate text-xs text-muted-foreground"
                  title={account.name}
                >
                  {account.name}
                </p>
              )}
            </div>
            <span className="codex-account-card__sequence">#{sequence}</span>
            <span className="codex-account-card__sequence">
              {t("keyUsageAccounts.idLabel", { id: account.id })}
            </span>
          </>
        }
        topline={
          <>
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <PlanBadge planType={account.plan_type} />
              <AccountResetCreditBadge count={account.reset_credits} onClick={() => setDetailsOpen(true)} />
              {credit && (
                <span
                  className="codex-account-card__flag"
                  title={t("accounts.creditsBalanceLabel")}
                >
                  <Coins className="size-3" />
                  {credit}
                </span>
              )}
              {account.active_requests != null && (
                <span
                  className="codex-account-card__flag"
                  title={t("accounts.concurrency")}
                >
                  <Layers className="size-3" />
                  {account.active_requests} /{" "}
                  {account.dynamic_concurrency_limit ?? "—"}
                </span>
              )}
            </div>
            <StatusBadge status={account.status} />
          </>
        }
      />
      <AccountCardBody
        usage={
          <>
            <div className="account-usage-cell">
              <div className="account-usage-windows">
                <AccountUsageBar
                  label="5h"
                  pct={account.usage_percent_5h}
                  resetAt={account.reset_5h_at}
                />
                <AccountUsageBar
                  label={formatLongUsageWindowLabel({
                    usage_window_7d_seconds: account.long_window_seconds,
                  })}
                  pct={account.usage_percent_7d}
                  resetAt={account.reset_7d_at}
                />
              </div>
            </div>
            {account.health_tier && (
              <div className="codex-account-card__health flex items-center justify-between text-[11px] text-muted-foreground">
                <span>{t("accounts.healthBarLabel")}</span>
                <span
                  className="codex-account-card__health-tier"
                  data-health={account.health_tier}
                >
                  {t(healthLabels[account.health_tier] ?? "accounts.unknown")}
                </span>
              </div>
            )}
          </>
        }
        metrics={
          <>
            <AccountCardMetric
              label={t("keyUsageAccounts.keyRequests")}
              icon={<Activity className="size-3.5" />}
            >
              {windowPair(account.requests_5h, account.requests_7d)}
            </AccountCardMetric>
            <AccountCardMetric
              label={t("keyUsageAccounts.keyCost")}
              icon={<Wallet className="size-3.5" />}
            >
              {windowPair(account.user_billed_5h, account.user_billed_7d, true)}
            </AccountCardMetric>
          </>
        }
      />
      <div className="codex-account-card__metadata">
        <div className="codex-account-card__tags">
          {account.group_ids.length ? (
            account.group_ids.map((id) => (
              <Badge key={id} variant="secondary">
                {groups.find((item) => item.id === id)?.name ||
                  t("keyUsageAccounts.groupLabel", { id })}
              </Badge>
            ))
          ) : (
            <Badge variant="outline">{t("keyUsageAccounts.ungrouped")}</Badge>
          )}
        </div>
      </div>
      <dl className="codex-account-card__timestamps">
        <AccountCardTimestamp
          icon={<Clock3 className="size-3" />}
          label={t("accounts.usageUpdatedAtShort")}
          value={updated}
          fallback={t("accounts.noUsageUpdatedAt")}
        />
        {account.last_used_at && (
          <AccountCardTimestamp
            icon={<Activity className="size-3" />}
            label={t("keyUsageAccounts.lastActive")}
            value={account.last_used_at}
          />
        )}
        <AccountCardTimestamp
          icon={<FolderOpen className="size-3" />}
          label={t("accounts.importTime")}
          value={account.created_at}
          dateOnly
        />
      </dl>
      <footer className="codex-account-card__actions codex-account-card__actions--portal">
        <Sheet open={detailsOpen} onOpenChange={setDetailsOpen}>
          <SheetTrigger asChild>
            <Button
              variant="ghost"
              size="sm"
              className="codex-account-card__action"
            >
              <Eye className="size-3.5" />
              <span>{t("accounts.cardDetails")}</span>
            </Button>
          </SheetTrigger>
          <AccountDetailContent
            id={account.id}
            name={displayName}
            plan={account.plan_type}
            sequence={sequence}
            description={
              <SheetDescription>
                {t("keyUsageAccounts.accountScope")}
              </SheetDescription>
            }
          >
            <AccountDetailBody>
              <AccountDetailSection title={t("accounts.status")}>
                <StatusBadge status={account.status} />
              </AccountDetailSection>
              <AccountDetailSection title={t("accounts.usage")}>
                {[
                  {
                    label: "5h",
                    pct: account.usage_percent_5h,
                    reset: account.reset_5h_at,
                    update: account.usage_updated_at_5h,
                  },
                  {
                    label: formatLongUsageWindowLabel({
                      usage_window_7d_seconds: account.long_window_seconds,
                    }),
                    pct: account.usage_percent_7d,
                    reset: account.reset_7d_at,
                    update: account.usage_updated_at_7d,
                  },
                ].map((row, index) => (
                  <section
                    key={index}
                    className="space-y-2 rounded-xl border p-3"
                  >
                    <AccountUsageBar label={row.label} pct={row.pct} />
                    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
                      <dt>{t("keyUsageAccounts.resetTime")}</dt>
                      <dd>{formatBeijingTime(row.reset, "—")}</dd>
                      <dt>{t("keyUsageAccounts.updated")}</dt>
                      <dd>{formatBeijingTime(row.update, "—")}</dd>
                    </dl>
                  </section>
                ))}
              </AccountDetailSection>
              <AccountDetailSection title={t("accounts.detailMetrics")}>
                <div className="grid grid-cols-2 gap-2">
                  <AccountDetailMetric
                    label={t("keyUsageAccounts.keyRequests")}
                  >
                    {windowPair(account.requests_5h, account.requests_7d)}
                  </AccountDetailMetric>
                  <AccountDetailMetric label={t("keyUsageAccounts.keyCost")}>
                    {windowPair(
                      account.user_billed_5h,
                      account.user_billed_7d,
                      true,
                    )}
                  </AccountDetailMetric>
                  <AccountDetailMetric label={t("keyUsageAccounts.cards")}>
                    {account.reset_credits ?? t("keyUsageAccounts.unknown")}
                  </AccountDetailMetric>
                </div>
              </AccountDetailSection>
            </AccountDetailBody>
          </AccountDetailContent>
        </Sheet>
        {account.reset_allowed && (
          <Button
            variant="ghost"
            size="sm"
            className="codex-account-card__action"
            onClick={onRedeem}
            disabled={busy || account.reset_credits === 0}
          >
            <RotateCcw
              className={`size-3.5 ${pending ? "animate-spin" : ""}`}
            />
            {t(
              pending ? "keyUsageAccounts.pending" : "keyUsageAccounts.redeem",
            )}
          </Button>
        )}
        {account.reauth_allowed && (
          <KeyUsageReauthDialog
            account={account}
            apiKey={apiKey}
            onComplete={onRefresh}
          />
        )}
      </footer>
    </AccountCardSurface>
  );
}
