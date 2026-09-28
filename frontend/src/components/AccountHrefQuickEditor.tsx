import type { ChangeEvent } from "react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ExternalLink, Loader2 } from "lucide-react";

import { api } from "../api";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import Modal from "./Modal";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export interface AccountHrefEditorAccount {
  id: number;
  account_href?: string | null;
  base_url?: string | null;
}

interface AccountHrefQuickEditorProps {
  account: AccountHrefEditorAccount | null;
  /** 账号显示名由各页自己的命名规则决定（Codex 用邮箱、中转/Grok 用名称）。 */
  accountLabel: string;
  onClose: () => void;
  onSaved: () => void | Promise<void>;
}

const HREF_SCHEMES = ["http:", "https:"];

function isValidHrefInput(url: string): boolean {
  if (!url.trim()) return true;
  try {
    const parsed = new URL(url.trim());
    return Boolean(parsed.hostname) && HREF_SCHEMES.includes(parsed.protocol);
  } catch {
    return false;
  }
}

/** 未配置跳转地址时实际打开的目标 = account_href ?? base_url。 */
export function accountHrefTarget(
  account: Pick<AccountHrefEditorAccount, "account_href" | "base_url"> | null,
): string {
  if (!account) return "";
  return (account.account_href || account.base_url || "").trim();
}

/** 在新窗口打开账号跳转地址；配置了 account_href 用它，否则回退 base_url。 */
export function openAccountHref(
  account: Pick<AccountHrefEditorAccount, "account_href" | "base_url"> | null,
): boolean {
  const target = accountHrefTarget(account);
  if (!target || !/^https?:\/\//i.test(target)) return false;
  window.open(target, "_blank", "noopener,noreferrer");
  return true;
}

// 只改 account_href 一个字段的小弹窗：从账号列表直达，配置的地址在浏览器新
// 窗口打开（账号控制台/用量页等）；留空 = 回退打开 api-base（base_url）。
export default function AccountHrefQuickEditor({
  account,
  accountLabel,
  onClose,
  onSaved,
}: AccountHrefQuickEditorProps) {
  const { t } = useTranslation();
  const { showToast } = useToast();

  const [value, setValue] = useState("");
  const [saving, setSaving] = useState(false);
  const [syncedId, setSyncedId] = useState<number | null>(null);

  const accountID = account?.id ?? null;
  if (accountID !== syncedId) {
    setSyncedId(accountID);
    setValue((account?.account_href ?? "").trim());
  }

  const trimmed = value.trim();
  const boundHref = (account?.account_href ?? "").trim();
  const dirty = trimmed !== boundHref;
  const fallbackURL = (account?.base_url ?? "").trim();

  const submit = async (nextHref: string) => {
    if (!account || saving) return;
    if (!isValidHrefInput(nextHref)) {
      showToast(t("accounts.hrefQuickInvalid"), "error");
      return;
    }
    setSaving(true);
    try {
      await api.updateAccountScheduler(account.id, { account_href: nextHref });
      showToast(t("accounts.hrefQuickSaveDone"));
      await onSaved();
      onClose();
    } catch (error) {
      showToast(
        t("accounts.hrefQuickSaveFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      show={Boolean(account)}
      title={t("accounts.hrefQuickTitle")}
      contentClassName="sm:max-w-[520px]"
      onClose={() => {
        if (saving) return;
        onClose();
      }}
      footer={
        <>
          <Button
            type="button"
            variant="outline"
            className="mr-auto gap-1.5"
            disabled={saving || !boundHref}
            onClick={() => void submit("")}
          >
            {t("accounts.hrefQuickClear")}
          </Button>
          <Button type="button" variant="outline" disabled={saving} onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button
            type="button"
            disabled={saving || !dirty}
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
          <div className="mt-2 text-xs">{t("accounts.hrefQuickDesc")}</div>
          {fallbackURL ? (
            <div className="mt-2 break-all text-xs">
              {t("accounts.hrefQuickFallback")}
              <span className="ml-1 font-medium text-foreground">
                {fallbackURL}
              </span>
            </div>
          ) : null}
        </div>

        <div className="space-y-2.5">
          <label className="block text-sm font-semibold text-muted-foreground">
            {t("accounts.hrefQuickLabel")}
          </label>
          <div className="flex flex-col gap-2 sm:flex-row sm:items-stretch">
            <Input
              className="min-w-0 flex-1"
              placeholder={t("accounts.hrefQuickPlaceholder")}
              value={value}
              disabled={saving}
              onChange={(event: ChangeEvent<HTMLInputElement>) =>
                setValue(event.target.value)
              }
            />
            <Button
              type="button"
              variant="outline"
              className="shrink-0 justify-center gap-1.5 sm:min-w-[108px]"
              disabled={saving || !accountHrefTarget({ account_href: trimmed, base_url: fallbackURL })}
              onClick={() => openAccountHref({ account_href: trimmed, base_url: fallbackURL })}
            >
              <ExternalLink className="size-3.5" />
              {t("accounts.hrefQuickOpen")}
            </Button>
          </div>
        </div>
      </div>
    </Modal>
  );
}
