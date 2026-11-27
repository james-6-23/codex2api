import { RotateCcw } from "lucide-react";
import { useTranslation } from "react-i18next";

/** Opens a read-only usage view; redeeming a credit is a separate operation. */
export default function AccountResetCreditBadge({
  count,
  onClick,
}: {
  count?: number;
  onClick: () => void;
}) {
  const { t } = useTranslation();
  if (count == null || count <= 0) return null;
  const label = t("accounts.resetCreditsBadge", { count });
  return (
    <button
      type="button"
      className="codex-account-card__flag codex-account-card__flag--clickable"
      onClick={onClick}
      title={label}
      aria-label={label}
    >
      <RotateCcw className="size-3" />
      {count}
    </button>
  );
}
