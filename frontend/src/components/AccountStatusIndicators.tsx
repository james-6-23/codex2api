import { useEffect, useState } from "react";
import { Coins, Hourglass } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatBeijingTime } from "../utils/time";

function useCountdownRemaining(until: string): string {
  const target = Date.parse(until);
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const start = Date.now();
    setNow(start);
    if (!Number.isFinite(target) || target <= start) return;
    const tick = () => {
      const next = Date.now();
      setNow(next);
      if (next >= target) clearInterval(id);
    };
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [target]);
  const diff = Math.ceil((target - now) / 1000);
  if (!Number.isFinite(diff) || diff <= 0) return "";
  const hours = Math.floor(diff / 3600);
  const minutes = Math.floor((diff % 3600) / 60);
  const seconds = diff % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  if (hours) return `${hours}h ${pad(minutes)}m ${pad(seconds)}s`;
  if (minutes) return `${minutes}m ${pad(seconds)}s`;
  return `${seconds}s`;
}

// UsingCreditsBadge 紧跟在限流徽章后面：账号显示仍是「限流」（用量窗口客观上确实打满了），
// 这个积分徽章表示"正在用积分顶替限流，因此仍在参与调度"。
// 后端 using_credits 已经算过全部前提（两个开关 + 当下确有余额 + 窗口真打满），
// 这里只负责显示，不重算，避免两边判定漂移。
export function UsingCreditsBadge({
  account,
}: {
  account: { using_credits?: boolean };
}) {
  const { t } = useTranslation();
  if (!account.using_credits) return null;
  // 纯图标：余额已经在邮箱下方的徽章里显示过，这里再写一遍是重复信息，
  // 而且会把「限流 | 7d」和倒计时之间撑开。语义靠 title / aria-label 承载。
  return (
    <span
      className="inline-flex size-5 shrink-0 items-center justify-center rounded-md border border-teal-500/30 bg-teal-500/10 text-teal-700 dark:text-teal-300"
      title={t("accounts.usingCreditsBadgeTooltip")}
      aria-label={t("accounts.usingCreditsBadge")}
    >
      <Coins className="size-3" />
    </span>
  );
}

// 冷却倒计时组件
export function CooldownTimer({ until }: { until: string }) {
  const remaining = useCountdownRemaining(until);
  const title = formatBeijingTime(until);

  if (!remaining) return null;
  return (
    <span
      className="inline-flex h-6 min-w-[112px] shrink-0 items-center justify-center gap-1.5 rounded-full bg-amber-50 px-2 text-[11px] font-mono leading-none tabular-nums text-amber-700 ring-1 ring-inset ring-amber-200/70 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20"
      title={title}
    >
      <Hourglass className="size-3 shrink-0" aria-hidden="true" />
      {remaining}
    </span>
  );
}
