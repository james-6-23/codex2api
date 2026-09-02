import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Zap } from "lucide-react";

import { api } from "../api";
import type { ProxyRow } from "../api";
import { ProxyPoolSelect } from "./ProxyPoolSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";

// ProxyField 是账号表单里统一的代理选择字段(与 Codex 的 renderProxyInput 同构):
//   第一行:手动填写代理 URL + 「测试」按钮(调 /proxies/test 验证连通与落地地点)
//   第二行:从代理池下拉选择(池非空时显示,含地点/绑定数/空闲优先)
// 各渠道的添加/编辑弹窗都用它,避免各页自造导致体验割裂。
export function ProxyField({
  value,
  onChange,
  proxies,
  label,
  placeholder = "socks5://user:pass@host:port",
  disabled = false,
}: {
  value: string;
  onChange: (value: string) => void;
  proxies: ProxyRow[];
  label?: string;
  placeholder?: string;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [testing, setTesting] = useState(false);

  const testProxy = async () => {
    const url = value.trim();
    if (!url) return;
    setTesting(true);
    try {
      const res = await api.testProxy(url);
      if (res.success) {
        const loc = [res.country, res.region, res.city].filter(Boolean).join(" ") || res.location || res.ip || "";
        showToast(
          `${t("accounts.testProxySuccess")}${loc ? ` · ${loc}` : ""}${res.latency_ms ? ` · ${res.latency_ms}ms` : ""}`,
          "success",
        );
      } else {
        showToast(res.error || t("accounts.testProxyFailed"), "error");
      }
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setTesting(false);
    }
  };

  return (
    <div className="space-y-2">
      <span className="text-xs font-semibold text-muted-foreground">{label ?? t("accounts.proxyUrl")}</span>
      <div className="flex flex-col gap-2 sm:flex-row sm:items-stretch">
        <Input
          className="min-w-0 flex-1"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          disabled={disabled}
        />
        <Button
          type="button"
          variant="outline"
          className="shrink-0 justify-center gap-1.5 sm:min-w-[104px]"
          disabled={disabled || testing || !value.trim()}
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
    </div>
  );
}
