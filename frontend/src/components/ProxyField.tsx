import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { X, Zap } from "lucide-react";

import { api } from "../api";
import type { ProxyRow } from "../api";
import { ProxyPoolSelect } from "./ProxyPoolSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import { cn } from "@/lib/utils";

// ProxyUrlInput 带内嵌清空按钮的代理 URL 输入框。各渠道账号弹窗的代理字段共用,
// 手填与从代理池选择写的是同一个 value,X 一键清空两者。
export function ProxyUrlInput({
  value,
  onChange,
  placeholder,
  disabled = false,
  className,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <div className={cn("relative", className)}>
      <Input
        className={value ? "w-full pr-8" : "w-full"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        disabled={disabled}
      />
      {value && !disabled ? (
        <button
          type="button"
          onClick={() => onChange("")}
          title={t("accounts.clearProxy")}
          aria-label={t("accounts.clearProxy")}
          className="absolute inset-y-0 right-0 flex w-8 items-center justify-center rounded-r-md text-muted-foreground transition-colors hover:text-foreground"
        >
          <X className="size-3.5" />
        </button>
      ) : null}
    </div>
  );
}

// ProxyField 是四个渠道(Codex/Grok/Claude/Antigravity)账号表单里唯一的代理选择字段:
//   第一行:手动填写代理 URL + 「测试」按钮(调 /proxies/test 验证连通与落地地点)
//   第二行:从代理池下拉选择(含地点/绑定数/空闲优先;池为空时给禁用占位与引导)
//   第三行:当前值命中池内节点时给一句关联提示——账号与节点按 URL 关联,
//          在「代理」页改节点地址会同步到所有绑定账号,删除则自动解绑。
// 各渠道的添加/编辑弹窗都必须用它,不要再各页自造导致体验割裂。
export function ProxyField({
  value,
  onChange,
  proxies,
  label,
  labelClassName,
  placeholder = "socks5://user:pass@host:port",
  disabled = false,
  className,
}: {
  value: string;
  onChange: (value: string) => void;
  proxies: ProxyRow[];
  /** 字段标题;传空串则不渲染标题(由外层自行摆放)。 */
  label?: string;
  /** 标题样式覆盖,便于与所在弹窗相邻字段的标题风格对齐。 */
  labelClassName?: string;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
}) {
  const { t, i18n } = useTranslation();
  const { showToast } = useToast();
  const [testing, setTesting] = useState(false);

  const trimmed = value.trim();
  // 当前值是否命中代理池节点:命中即"关联"态,提示用户改节点会级联。
  const linked = useMemo(
    () => (trimmed ? proxies.find((p) => p.url === trimmed) : undefined),
    [proxies, trimmed],
  );

  const testProxy = async () => {
    const url = trimmed;
    if (!url) {
      showToast(t("accounts.proxyUrlRequired"), "error");
      return;
    }
    setTesting(true);
    try {
      const lang = i18n.language?.startsWith("zh") ? "zh-CN" : "en";
      const res = await api.testProxy(url, undefined, lang);
      if (!res.success) {
        showToast(
          t("accounts.proxyTestFailed", { error: res.error || t("accounts.proxyTestUnknownError") }),
          "error",
        );
        return;
      }
      const location = res.location || [res.country, res.region, res.city].filter(Boolean).join(" ");
      showToast(
        t("accounts.proxyTestSuccess", {
          ip: res.ip || "-",
          location: location || "-",
          latency: res.latency_ms ?? 0,
        }),
        "success",
      );
    } catch (error) {
      showToast(t("accounts.proxyTestFailed", { error: getErrorMessage(error) }), "error");
    } finally {
      setTesting(false);
    }
  };

  const title = label ?? t("accounts.proxyUrl");

  return (
    <div className={cn("space-y-2", className)}>
      {title ? (
        <span className={cn("block text-xs font-semibold text-muted-foreground", labelClassName)}>{title}</span>
      ) : null}
      <div className="flex flex-col gap-2 sm:flex-row sm:items-stretch">
        <ProxyUrlInput
          className="min-w-0 flex-1"
          value={value}
          onChange={onChange}
          placeholder={placeholder}
          disabled={disabled}
        />
        <Button
          type="button"
          variant="outline"
          className="shrink-0 justify-center gap-1.5 sm:min-w-[104px]"
          disabled={disabled || testing || !trimmed}
          onClick={() => void testProxy()}
        >
          <Zap className={`size-3.5 ${testing ? "animate-pulse" : ""}`} />
          {testing ? t("accounts.testingProxy") : t("accounts.testProxy")}
        </Button>
      </div>
      {proxies.length > 0 ? (
        <ProxyPoolSelect className="w-full" proxies={proxies} value={value} onSelect={onChange} disabled={disabled} />
      ) : (
        // 代理池为空时仍显示一个禁用占位下拉 + 引导,让"从代理池选择"始终可见,
        // 避免让用户误以为该功能缺失(池条目来自「代理」页)。
        <div className="flex h-9 w-full items-center justify-between rounded-md border border-dashed border-input bg-muted/20 px-2.5 text-xs text-muted-foreground/70">
          <span>{t("accounts.proxyPoolEmpty")}</span>
          <span className="opacity-60">▾</span>
        </div>
      )}
      {linked ? (
        <p className="text-[11px] leading-relaxed text-muted-foreground">{t("accounts.proxyPoolLinkedHint")}</p>
      ) : null}
    </div>
  );
}
