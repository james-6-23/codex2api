import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import ChannelLogo from "./ChannelLogo";
import { cn } from "@/lib/utils";

// 仪表盘/用量页共用的上游渠道过滤（全部/Codex/Claude/Grok）。
// 选择持久化到 localStorage，两页共享同一份状态键。
export type UsageChannel = "" | "codex" | "grok" | "claude";

const USAGE_CHANNEL_KEY = "codex2api:usage:channel";

export function useUsageChannel(): [UsageChannel, (next: UsageChannel) => void] {
  const [channel, setChannel] = useState<UsageChannel>(() => {
    try {
      const raw = window.localStorage.getItem(USAGE_CHANNEL_KEY);
      if (raw === "codex" || raw === "grok" || raw === "claude") return raw;
    } catch {
      // ignore
    }
    return "";
  });
  useEffect(() => {
    try {
      window.localStorage.setItem(USAGE_CHANNEL_KEY, channel);
    } catch {
      // ignore
    }
  }, [channel]);
  return [channel, setChannel];
}

export default function ChannelFilter({
  value,
  onChange,
  className,
}: {
  value: UsageChannel;
  onChange: (next: UsageChannel) => void;
  className?: string;
}) {
  const { t } = useTranslation();
  const options: Array<{
    key: UsageChannel;
    label: string;
    logo?: "codex" | "grok" | "claude";
  }> = [
    { key: "", label: t("usage.channelAll") },
    { key: "codex", label: "Codex", logo: "codex" },
    { key: "claude", label: "Claude", logo: "claude" },
    { key: "grok", label: "Grok", logo: "grok" },
  ];
  const activeIndex = Math.max(
    0,
    options.findIndex((o) => o.key === value),
  );
  return (
    <div
      className={cn(
        "relative grid grid-cols-4 items-center rounded-lg border border-border bg-muted/40 p-0.5",
        className,
      )}
    >
      {/* 滑块指示器：等宽四格，translateX 过渡到选中项 */}
      <span
        aria-hidden
        className="absolute inset-y-0.5 left-0.5 w-[calc((100%-4px)/4)] rounded-md bg-background shadow-sm transition-transform duration-300 ease-out"
        style={{ transform: `translateX(${activeIndex * 100}%)` }}
      />
      {options.map(({ key, label, logo }) => (
        <button
          key={key || "all"}
          type="button"
          onClick={() => onChange(key)}
          aria-pressed={value === key}
          title={label}
          className={cn(
            "relative z-10 inline-flex items-center justify-center gap-1.5 rounded-md px-3.5 py-1.5 text-sm font-semibold transition-colors duration-200",
            value === key
              ? "text-foreground"
              : "text-muted-foreground opacity-75 grayscale hover:opacity-100 hover:grayscale-0 hover:text-foreground",
          )}
        >
          {logo ? <ChannelLogo channel={logo} size={16} /> : null}
          {label}
        </button>
      ))}
    </div>
  );
}
