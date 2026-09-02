import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Pencil, Trash2 } from "lucide-react";

import { api } from "../api";
import type { AccountGroup, UpstreamChannel } from "../types";
import Modal from "./Modal";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { getErrorMessage } from "../utils/error";

// 分组管理器的调色板(与账号页一致,避免各页各造一套)。
export const ACCOUNT_GROUP_COLORS = [
  "#2563eb",
  "#16a34a",
  "#d97706",
  "#dc2626",
  "#7c3aed",
  "#0891b2",
  "#64748b",
] as const;

function normalizeGroupColor(color?: string): string {
  const v = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(v) ? v : ACCOUNT_GROUP_COLORS[0];
}

type GroupDraft = {
  id: number | null;
  name: string;
  description: string;
  color: string;
  baseConcurrency: string;
  autoPause5h: string;
  autoPause7d: string;
  proxyUrls: string;
};

function emptyDraft(color: string): GroupDraft {
  return { id: null, name: "", description: "", color, baseConcurrency: "", autoPause5h: "", autoPause7d: "", proxyUrls: "" };
}

// AccountGroupManagerModal 是各渠道通用的「管理分组」弹窗:创建/编辑/删除分组,
// 字段与账号页的分组管理器一致(名称/描述/颜色/基础并发/自动暂停阈值/分组代理)。
// channel 决定新建分组归属的渠道;groups 为该渠道已有分组。
export function AccountGroupManagerModal({
  channel,
  groups,
  title,
  onClose,
  onChanged,
}: {
  channel: UpstreamChannel;
  groups: AccountGroup[];
  title?: string;
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const defaultColor = useMemo(
    () => ACCOUNT_GROUP_COLORS[groups.length % ACCOUNT_GROUP_COLORS.length],
    [groups.length],
  );
  const [draft, setDraft] = useState<GroupDraft>(() => emptyDraft(defaultColor));
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (draft.id === null) setDraft((d) => ({ ...d, color: d.color || defaultColor }));
  }, [defaultColor, draft.id]);

  const reset = useCallback(() => setDraft(emptyDraft(defaultColor)), [defaultColor]);

  const parseNum = (v: string): number | null => {
    const s = v.trim();
    if (!s) return null;
    const n = Number(s);
    return Number.isFinite(n) ? n : null;
  };

  const startEdit = (g: AccountGroup) => {
    setDraft({
      id: g.id,
      name: g.name,
      description: g.description ?? "",
      color: normalizeGroupColor(g.color),
      baseConcurrency: g.base_concurrency_override != null ? String(g.base_concurrency_override) : "",
      autoPause5h: g.auto_pause_5h_threshold ? String(g.auto_pause_5h_threshold) : "",
      autoPause7d: g.auto_pause_7d_threshold ? String(g.auto_pause_7d_threshold) : "",
      proxyUrls: (g.proxy_urls ?? []).join("\n"),
    });
  };

  const save = useCallback(async () => {
    const name = draft.name.trim();
    if (!name) {
      showToast(t("accountGroups.nameRequired"), "error");
      return;
    }
    setBusy(true);
    const payload = {
      name,
      description: draft.description.trim(),
      color: normalizeGroupColor(draft.color),
      base_concurrency_override: parseNum(draft.baseConcurrency),
      auto_pause_5h_threshold: parseNum(draft.autoPause5h) ?? 0,
      auto_pause_7d_threshold: parseNum(draft.autoPause7d) ?? 0,
      proxy_urls: draft.proxyUrls
        .split(/[\n,]/)
        .map((s) => s.trim())
        .filter(Boolean),
    };
    try {
      if (draft.id === null) {
        await api.createAccountGroup({ ...payload, channel });
      } else {
        await api.updateAccountGroup(draft.id, { ...payload, channel });
      }
      showToast(t("accountGroups.saved"), "success");
      reset();
      onChanged();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  }, [draft, channel, onChanged, reset, showToast, t]);

  const remove = useCallback(
    async (g: AccountGroup) => {
      const ok = await confirm({ title: t("accountGroups.deleteConfirm"), description: g.name });
      if (!ok) return;
      try {
        await api.deleteAccountGroup(g.id, true);
        showToast(t("accountGroups.deleted"), "success");
        if (draft.id === g.id) reset();
        onChanged();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      }
    },
    [confirm, draft.id, onChanged, reset, showToast, t],
  );

  const fieldLabel = "text-xs font-semibold text-muted-foreground";

  return (
    <Modal
      show
      onClose={onClose}
      title={title ?? t("accountGroups.manageTitle")}
      contentClassName="sm:max-w-[760px]"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.close")}
          </Button>
          <Button onClick={() => void save()} disabled={busy || !draft.name.trim()}>
            {draft.id === null ? t("accountGroups.create") : t("common.save")}
          </Button>
        </div>
      }
    >
      <div className="grid gap-5 md:grid-cols-[minmax(0,1fr)_260px]">
        {/* 左:创建/编辑表单 */}
        <div className="space-y-3">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {draft.id === null ? t("accountGroups.newGroup") : t("accountGroups.editGroup")}
          </div>
          <div className="space-y-1">
            <span className={fieldLabel}>{t("accountGroups.name")}</span>
            <Input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} placeholder={t("accountGroups.namePlaceholder")} />
          </div>
          <div className="space-y-1">
            <span className={fieldLabel}>{t("accountGroups.color")}</span>
            <div className="flex flex-wrap gap-1.5">
              {ACCOUNT_GROUP_COLORS.map((c) => (
                <button
                  key={c}
                  type="button"
                  onClick={() => setDraft({ ...draft, color: c })}
                  className={cn(
                    "size-6 rounded-full ring-2 ring-offset-2 ring-offset-background transition-transform hover:scale-110",
                    normalizeGroupColor(draft.color) === c ? "ring-foreground" : "ring-transparent",
                  )}
                  style={{ backgroundColor: c }}
                  aria-label={c}
                />
              ))}
            </div>
          </div>
          <div className="space-y-1">
            <span className={fieldLabel}>{t("accountGroups.description")}</span>
            <Input value={draft.description} onChange={(e) => setDraft({ ...draft, description: e.target.value })} placeholder={t("accountGroups.descriptionPlaceholder")} />
          </div>
          <div className="grid grid-cols-3 gap-2">
            <div className="space-y-1">
              <span className={fieldLabel}>{t("accountGroups.baseConcurrency")}</span>
              <Input value={draft.baseConcurrency} onChange={(e) => setDraft({ ...draft, baseConcurrency: e.target.value })} placeholder={t("accountGroups.followGlobal")} inputMode="numeric" />
            </div>
            <div className="space-y-1">
              <span className={fieldLabel}>{t("accountGroups.autoPause5h")}</span>
              <Input value={draft.autoPause5h} onChange={(e) => setDraft({ ...draft, autoPause5h: e.target.value })} placeholder="0" inputMode="numeric" />
            </div>
            <div className="space-y-1">
              <span className={fieldLabel}>{t("accountGroups.autoPause7d")}</span>
              <Input value={draft.autoPause7d} onChange={(e) => setDraft({ ...draft, autoPause7d: e.target.value })} placeholder="0" inputMode="numeric" />
            </div>
          </div>
          <div className="space-y-1">
            <span className={fieldLabel}>{t("accountGroups.proxyUrls")}</span>
            <textarea
              value={draft.proxyUrls}
              onChange={(e) => setDraft({ ...draft, proxyUrls: e.target.value })}
              rows={2}
              placeholder={t("accountGroups.proxyUrlsPlaceholder")}
              className="w-full resize-none rounded-md border border-input bg-background p-2 font-mono text-[11px] outline-none focus-visible:border-ring"
            />
          </div>
          {draft.id !== null ? (
            <Button variant="ghost" size="sm" onClick={reset}>
              {t("accountGroups.cancelEdit")}
            </Button>
          ) : null}
        </div>

        {/* 右:已有分组列表 */}
        <div className="space-y-1.5">
          <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("accountGroups.existing", { count: groups.length })}
          </div>
          {groups.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">{t("accountGroups.empty")}</p>
          ) : (
            <div className="max-h-[320px] space-y-1 overflow-y-auto pr-1">
              {groups.map((g) => (
                <div
                  key={g.id}
                  className={cn(
                    "flex items-center justify-between gap-2 rounded-md border px-2.5 py-1.5",
                    draft.id === g.id ? "border-primary/40 bg-primary/5" : "border-border",
                  )}
                >
                  <span className="inline-flex min-w-0 items-center gap-2 text-sm">
                    <span className="size-3 shrink-0 rounded-full" style={{ backgroundColor: normalizeGroupColor(g.color) }} />
                    <span className="truncate">{g.name}</span>
                    <span className="shrink-0 text-xs text-muted-foreground">({g.member_count})</span>
                  </span>
                  <span className="flex shrink-0 items-center gap-0.5">
                    <button type="button" onClick={() => startEdit(g)} className="rounded p-1 text-muted-foreground hover:text-foreground" title={t("common.edit")}>
                      <Pencil className="size-3.5" />
                    </button>
                    <button type="button" onClick={() => void remove(g)} className="rounded p-1 text-muted-foreground hover:text-rose-600 dark:hover:text-rose-400" title={t("common.delete")}>
                      <Trash2 className="size-3.5" />
                    </button>
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
      {confirmDialog}
    </Modal>
  );
}
