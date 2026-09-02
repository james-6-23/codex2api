import type { ChangeEvent } from "react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Loader2, Unlink, Zap } from "lucide-react";

import { api, type ProxyRow } from "../api";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import type { ProxyBindingContext } from "../lib/accountProxyBinding";
import AccountProxyBadge from "./AccountProxyBadge";
import Modal from "./Modal";
import { ProxyPoolSelect } from "./ProxyPoolSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

const PROXY_SCHEMES = ["http:", "https:", "socks5:", "socks5h:"];

// 与代理页 validateProxyInput 同一套规则：入池的和绑账号的必须一个口径，
// 否则会出现"池里进不去、却能绑到账号上"的错配。
function isValidProxyInput(url: string): boolean {
  const trimmed = url.trim();
  if (!trimmed) return false;
  try {
    const parsed = new URL(trimmed);
    return Boolean(parsed.hostname) && PROXY_SCHEMES.includes(parsed.protocol);
  } catch {
    return false;
  }
}

export interface ProxyQuickEditorAccount {
  id: number;
  proxy_url?: string | null;
  group_ids?: number[] | null;
}

interface AccountProxyQuickEditorProps {
  account: ProxyQuickEditorAccount | null;
  /** 账号显示名由各页自己的命名规则决定（Codex 用邮箱、Grok/Responses 用名称）。 */
  accountLabel: string;
  proxies: ProxyRow[];
  ctx: ProxyBindingContext;
  onClose: () => void;
  onSaved: () => void | Promise<void>;
}

// 只改 proxy_url 一个字段的小弹窗：从账号列表的代理徽章直达，省掉"进编辑页→
// 翻到网络那一栏→连带提交整张表单"的绕路。
export default function AccountProxyQuickEditor({
  account,
  accountLabel,
  proxies,
  ctx,
  onClose,
  onSaved,
}: AccountProxyQuickEditorProps) {
  const { t, i18n } = useTranslation();
  const { showToast } = useToast();

  const [value, setValue] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [syncedId, setSyncedId] = useState<number | null>(null);

  const accountID = account?.id ?? null;
  if (accountID !== syncedId) {
    setSyncedId(accountID);
    setValue((account?.proxy_url ?? "").trim());
  }

  const busy = saving || testing;
  const trimmed = value.trim();
  const boundURL = (account?.proxy_url ?? "").trim();
  const dirty = trimmed !== boundURL;

  const handleTest = async () => {
    if (!trimmed || testing) return;
    setTesting(true);
    try {
      const result = await api.testProxy(
        trimmed,
        undefined,
        i18n.language?.startsWith("zh") ? "zh-CN" : "en",
      );
      if (!result.success) {
        showToast(
          t("accounts.proxyTestFailed", {
            error: result.error || t("accounts.proxyTestUnknownError"),
          }),
          "error",
        );
        return;
      }
      const location =
        result.location ||
        [result.country, result.region, result.city].filter(Boolean).join(" ");
      showToast(
        t("accounts.proxyTestSuccess", {
          ip: result.ip || "-",
          location: location || "-",
          latency: result.latency_ms ?? 0,
        }),
      );
    } catch (error) {
      showToast(
        t("accounts.proxyTestFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setTesting(false);
    }
  };

  const submit = async (nextURL: string) => {
    if (!account || saving) return;
    if (nextURL && !isValidProxyInput(nextURL)) {
      showToast(t("accounts.proxyQuickInvalid"), "error");
      return;
    }
    setSaving(true);
    try {
      await api.updateAccountScheduler(account.id, { proxy_url: nextURL });
      showToast(
        nextURL
          ? t("accounts.proxyQuickSaveDone")
          : t("accounts.proxyQuickUnbindDone"),
      );
      await onSaved();
      onClose();
    } catch (error) {
      showToast(
        t("accounts.proxyQuickSaveFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      show={Boolean(account)}
      title={t("accounts.proxyQuickTitle")}
      contentClassName="sm:max-w-[520px]"
      onClose={() => {
        if (busy) return;
        onClose();
      }}
      footer={
        <>
          <Button
            type="button"
            variant="outline"
            className="mr-auto gap-1.5 text-destructive hover:bg-destructive/10 hover:text-destructive"
            disabled={busy || !boundURL}
            onClick={() => void submit("")}
          >
            <Unlink className="size-3.5" />
            {t("accounts.proxyQuickUnbind")}
          </Button>
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button
            type="button"
            disabled={busy || !dirty}
            onClick={() => void submit(trimmed)}
          >
            {saving ? <Loader2 className="size-4 animate-spin" /> : null}
            {saving ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
          <div className="break-all font-semibold text-foreground">
            {accountLabel}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-2">
            <span className="text-xs">{t("accounts.proxyQuickCurrent")}</span>
            <AccountProxyBadge account={account} ctx={ctx} />
          </div>
          <div className="mt-2 text-xs">{t("accounts.proxyQuickDesc")}</div>
        </div>

        <div className="space-y-2.5">
          <label className="block text-sm font-semibold text-muted-foreground">
            {t("accounts.proxyUrl")}
          </label>
          <div className="flex flex-col gap-2 sm:flex-row sm:items-stretch">
            <Input
              className="min-w-0 flex-1"
              placeholder={t("accounts.proxyUrlPlaceholder")}
              value={value}
              disabled={busy}
              onChange={(event: ChangeEvent<HTMLInputElement>) =>
                setValue(event.target.value)
              }
            />
            <Button
              type="button"
              variant="outline"
              className="shrink-0 justify-center gap-1.5 sm:min-w-[108px]"
              disabled={busy || !trimmed}
              onClick={() => void handleTest()}
            >
              <Zap className={`size-3.5 ${testing ? "animate-pulse" : ""}`} />
              {testing ? t("accounts.testingProxy") : t("accounts.testProxy")}
            </Button>
          </div>
          <ProxyPoolSelect
            className="w-full"
            proxies={proxies}
            disabled={busy}
            onSelect={setValue}
          />
        </div>
      </div>
    </Modal>
  );
}
