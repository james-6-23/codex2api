import type { ChangeEvent } from "react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Fingerprint,
  Gauge,
  Globe,
  Loader2,
  Save,
  Tag,
} from "lucide-react";
import type {
  AccountGroup,
  AccountRow,
  CodexFingerprintMode,
} from "../types";
import { api } from "../api";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import {
  accountHasQuickConfigDetails,
  buildQuickConfigSavePayload,
  canSaveQuickConfig,
  formStateFromAccount,
  isAbortError,
  isQuickConfigFormCurrent,
  type QuickConfigFormState,
  type QuickConfigLoadStatus,
  type QuickConfigReadySaveError,
} from "../lib/accountQuickConfig";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Sheet,
  SheetBody,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import ChipInput from "./ChipInput";
import AccountGroupMultiSelect from "./AccountGroupMultiSelect";
import StateShell from "./StateShell";

function formatSignedNumber(value: number): string {
  if (value > 0) return `+${value}`;
  return String(value);
}

function getDefaultScoreBias(planType?: string | null): number {
  const normalized = (planType || "").toLowerCase();
  if (
    normalized.includes("pro") ||
    normalized.includes("plus") ||
    normalized.includes("team")
  ) {
    return 50;
  }
  return 0;
}

const SAVE_ERROR_TOAST: Record<QuickConfigReadySaveError, string> = {
  invalid_headers: "自定义请求头必须是 JSON 对象，且所有值必须是字符串",
  invalid_score: "自定义加权分必须在 -200 到 200 之间",
  invalid_concurrency: "基础并发覆盖必须大于等于 1",
  invalid_priority: "调度优先级必须在 -100 到 100 之间",
};

interface AccountQuickConfigSheetProps {
  account: AccountRow | null;
  groups: AccountGroup[];
  show: boolean;
  onClose: () => void;
  onSaved: () => void;
}

export default function AccountQuickConfigSheet({
  account,
  groups,
  show,
  onClose,
  onSaved,
}: AccountQuickConfigSheetProps) {
  const { t } = useTranslation();
  const { showToast } = useToast();

  const [saving, setSaving] = useState(false);
  const [loadStatus, setLoadStatus] = useState<QuickConfigLoadStatus>("loading");
  const [loadError, setLoadError] = useState("");
  const [retryNonce, setRetryNonce] = useState(0);
  const [form, setForm] = useState<QuickConfigFormState | null>(null);
  const [syncedId, setSyncedId] = useState<number | null>(null);

  const accountId = account?.id ?? null;
  if (accountId !== syncedId) {
    setSyncedId(accountId);
    if (account && accountHasQuickConfigDetails(account)) {
      setForm(formStateFromAccount(account));
      setLoadStatus("ready");
      setLoadError("");
    } else {
      setForm(null);
      setLoadStatus("loading");
      setLoadError("");
    }
  }

  const formCurrent = isQuickConfigFormCurrent(form, accountId);
  const detailsReady = loadStatus === "ready" && formCurrent;
  const detailLoaded = Boolean(account?.detail_loaded);
  const saveEnabled = canSaveQuickConfig({
    status: loadStatus,
    saving,
    form,
    accountId,
  });

  useEffect(() => {
    if (!show || accountId == null || detailLoaded) {
      return undefined;
    }

    const controller = new AbortController();
    setForm(null);
    setLoadStatus("loading");
    setLoadError("");
    void api
      .getAccount(accountId, controller.signal)
      .then((detailed) => {
        setForm(formStateFromAccount(detailed));
        setLoadStatus("ready");
      })
      .catch((error: unknown) => {
        if (isAbortError(error) || controller.signal.aborted) return;
        const message = getErrorMessage(error);
        setForm(null);
        setLoadStatus("error");
        setLoadError(message);
        showToast(message, "error");
      });

    return () => {
      controller.abort();
    };
  }, [show, accountId, detailLoaded, retryNonce, showToast]);

  const patchForm = (patch: Partial<QuickConfigFormState>) => {
    setForm((current) => (current ? { ...current, ...patch } : current));
  };

  if (!account) return null;

  const handleSave = async () => {
    if (!saveEnabled) return;
    const result = buildQuickConfigSavePayload(form, detailsReady);
    if (!result.ok) {
      if (result.error !== "not_ready") {
        showToast(SAVE_ERROR_TOAST[result.error], "error");
      }
      return;
    }

    setSaving(true);
    try {
      await api.updateAccountScheduler(account.id, result.payload);
      showToast("账号配置与设备指纹已保存");
      onSaved();
      onClose();
    } catch (err) {
      showToast(`保存失败: ${getErrorMessage(err)}`, "error");
    } finally {
      setSaving(false);
    }
  };

  const fingerprintOptions: { value: CodexFingerprintMode; label: string }[] = [
    { value: "off", label: t("accounts.codexFingerprintModeOff") },
    { value: "device", label: t("accounts.codexFingerprintModeDevice") },
    { value: "session", label: t("accounts.codexFingerprintModeSession") },
    { value: "full", label: t("accounts.codexFingerprintModeFull") },
  ];

  const fingerprintDetails: Record<CodexFingerprintMode, string> = {
    off: t("accounts.codexFingerprintModeOffDetail"),
    device: t("accounts.codexFingerprintModeDeviceDetail"),
    session: t("accounts.codexFingerprintModeSessionDetail"),
    full: t("accounts.codexFingerprintModeFullDetail"),
  };

  const fingerprintMode = form?.fingerprintMode ?? "off";
  const scoreMode = form?.scoreMode ?? "default";
  const concurrencyMode = form?.concurrencyMode ?? "default";

  return (
    <Sheet open={show} onOpenChange={(o) => { if (!o) onClose(); }}>
      <SheetContent
        side="left"
        className="sm:w-[min(calc(100%-1.5rem),520px)] sm:max-w-[min(calc(100%-1.5rem),520px)]"
      >
        <SheetHeader>
          <div className="flex items-center gap-2">
            <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-primary/20">
              <Fingerprint className="size-4.5" />
            </div>
            <div className="min-w-0">
              <SheetTitle className="text-base font-bold text-foreground">
                账号指纹与快捷配置
              </SheetTitle>
              <SheetDescription className="truncate text-xs text-muted-foreground">
                {account.email || account.name || `ID ${account.id}`}
              </SheetDescription>
            </div>
          </div>
        </SheetHeader>

        <SheetBody className="space-y-5">
          <div className="rounded-xl border border-border/70 bg-gradient-to-r from-primary/5 via-muted/30 to-background p-3.5 shadow-2xs">
            <div className="flex items-center justify-between gap-2">
              <div className="min-w-0 space-y-0.5">
                <div className="text-xs font-bold text-foreground truncate">
                  {account.email || account.name || `ID ${account.id}`}
                </div>
                <div className="text-[11px] text-muted-foreground">
                  账号 ID: {account.id} · 套餐: {account.plan_type || "Standard"}
                </div>
              </div>
              <Badge variant="outline" className="text-xs font-mono font-semibold">
                {account.status || "active"}
              </Badge>
            </div>
          </div>

          <StateShell
            variant="section"
            loading={loadStatus === "loading" || (loadStatus === "ready" && !formCurrent)}
            error={loadStatus === "error" ? loadError || t("common.loadFailed") : null}
            onRetry={() => {
              setForm(null);
              setLoadStatus("loading");
              setLoadError("");
              setRetryNonce((value) => value + 1);
            }}
            loadingTitle={t("common.loading")}
            loadingDescription={t("accounts.quickConfigLoadingDesc")}
            errorTitle={t("common.loadFailed")}
          >
            {form ? (
              <>
          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Fingerprint className="size-4 text-teal-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                {t("accounts.codexFingerprintModeTitle")}
              </span>
            </div>
            <p className="text-xs text-muted-foreground leading-relaxed">
              {t("accounts.codexFingerprintModeHint")}
            </p>
            <div className="grid grid-cols-2 gap-1.5 rounded-xl border border-border/70 bg-muted/30 p-1">
              {fingerprintOptions.map((opt) => (
                <button
                  key={opt.value}
                  type="button"
                  onClick={() => patchForm({ fingerprintMode: opt.value })}
                  className={`rounded-lg px-2.5 py-1.5 text-xs font-semibold transition-all duration-150 ${
                    fingerprintMode === opt.value
                      ? "bg-primary text-primary-foreground font-bold shadow-xs ring-1 ring-primary/30"
                      : "text-muted-foreground hover:bg-background/60 hover:text-foreground"
                  }`}
                >
                  {opt.label}
                </button>
              ))}
            </div>
            <div className="rounded-lg border border-border/50 bg-muted/20 p-2.5 text-xs text-muted-foreground">
              {fingerprintDetails[fingerprintMode]}
            </div>
          </div>

          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3.5">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Gauge className="size-4 text-amber-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                调度与并发加权
              </span>
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between text-xs font-semibold">
                <span className="text-foreground">{t("accounts.schedulerScoreLabel")}</span>
                <span className="text-muted-foreground text-[11px]">{t("accounts.schedulerScoreHint")}</span>
              </div>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => patchForm({ scoreMode: "default" })}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    scoreMode === "default"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerScoreAuto")}
                </button>
                <button
                  type="button"
                  onClick={() => patchForm({ scoreMode: "custom" })}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    scoreMode === "custom"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerCustom")}
                </button>
              </div>
              {scoreMode === "default" ? (
                <div className="rounded-lg border border-border/60 bg-muted/20 px-3 py-2 text-xs text-muted-foreground flex justify-between items-center">
                  <span>跟随套餐默认</span>
                  <span className="font-mono font-bold text-primary">
                    {formatSignedNumber(getDefaultScoreBias(account.plan_type))}
                  </span>
                </div>
              ) : (
                <Input
                  inputMode="numeric"
                  value={form.scoreInput}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    patchForm({ scoreInput: e.target.value })
                  }
                  placeholder={t("accounts.schedulerScorePlaceholder")}
                />
              )}
            </div>

            <div className="space-y-2 pt-1 border-t border-border/40">
              <div className="flex items-center justify-between text-xs font-semibold">
                <span className="text-foreground">{t("accounts.schedulerConcurrencyLabel")}</span>
                <span className="text-muted-foreground text-[11px]">{t("accounts.schedulerConcurrencyHint")}</span>
              </div>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => patchForm({ concurrencyMode: "default" })}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    concurrencyMode === "default"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerConcurrencyAuto")}
                </button>
                <button
                  type="button"
                  onClick={() => patchForm({ concurrencyMode: "custom" })}
                  className={`flex-1 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all ${
                    concurrencyMode === "custom"
                      ? "bg-primary text-primary-foreground shadow-2xs"
                      : "bg-muted/60 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {t("accounts.schedulerCustom")}
                </button>
              </div>
              {concurrencyMode === "default" ? (
                <div className="rounded-lg border border-border/60 bg-muted/20 px-3 py-2 text-xs text-muted-foreground flex justify-between items-center">
                  <span>跟随分组/全局</span>
                  <span className="font-mono font-bold text-primary">
                    {account.base_concurrency_effective ?? 2}
                  </span>
                </div>
              ) : (
                <Input
                  inputMode="numeric"
                  value={form.concurrencyInput}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    patchForm({ concurrencyInput: e.target.value })
                  }
                  placeholder={t("accounts.schedulerConcurrencyPlaceholder")}
                />
              )}
            </div>

            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                {t("accounts.schedulerPriorityTitle")}
              </label>
              <Input
                inputMode="numeric"
                value={form.schedulerPriorityInput}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  patchForm({ schedulerPriorityInput: e.target.value })
                }
                placeholder={t("accounts.schedulerPriorityPlaceholder")}
              />
            </div>
          </div>

          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3.5">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Globe className="size-4 text-indigo-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                防护与网络代理
              </span>
            </div>

            <div className="flex items-center justify-between gap-3">
              <div>
                <div className="text-xs font-semibold text-foreground">
                  {t("accounts.schedulerSkipWarmLabel")}
                </div>
                <div className="text-[11px] text-muted-foreground">
                  {t("accounts.schedulerSkipWarmHint")}
                </div>
              </div>
              <Switch
                checked={form.skipWarmTier}
                onCheckedChange={(checked) => patchForm({ skipWarmTier: checked })}
              />
            </div>

            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                代理服务器 (Proxy URL)
              </label>
              <Input
                value={form.proxyUrl}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  patchForm({ proxyUrl: e.target.value })
                }
                placeholder="http://user:pass@host:port"
              />
            </div>

            <div className="space-y-1.5">
              <label className="block text-xs font-semibold text-foreground" htmlFor="upstream-request-id-header">上游请求 ID 响应头</label>
              <Input id="upstream-request-id-header" maxLength={64} value={form.upstreamRequestIdHeader}
                onChange={(e: ChangeEvent<HTMLInputElement>) => patchForm({ upstreamRequestIdHeader: e.target.value })}
                placeholder="自动识别 X-Request-ID / Request-ID" />
              <p className="text-[11px] text-muted-foreground">填写上游用于定位请求的响应头名称，留空自动识别。WebSocket 轮次不记录握手请求 ID。</p>
            </div>

            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                自定义请求头 JSON
              </label>
              <textarea
                rows={3}
                value={form.customHeadersText}
                onChange={(e: ChangeEvent<HTMLTextAreaElement>) =>
                  patchForm({ customHeadersText: e.target.value })
                }
                placeholder='{"Chatgpt-Account-Id": "workspace-id"}'
                className="w-full rounded-lg border border-input bg-background px-3 py-2 font-mono text-xs shadow-2xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              />
            </div>
          </div>

          <div className="rounded-xl border border-border/70 bg-card p-4 shadow-2xs space-y-3.5">
            <div className="flex items-center gap-2 border-b border-border/50 pb-2.5">
              <Tag className="size-4 text-violet-500" />
              <span className="text-xs font-bold uppercase tracking-wider text-foreground">
                标签与分组
              </span>
            </div>

            <div className="space-y-1.5">
              <label className="block text-xs font-semibold text-foreground">
                {t("accounts.tagsLabel")}
              </label>
              <ChipInput
                value={form.tags}
                onChange={(tags) => patchForm({ tags })}
                placeholder={t("accounts.tagsPlaceholder")}
                maxVisible={3}
              />
            </div>

            <div className="space-y-1.5 pt-1 border-t border-border/40">
              <label className="block text-xs font-semibold text-foreground">
                {t("accounts.groupsLabel")}
              </label>
              <AccountGroupMultiSelect
                groups={groups}
                value={form.groupIds}
                onChange={(groupIds) => patchForm({ groupIds })}
                allLabel={t("accounts.groupsUnbound")}
                selectedLabel={t("accounts.groupsSelected", {
                  count: form.groupIds.length,
                })}
                placeholder={t("accounts.groupsPlaceholder")}
                emptyLabel={t("accounts.groupsNone")}
                emptyHint={t("accounts.groupsSelectHint")}
              />
            </div>
          </div>
              </>
            ) : null}
          </StateShell>
        </SheetBody>

        <SheetFooter>
          <Button variant="outline" onClick={onClose} disabled={saving}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void handleSave()} disabled={!saveEnabled}>
            {saving ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Save className="size-4" />
            )}
            保存配置
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
