import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { Badge } from "./ui/badge";

const REFRESH_INTERVAL_MS = 5_000;

export function useAPIKeyConcurrency() {
  const [counts, setCounts] = useState<Record<string, number> | null>(null);
  useEffect(() => {
    let cancelled = false;
    let pending = false;
    const refresh = async () => {
      if (pending || document.hidden) return;
      pending = true;
      try {
        const response = await api.getAPIKeyConcurrency();
        if (!cancelled) setCounts(response.concurrency);
      } catch {
        // 暂时读取失败时保留上次快照，下一次轮询重试。
      } finally {
        pending = false;
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), REFRESH_INTERVAL_MS);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, []);
  return counts;
}

export default function APIKeyConcurrencyBadge({ current, limit }: {
  current?: number;
  limit?: number;
}) {
  const { t } = useTranslation();
  if (current === undefined || current <= 0) return null;

  return (
    <Badge
      variant="outline"
      title={t("apiKeys.limits.concurrencySuffix")}
      className="gap-1.5 rounded-full border-blue-500/20 bg-blue-50 px-2 py-0.5 text-[11px] font-semibold tabular-nums text-blue-600 shadow-2xs dark:border-blue-400/20 dark:bg-blue-950 dark:text-blue-400"
    >
      <span
        className="size-1.5 animate-pulse rounded-full bg-blue-500 dark:bg-blue-400"
        aria-hidden
      />
      {current}/{limit && limit > 0 ? limit : "∞"}
    </Badge>
  );
}
