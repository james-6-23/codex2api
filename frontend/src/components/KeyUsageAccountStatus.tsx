import { useTranslation } from "react-i18next";
import type { PublicKeyUsageAccount } from "../api";
import { formatLongUsageWindowLabel } from "../lib/usageFormat";
import { CooldownTimer, UsingCreditsBadge } from "./AccountStatusIndicators";
import StatusBadge from "./StatusBadge";
import { SubscriptionStatusChip } from "./SubscriptionStatusChip";
import { formatBeijingTime } from "../utils/time";

export function KeyUsageSubscription({
  account,
}: {
  account: PublicKeyUsageAccount;
}) {
  const { t } = useTranslation();
  const sub = account.subscription;
  if (!sub) return null;
  const expires = formatBeijingTime(sub.expires_at, "");
  return (
    <SubscriptionStatusChip
      subscription={sub}
      showUnknown
      title={
        expires
          ? t("accounts.subscriptionTipExpiresAt", { value: expires })
          : undefined
      }
    />
  );
}

export function KeyUsageAccountStatus({
  account,
}: {
  account: PublicKeyUsageAccount;
}) {
  const { t } = useTranslation();
  const reason = account.limit_reason;
  const long = formatLongUsageWindowLabel({
    usage_window_7d_seconds: account.long_window_seconds,
  });
  // Treat reason as an enum, not text: never render an unexpected provider message.
  let detail: string | undefined;
  switch (reason) {
    case "window_5h":
    case "quota_paused_5h":
      detail = "5h";
      break;
    case "window_7d":
    case "quota_paused_7d":
      detail = long;
      break;
    case "cooldown":
    case "quota_paused":
    case "rate_limited":
      if (reason !== account.status) detail = t(`status.${reason}`);
      break;
  }
  const status =
    account.status === "rate_limited_5h" ||
    account.status === "rate_limited_7d" ||
    account.status === "upstream_429" ||
    account.status === "usage_limit"
      ? "rate_limited"
      : account.status;
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <StatusBadge status={status} detail={detail} />
      <UsingCreditsBadge account={account} />
      {account.recovery_at && <CooldownTimer until={account.recovery_at} />}
    </div>
  );
}
