import { useTranslation } from "react-i18next";
import { AlertTriangle, Globe, Plus } from "lucide-react";

import {
  formatProxyDisplayURL,
  formatProxyEgressSummary,
  formatProxyShortName,
  resolveAccountProxyBinding,
  type AccountProxyBindingKind,
  type ProxyBindingAccount,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import { cn } from "@/lib/utils";

// 账号列表里的代理徽章：一眼看出这个号在走谁的出口，以及它是不是已经没有出口了。
// 判定全在 lib/accountProxyBinding，本组件只负责配色与文案。
const TONE: Record<AccountProxyBindingKind, string> = {
  bound:
    "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20",
  bound_custom:
    "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20",
  bound_unusable:
    "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20",
  direct_blocked:
    "bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-300 dark:ring-rose-400/20",
  group:
    "bg-violet-50 text-violet-700 ring-violet-600/20 dark:bg-violet-950 dark:text-violet-400 dark:ring-violet-400/20",
  pool: "bg-muted/60 text-muted-foreground ring-border",
  global: "bg-muted/60 text-muted-foreground ring-border",
  direct: "text-muted-foreground ring-transparent",
};

interface AccountProxyBadgeProps {
  account: ProxyBindingAccount | null | undefined;
  ctx: ProxyBindingContext;
  onClick?: () => void;
  className?: string;
}

export default function AccountProxyBadge({
  account,
  ctx,
  onClick,
  className,
}: AccountProxyBadgeProps) {
  const { t } = useTranslation();
  const binding = resolveAccountProxyBinding(account, ctx);
  const shortName = formatProxyShortName(binding.proxy, binding.url);
  const egress = formatProxyEgressSummary(binding.proxy);
  const latency = binding.proxy?.test_latency_ms ?? 0;
  // 提示里的 URL 一律打掉账密：列表 hover 不是"我要看凭据"的动作。
  const safeURL = formatProxyDisplayURL(binding.url);

  let text: string;
  let tip: string;
  switch (binding.kind) {
    case "bound":
      text =
        binding.proxy?.test_status === "success" && latency > 0
          ? `${shortName} · ${latency}ms`
          : shortName;
      tip = [t("accounts.proxyTipBound", { url: safeURL }), egress]
        .filter(Boolean)
        .join("\n");
      break;
    case "bound_custom":
      text = shortName;
      tip = t("accounts.proxyTipCustom", { url: safeURL });
      break;
    case "bound_unusable":
      text = t("accounts.proxyBadgeUnusable");
      tip = t("accounts.proxyTipUnusable", { url: safeURL });
      break;
    case "group":
      text = t("accounts.proxyBadgeGroup", { name: binding.groupName });
      tip = t("accounts.proxyTipGroup", { name: binding.groupName });
      break;
    case "pool":
      text = t("accounts.proxyBadgePool");
      // 用 total 而不是 count：i18next 见到 count 会先去找 _one/_other 复数键。
      tip = t("accounts.proxyTipPool", { total: ctx.poolSize });
      break;
    case "global":
      text = t("accounts.proxyBadgeGlobal");
      tip = t("accounts.proxyTipGlobal", {
        url: formatProxyDisplayURL(ctx.globalProxy),
      });
      break;
    case "direct_blocked":
      text = t("accounts.proxyBadgeBlocked");
      tip = t("accounts.proxyTipBlocked");
      break;
    default:
      text = t("accounts.proxyBadgeDirect");
      tip = t("accounts.proxyTipDirect");
      break;
  }

  const title = onClick ? `${tip}\n${t("accounts.proxyTipEdit")}` : tip;
  const Icon =
    binding.kind === "bound_unusable" || binding.kind === "direct_blocked"
      ? AlertTriangle
      : binding.kind === "direct"
        ? Plus
        : Globe;

  const body = (
    <span
      className={cn(
        "inline-flex min-w-0 max-w-full items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold ring-1 ring-inset",
        binding.kind === "direct" && "border border-dashed border-border",
        TONE[binding.kind],
      )}
    >
      <Icon className="size-2.5 shrink-0" />
      <span className="truncate">{text}</span>
    </span>
  );

  if (!onClick) {
    return (
      <span className={cn("inline-flex min-w-0 max-w-full", className)} title={title}>
        {body}
      </span>
    );
  }

  return (
    <button
      type="button"
      className={cn(
        "inline-flex min-w-0 max-w-full text-left transition-opacity hover:opacity-80",
        className,
      )}
      title={title}
      onClick={(event) => {
        event.stopPropagation();
        onClick();
      }}
    >
      {body}
    </button>
  );
}
