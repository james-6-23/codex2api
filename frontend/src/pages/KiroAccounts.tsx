import { useCallback, useEffect, useRef, useState } from "react";
import type { ChangeEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Download, ExternalLink, FlaskConical, KeyRound, LogIn, Power, PowerOff, RefreshCw, Search, Trash2, Upload } from "lucide-react";
import { api } from "../api";
import type { NamedBlob } from "../api";
import type { AccountRow, KiroDeviceLoginStart, KiroUsageSnapshot } from "../types";
import AccountUsageBar from "../components/AccountUsageBar";
import ChannelLogo from "../components/ChannelLogo";
import { HeaderActionMenu } from "../components/HeaderActionMenu";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import TestConnectionModal from "../components/TestConnectionModal";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { useToast } from "../hooks/useToast";
import { getAccountStatusBadgeStatus } from "../lib/usageFormat";
import { getErrorMessage } from "../utils/error";

// Kiro 账号页：导入（kiro.rs / Kiro IDE / Kiro Account Manager 三种 JSON 自动识别）、
// Social 登录（回调 URL 粘回）、Builder ID / Enterprise 设备码登录、额度与模型同步、导出。

const PAGE_SIZE_OPTIONS = [20, 50, 100];
const STATUS_FILTERS = ["all", "normal", "rate_limited", "abnormal", "disabled"] as const;
type StatusFilter = (typeof STATUS_FILTERS)[number];

const TEXTAREA_CLASS =
  "flex min-h-[160px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50";

function downloadNamedBlob(payload: NamedBlob, fallbackName: string): void {
  const objectURL = URL.createObjectURL(payload.blob);
  const anchor = document.createElement("a");
  anchor.href = objectURL;
  anchor.download = payload.filename || fallbackName;
  anchor.rel = "noopener";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
}

function percent(used: number, limit: number): number | null {
  return limit > 0 ? (used / limit) * 100 : null;
}

function formatCredits(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(2);
}

function KiroQuota({ usage }: { usage?: KiroUsageSnapshot }) {
  const { t } = useTranslation();
  if (!usage || !usage.fetched_at || usage.limit <= 0) {
    return <span className="text-xs text-muted-foreground">{t("kiro.quotaUnknown")}</span>;
  }
  const extraUsed = usage.bonus_used + usage.trial_used;
  const extraLimit = usage.bonus_limit + usage.trial_limit;
  return (
    <div className="space-y-1">
      <AccountUsageBar
        label={t("kiro.quotaBase")}
        pct={percent(usage.base_used, usage.base_limit)}
        resetAt={usage.next_reset_at}
        detailText={`${formatCredits(usage.base_used)} / ${formatCredits(usage.base_limit)}`}
      />
      {extraLimit > 0 ? (
        <AccountUsageBar
          label={t("kiro.quotaBonus")}
          pct={percent(extraUsed, extraLimit)}
          resetAt={usage.credits_expire_at}
          detailText={`${formatCredits(extraUsed)} / ${formatCredits(extraLimit)}`}
        />
      ) : null}
    </div>
  );
}

function accountLabel(account: AccountRow): string {
  return account.email || account.name || `#${account.id}`;
}

function ImportModal({ show, onClose, onDone }: { show: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [content, setContent] = useState("");
  const [proxyURL, setProxyURL] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const readFile = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (file) setContent(await file.text());
  };

  const submit = async () => {
    setSubmitting(true);
    try {
      const result = await api.importKiroAccounts({ content, proxy_url: proxyURL.trim() || undefined });
      const firstError = result.results.find((item) => item.error)?.error;
      showToast(
        t("kiro.importResult", { imported: result.imported, failed: result.failed }) + (firstError ? ` · ${firstError}` : ""),
        result.failed > 0 ? "error" : "success",
      );
      if (result.imported > 0) {
        setContent("");
        onDone();
        onClose();
      }
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("kiro.importTitle")}
      onClose={onClose}
      contentClassName="sm:max-w-[640px]"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void submit()} disabled={submitting || !content.trim()}>
            {submitting ? <RefreshCw className="size-3.5 animate-spin" /> : <Upload className="size-3.5" />}
            {t("kiro.importSubmit")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-xs text-muted-foreground">{t("kiro.importHint")}</p>
        <textarea
          value={content}
          onChange={(event) => setContent(event.target.value)}
          placeholder={t("kiro.importPlaceholder")}
          className={TEXTAREA_CLASS}
          spellCheck={false}
        />
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => fileRef.current?.click()}>
            <Upload className="size-3.5" />
            {t("kiro.importFile")}
          </Button>
          <input ref={fileRef} type="file" accept=".json,application/json" className="hidden" onChange={(event) => void readFile(event)} />
        </div>
        <label className="block space-y-1 text-xs font-medium text-muted-foreground">
          <span>{t("kiro.proxyLabel")}</span>
          <Input value={proxyURL} onChange={(event) => setProxyURL(event.target.value)} placeholder="socks5://host:port" />
        </label>
      </div>
    </Modal>
  );
}

function SocialLoginModal({ show, onClose, onDone }: { show: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [session, setSession] = useState<{ auth_url: string; session: string } | null>(null);
  const [callbackURL, setCallbackURL] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!show) {
      setSession(null);
      setCallbackURL("");
    }
  }, [show]);

  const start = async () => {
    setBusy(true);
    try {
      const next = await api.startKiroSocialLogin({});
      setSession(next);
      window.open(next.auth_url, "_blank", "noopener");
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  };

  const finish = async () => {
    if (!session) return;
    setBusy(true);
    try {
      const created = await api.finishKiroSocialLogin({ session: session.session, callback_url: callbackURL.trim() });
      showToast(t("kiro.loginDone", { account: created.email || `#${created.id}` }), "success");
      onDone();
      onClose();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("kiro.socialTitle")}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.cancel")}</Button>
          {session ? (
            <Button onClick={() => void finish()} disabled={busy || !callbackURL.trim()}>
              {busy ? <RefreshCw className="size-3.5 animate-spin" /> : <LogIn className="size-3.5" />}
              {t("kiro.socialFinish")}
            </Button>
          ) : (
            <Button onClick={() => void start()} disabled={busy}>
              {busy ? <RefreshCw className="size-3.5 animate-spin" /> : <ExternalLink className="size-3.5" />}
              {t("kiro.socialStart")}
            </Button>
          )}
        </>
      }
    >
      <div className="space-y-3 text-sm">
        <p className="text-xs text-muted-foreground">{t("kiro.socialHint")}</p>
        {session ? (
          <>
            <a href={session.auth_url} target="_blank" rel="noopener noreferrer" className="block break-all font-mono text-xs text-primary underline">
              {session.auth_url}
            </a>
            <label className="block space-y-1 text-xs font-medium text-muted-foreground">
              <span>{t("kiro.socialCallback")}</span>
              <Input value={callbackURL} onChange={(event) => setCallbackURL(event.target.value)} placeholder="http://127.0.0.1:3128/oauth/callback?code=…&state=…" />
            </label>
          </>
        ) : null}
      </div>
    </Modal>
  );
}

function DeviceLoginModal({ show, onClose, onDone }: { show: boolean; onClose: () => void; onDone: () => void }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [kind, setKind] = useState<"builder" | "enterprise">("builder");
  const [startURL, setStartURL] = useState("");
  const [region, setRegion] = useState("us-east-1");
  const [pending, setPending] = useState<KiroDeviceLoginStart | null>(null);
  const [busy, setBusy] = useState(false);
  // 父组件每次渲染都会新建回调；用 ref 持有，避免轮询循环被反复重启。
  const callbacks = useRef({ onClose, onDone });
  callbacks.current = { onClose, onDone };

  useEffect(() => {
    if (!show) setPending(null);
  }, [show]);

  useEffect(() => {
    if (!pending) return;
    let stopped = false;
    const poll = async () => {
      while (!stopped && Date.now() < Date.parse(pending.expires_at)) {
        await new Promise((resolve) => window.setTimeout(resolve, Math.max(pending.interval, 1) * 1000));
        if (stopped) return;
        try {
          const result = await api.pollKiroDeviceLogin(pending.session);
          if (result.status === "completed") {
            showToast(t("kiro.loginDone", { account: result.email || `#${result.id}` }), "success");
            callbacks.current.onDone();
            callbacks.current.onClose();
            return;
          }
        } catch (error) {
          showToast(getErrorMessage(error), "error");
          setPending(null);
          return;
        }
      }
    };
    void poll();
    return () => {
      stopped = true;
    };
  }, [pending, showToast, t]);

  const start = async () => {
    setBusy(true);
    try {
      const next = await api.startKiroDeviceLogin(kind === "enterprise" ? { start_url: startURL.trim(), region: region.trim() } : {});
      setPending(next);
      window.open(next.verification_uri_complete, "_blank", "noopener");
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal
      show={show}
      title={t("kiro.deviceTitle")}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.cancel")}</Button>
          {!pending ? (
            <Button onClick={() => void start()} disabled={busy || (kind === "enterprise" && !startURL.trim())}>
              {busy ? <RefreshCw className="size-3.5 animate-spin" /> : <KeyRound className="size-3.5" />}
              {t("kiro.deviceStart")}
            </Button>
          ) : null}
        </>
      }
    >
      <div className="space-y-3 text-sm">
        {pending ? (
          <>
            <p className="text-xs text-muted-foreground">{t("kiro.deviceWaiting")}</p>
            <div className="rounded-lg border border-dashed border-border px-4 py-3 text-center font-mono text-2xl font-semibold tracking-[0.3em]">{pending.user_code}</div>
            <a href={pending.verification_uri_complete} target="_blank" rel="noopener noreferrer" className="block break-all font-mono text-xs text-primary underline">
              {pending.verification_uri_complete}
            </a>
            <p className="flex items-center gap-1.5 text-xs text-muted-foreground"><RefreshCw className="size-3 animate-spin" />{t("kiro.devicePolling")}</p>
          </>
        ) : (
          <>
            <Select
              value={kind}
              onValueChange={(value) => setKind(value as "builder" | "enterprise")}
              options={[
                { value: "builder", label: t("kiro.deviceBuilder") },
                { value: "enterprise", label: t("kiro.deviceEnterprise") },
              ]}
            />
            {kind === "enterprise" ? (
              <div className="grid gap-2 sm:grid-cols-[2fr_1fr]">
                <Input value={startURL} onChange={(event) => setStartURL(event.target.value)} placeholder="https://d-xxxxxxxxxx.awsapps.com/start" />
                <Input value={region} onChange={(event) => setRegion(event.target.value)} placeholder="us-east-1" />
              </div>
            ) : null}
            <p className="text-xs text-muted-foreground">{t("kiro.deviceHint")}</p>
          </>
        )}
      </div>
    </Modal>
  );
}

export default function KiroAccounts({ headerSlot }: { headerSlot?: ReactNode }) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(PAGE_SIZE_OPTIONS[0]);
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<StatusFilter>("all");
  const [busyIDs, setBusyIDs] = useState<Set<number>>(new Set());
  const [syncingAll, setSyncingAll] = useState(false);
  const [modal, setModal] = useState<"import" | "social" | "device" | null>(null);
  const [testing, setTesting] = useState<AccountRow | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setQuery(search.trim());
      setPage(1);
    }, 300);
    return () => window.clearTimeout(timer);
  }, [search]);

  const reload = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    try {
      const response = await api.getAccountsPage(
        { channel: "kiro", page, pageSize, search: query || undefined, status: status === "all" ? undefined : status },
        controller.signal,
      );
      setAccounts(response.accounts ?? []);
      setTotal(response.total);
      setLoadError("");
    } catch (error) {
      if (!controller.signal.aborted) setLoadError(getErrorMessage(error));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [page, pageSize, query, status]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const runRowAction = useCallback(
    async (account: AccountRow, action: () => Promise<unknown>, success: string) => {
      setBusyIDs((prev) => new Set(prev).add(account.id));
      try {
        await action();
        showToast(success, "success");
        await reload();
      } catch (error) {
        showToast(getErrorMessage(error), "error");
      } finally {
        setBusyIDs((prev) => {
          const next = new Set(prev);
          next.delete(account.id);
          return next;
        });
      }
    },
    [reload, showToast],
  );

  const syncAll = async () => {
    setSyncingAll(true);
    try {
      const result = await api.syncAllKiroAccounts();
      showToast(t("kiro.syncAllResult", { refreshed: result.refreshed, failed: result.failed }), result.failed > 0 ? "error" : "success");
      await reload();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setSyncingAll(false);
    }
  };

  const exportAccounts = async (format: "kam" | "kiro-rs", ids?: number[]) => {
    try {
      downloadNamedBlob(await api.exportKiroAccounts(format, ids), `kiro-${format}.json`);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    }
  };

  const remove = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t("kiro.deleteTitle"),
      description: t("kiro.deleteDescription", { account: accountLabel(account) }),
      confirmText: t("common.delete"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (confirmed) await runRowAction(account, () => api.deleteAccount(account.id), t("kiro.deleted"));
  };

  const renderActions = (account: AccountRow) => {
    const busy = busyIDs.has(account.id);
    const enabled = account.enabled !== false;
    return (
      <div className="flex justify-end gap-1">
        <Button size="icon-sm" variant="ghost" title={t("kiro.test")} aria-label={t("kiro.test")} onClick={() => setTesting(account)}>
          <FlaskConical className="size-3.5" />
        </Button>
        <Button size="icon-sm" variant="ghost" title={t("kiro.sync")} aria-label={t("kiro.sync")} disabled={busy}
          onClick={() => void runRowAction(account, () => api.syncKiroAccount(account.id), t("kiro.synced"))}>
          <RefreshCw className={busy ? "size-3.5 animate-spin" : "size-3.5"} />
        </Button>
        <Button size="icon-sm" variant="ghost" title={enabled ? t("kiro.disable") : t("kiro.enable")} aria-label={enabled ? t("kiro.disable") : t("kiro.enable")} disabled={busy}
          onClick={() => void runRowAction(account, () => api.toggleAccountEnabled(account.id, !enabled), enabled ? t("kiro.disabled") : t("kiro.enabled"))}>
          {enabled ? <PowerOff className="size-3.5" /> : <Power className="size-3.5" />}
        </Button>
        <Button size="icon-sm" variant="ghost" title={t("kiro.exportOne")} aria-label={t("kiro.exportOne")} onClick={() => void exportAccounts("kam", [account.id])}>
          <Download className="size-3.5" />
        </Button>
        <Button size="icon-sm" variant="ghost" className="text-muted-foreground hover:text-destructive" title={t("common.delete")} aria-label={t("common.delete")} disabled={busy} onClick={() => void remove(account)}>
          <Trash2 className="size-3.5" />
        </Button>
      </div>
    );
  };

  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  return (
    <div className="space-y-4">
      <PageHeader
        title={t("kiro.pageTitle")}
        description={t("kiro.pageSubtitle")}
        hideTitle={Boolean(headerSlot)}
        actionsBelow
        titleAdornment={headerSlot}
        onRefresh={() => void reload()}
        actions={
          <div className="flex w-full flex-wrap items-center gap-1.5 sm:gap-2 [&>button]:h-8">
            <Button size="sm" onClick={() => setModal("import")}>
              <Upload className="size-3.5" />
              {t("kiro.import")}
            </Button>
            <HeaderActionMenu
              label={t("kiro.login")}
              icon={<LogIn className="size-3.5" />}
              items={[
                { key: "social", label: t("kiro.socialTitle"), icon: <LogIn className="size-3.5" />, onSelect: () => setModal("social") },
                { key: "device", label: t("kiro.deviceTitle"), icon: <KeyRound className="size-3.5" />, onSelect: () => setModal("device") },
              ]}
            />
            <Button variant="outline" size="sm" onClick={() => void syncAll()} disabled={syncingAll || total === 0}>
              <RefreshCw className={syncingAll ? "size-3.5 animate-spin" : "size-3.5"} />
              {t("kiro.syncAll")}
            </Button>
            <HeaderActionMenu
              label={t("kiro.export")}
              icon={<Download className="size-3.5" />}
              items={[
                { key: "kam", label: t("kiro.exportKam"), icon: <Download className="size-3.5" />, disabled: total === 0, onSelect: () => void exportAccounts("kam") },
                { key: "kiro-rs", label: t("kiro.exportKiroRs"), icon: <Download className="size-3.5" />, disabled: total === 0, onSelect: () => void exportAccounts("kiro-rs") },
              ]}
            />
          </div>
        }
      />

      <Card>
        <CardContent className="space-y-3 p-3 sm:p-4">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-[200px] flex-1">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("kiro.searchPlaceholder")} className="pl-8" />
            </div>
            <Select
              value={status}
              onValueChange={(value) => {
                setStatus(value as StatusFilter);
                setPage(1);
              }}
              options={STATUS_FILTERS.map((value) => ({ value, label: t(`kiro.status.${value}`) }))}
              className="w-[140px]"
            />
          </div>

          <StateShell
            variant="section"
            loading={loading && accounts.length === 0}
            error={accounts.length === 0 ? loadError : null}
            onRetry={() => void reload()}
            isEmpty={!loading && !loadError && accounts.length === 0}
            emptyIcon={<ChannelLogo channel="kiro" size={30} />}
            emptyTitle={t("kiro.empty")}
            emptyDescription={t("kiro.emptyHint")}
          >
            <div className="hidden overflow-x-auto md:block">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("kiro.colAccount")}</TableHead>
                    <TableHead>{t("kiro.colPlan")}</TableHead>
                    <TableHead>{t("kiro.colQuota")}</TableHead>
                    <TableHead>{t("kiro.colStatus")}</TableHead>
                    <TableHead>{t("kiro.colModels")}</TableHead>
                    <TableHead className="text-right">{t("kiro.colActions")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {accounts.map((account) => {
                    const enabled = account.enabled !== false;
                    return (
                      <TableRow key={account.id}>
                        <TableCell className="min-w-[200px]">
                          <div className="truncate text-sm font-semibold">{accountLabel(account)}</div>
                          <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[11px] text-muted-foreground">
                            <span className="font-mono">#{account.id}</span>
                            {account.kiro_login_source ? <span>{account.kiro_login_source}</span> : null}
                            {account.kiro_auth_kind ? <span className="rounded bg-muted px-1 py-0.5 font-mono">{account.kiro_auth_kind}</span> : null}
                          </div>
                        </TableCell>
                        <TableCell className="whitespace-nowrap text-xs">{account.kiro_usage?.subscription_title || account.plan_type || "-"}</TableCell>
                        <TableCell className="min-w-[200px]"><KiroQuota usage={account.kiro_usage} /></TableCell>
                        <TableCell>
                          <StatusBadge status={enabled ? getAccountStatusBadgeStatus(account) : "disabled"} errorMessage={account.error_message} />
                        </TableCell>
                        <TableCell className="text-xs tabular-nums">{account.models?.length ?? 0}</TableCell>
                        <TableCell>{renderActions(account)}</TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
            <div className="space-y-2 md:hidden">
              {accounts.map((account) => (
                <div key={account.id} className="space-y-2 rounded-lg border border-border/70 p-3">
                  <div className="flex items-start justify-between gap-2">
                    <div className="min-w-0">
                      <div className="truncate text-sm font-semibold">{accountLabel(account)}</div>
                      <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[11px] text-muted-foreground">
                        <span className="font-mono">#{account.id}</span>
                        <span>{account.kiro_usage?.subscription_title || account.plan_type || "-"}</span>
                        <span>{t("kiro.colModels")} {account.models?.length ?? 0}</span>
                      </div>
                    </div>
                    <StatusBadge status={account.enabled !== false ? getAccountStatusBadgeStatus(account) : "disabled"} errorMessage={account.error_message} />
                  </div>
                  <KiroQuota usage={account.kiro_usage} />
                  {renderActions(account)}
                </div>
              ))}
            </div>
            <Pagination
              page={page}
              totalPages={totalPages}
              onPageChange={setPage}
              totalItems={total}
              pageSize={pageSize}
              onPageSizeChange={(size) => {
                setPageSize(size);
                setPage(1);
              }}
              pageSizeOptions={PAGE_SIZE_OPTIONS}
            />
          </StateShell>
        </CardContent>
      </Card>

      <ImportModal show={modal === "import"} onClose={() => setModal(null)} onDone={() => void reload()} />
      <SocialLoginModal show={modal === "social"} onClose={() => setModal(null)} onDone={() => void reload()} />
      <DeviceLoginModal show={modal === "device"} onClose={() => setModal(null)} onDone={() => void reload()} />
      {testing ? <TestConnectionModal account={testing} onClose={() => setTesting(null)} onSettled={() => void reload()} /> : null}
      {confirmDialog}
    </div>
  );
}
