import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import type { ChangeEvent, ReactNode } from "react";
import {
  Plus,
  RefreshCw,
  Trash2,
  Power,
  PowerOff,
  X,
  KeyRound,
  FileJson,
  Search,
  Sparkles,
  ExternalLink,
  Copy,
  Link2,
  Loader2,
  FlaskConical,
  Zap,
  CheckCircle2,
  XCircle,
  Rows3,
  LayoutGrid,
  Upload,
  FileText,
  RotateCcw,
  Pencil,
} from "lucide-react";
import { api, getAdminKey } from "../api";
import type {
  AccountRow,
  AccountHealthBucket,
  AddGrokAccountRequest,
  GrokSSOImportItem,
} from "../types";
import AccountHealthBar from "../components/AccountHealthBar";
import Modal from "../components/Modal";
import ModelLogo from "../components/ModelLogo";
import PageHeader from "../components/PageHeader";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "../hooks/useToast";
import { useOperationProgress } from "../hooks/useOperationProgress";
import OperationProgressToast from "../components/OperationProgressToast";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import { cn } from "@/lib/utils";

const DEFAULT_GROK_TEST_MODELS = [
  "grok-4.5",
  "grok-4",
  "grok-3-fast",
  "grok-3",
  "grok-2",
];

// 前端渲染成"限流"的账号状态集合(与 StatusBadge locale 一致)。free 账号进这些状态
// 但拿不到用量数字时,GrokUsageCell 用满格灰条兜底表意"已耗尽"。
const GROK_LIMITED_STATUSES = new Set([
  "usage_limited",
  "usage_exhausted",
  "rate_limited",
  "rate_limited_5h",
  "rate_limited_7d",
]);

// 与 Codex 账号页一致的表格/卡片双布局，选择持久化到 localStorage。
const GROK_VIEW_MODE_KEY = "codex2api:grok-accounts:view-mode";
type GrokViewMode = "table" | "grid";

function getInitialGrokViewMode(): GrokViewMode {
  try {
    const raw = window.localStorage.getItem(GROK_VIEW_MODE_KEY);
    if (raw === "grid" || raw === "table") return raw;
  } catch {
    // ignore
  }
  return "table";
}

// addMethod：Device 授权 / 粘贴 auth.json / xAI API Key / SSO 批量导入
type AddMethod = "oauth_link" | "oauth" | "api_key" | "sso";
type StatusFilter = "all" | "active" | "disabled" | "error";
type AuthFilter = "all" | "oauth" | "api_key";
type DeviceStep = "idle" | "waiting";

const EMPTY_FORM: AddGrokAccountRequest = {
  auth_kind: "oauth",
  auth_json: "",
  api_key: "",
  base_url: "",
  models: [],
  proxy_url: "",
};

async function copyTextToClipboard(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.left = "-9999px";
  document.body.appendChild(ta);
  ta.select();
  document.execCommand("copy");
  document.body.removeChild(ta);
}

// Grok 官方默认上游；base_url 为默认值时列表不显示（无信息量），仅自定义上游才展示。
const GROK_DEFAULT_HOSTS = new Set([
  "cli-chat-proxy.grok.com/v1",
  "api.x.ai/v1",
]);

function shortHost(raw?: string | null): string {
  const value = (raw ?? "").trim();
  if (!value) return "";
  let host = "";
  try {
    const url = new URL(value.includes("://") ? value : `https://${value}`);
    host = url.host + (url.pathname && url.pathname !== "/" ? url.pathname.replace(/\/$/, "") : "");
  } catch {
    host = value.replace(/^https?:\/\//, "").replace(/\/$/, "");
  }
  return GROK_DEFAULT_HOSTS.has(host) ? "" : host;
}

function isPremiumPlan(plan?: string | null): boolean {
  const p = (plan ?? "").trim().toLowerCase();
  return Boolean(p) && p !== "api" && p !== "free" && p !== "unknown";
}

// 套餐徽章：付费档（SuperGrok / Heavy）琥珀，free 绿色，api/unknown 中性。
// 表格用常规尺寸、空值显示占位「—」；卡片用 compact 尺寸、空值不渲染。
function GrokPlanBadge({
  plan,
  compact = false,
  className,
}: {
  plan?: string | null;
  compact?: boolean;
  className?: string;
}) {
  const raw = (plan ?? "").trim();
  const key = raw.toLowerCase();
  if (!raw) {
    return compact ? null : (
      <span className="text-[12px] text-muted-foreground">—</span>
    );
  }
  const tone = isPremiumPlan(raw)
    ? "bg-amber-50 text-amber-800 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-300 dark:ring-amber-400/20"
    : key === "free"
      ? "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-300 dark:ring-emerald-400/20"
      : "bg-muted text-muted-foreground ring-border";
  return (
    <span
      className={cn(
        "inline-flex items-center whitespace-nowrap rounded-md ring-1 ring-inset",
        compact
          ? "px-1.5 py-0.5 text-[10px] font-semibold"
          : "px-2 py-1 text-xs font-semibold",
        tone,
        className,
      )}
    >
      {raw}
    </span>
  );
}

function parseModelTokens(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function accountLabel(account: AccountRow): string {
  return account.name || account.email || `#${account.id}`;
}

function isAccountError(account: AccountRow): boolean {
  return account.status === "error" || account.status === "unauthorized";
}

function isAccountActive(account: AccountRow): boolean {
  return account.enabled !== false && !isAccountError(account);
}

export default function GrokAccounts({
  headerSlot,
}: {
  // headerSlot 由账号管理页注入 Codex/Grok 顶部切换器，渲染在标题旁。
  headerSlot?: ReactNode;
} = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  // 批量测试的右上角进度浮层，与 Codex 账号页共用同一实现。
  const { operationProgress, runStreamingOperation, closeOperationProgress } =
    useOperationProgress();

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [healthBars, setHealthBars] = useState<
    Record<string, AccountHealthBucket[]>
  >({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<number | null>(null);

  const [showAdd, setShowAdd] = useState(false);
  const [addMethod, setAddMethod] = useState<AddMethod>("oauth_link");
  const [form, setForm] = useState<AddGrokAccountRequest>(EMPTY_FORM);
  const [modelDraft, setModelDraft] = useState("");
  const [modelsLoading, setModelsLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  // SSO 批量导入：粘贴 sso token（JSON 或每行一个），后端自动转 Build 账号
  const [ssoTokens, setSsoTokens] = useState("");
  const [ssoImporting, setSsoImporting] = useState(false);
  const [ssoResult, setSsoResult] = useState<{
    total: number;
    imported: number;
    failed: number;
    items: GrokSSOImportItem[];
  } | null>(null);

  // 导入入口：选择器弹窗 + 三种来源（JSON 凭据文件 / sso.txt / refreshtoken.txt）
  const [showImportPicker, setShowImportPicker] = useState(false);
  const authFileInputRef = useRef<HTMLInputElement | null>(null);
  const ssoFileInputRef = useRef<HTMLInputElement | null>(null);
  const refreshFileInputRef = useRef<HTMLInputElement | null>(null);
  const [importBusy, setImportBusy] = useState(false);
  const [importResult, setImportResult] = useState<{
    total: number;
    imported: number;
    failed: number;
    items: GrokSSOImportItem[];
  } | null>(null);

  // Device Code 授权：start → 展示 user_code → 自动 poll
  const [deviceStep, setDeviceStep] = useState<DeviceStep>("idle");
  const [deviceSession, setDeviceSession] = useState<{
    session_id: string;
    user_code: string;
    verification_url: string;
    interval: number;
  } | null>(null);
  const [deviceStarting, setDeviceStarting] = useState(false);
  const [devicePolling, setDevicePolling] = useState(false);
  const devicePollTimer = useRef<number | null>(null);

  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [batchTesting, setBatchTesting] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [batchBusy, setBatchBusy] = useState(false);
  const selectAllRef = useRef<HTMLInputElement>(null);

  const [searchQuery, setSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [authFilter, setAuthFilter] = useState<AuthFilter>("all");
  const [viewMode, setViewMode] = useState<GrokViewMode>(getInitialGrokViewMode);

  useEffect(() => {
    try {
      window.localStorage.setItem(GROK_VIEW_MODE_KEY, viewMode);
    } catch {
      // ignore
    }
  }, [viewMode]);

  const reload = useCallback(async () => {
    try {
      const res = await api.getAccounts();
      const grokAccounts = res.accounts.filter((a) => a.grok_api);
      setAccounts(grokAccounts);
      // 选择集只保留仍然存在的账号，避免已删除账号残留在批量选择里。
      setSelected((prev) => {
        if (prev.size === 0) return prev;
        const alive = new Set(grokAccounts.map((a) => a.id));
        const next = new Set<number>();
        for (const id of prev) if (alive.has(id)) next.add(id);
        return next.size === prev.size ? prev : next;
      });
      setError(null);
    } catch (err) {
      const message = getErrorMessage(err);
      setError(message);
      showToast(message, "error");
    }
    // 健康采样条 best-effort，失败不影响列表
    try {
      const bars = await api.getAccountHealthBars();
      setHealthBars(bars.buckets ?? {});
    } catch {
      /* ignore */
    }
    setLoading(false);
  }, [showToast]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const stats = useMemo(() => {
    const total = accounts.length;
    const active = accounts.filter(isAccountActive).length;
    const disabled = accounts.filter((a) => a.enabled === false).length;
    const errored = accounts.filter(isAccountError).length;
    const oauth = accounts.filter((a) => a.grok_auth_kind === "oauth").length;
    const apiKey = accounts.filter((a) => a.grok_auth_kind === "api_key").length;
    return { total, active, disabled, errored, oauth, apiKey };
  }, [accounts]);

  const filteredAccounts = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    return accounts.filter((account) => {
      if (statusFilter === "active" && !isAccountActive(account)) return false;
      if (statusFilter === "disabled" && account.enabled !== false) return false;
      if (statusFilter === "error" && !isAccountError(account)) return false;
      if (authFilter === "oauth" && account.grok_auth_kind !== "oauth") return false;
      if (authFilter === "api_key" && account.grok_auth_kind !== "api_key")
        return false;
      if (!q) return true;
      const haystack = [
        account.name,
        account.email,
        String(account.id),
        ...(account.models ?? []),
        account.base_url,
      ]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      return haystack.includes(q);
    });
  }, [accounts, authFilter, searchQuery, statusFilter]);

  // 批量选择：全选/半选状态按当前过滤结果计算。
  const filteredIds = useMemo(
    () => filteredAccounts.map((a) => a.id),
    [filteredAccounts],
  );
  const filteredSelectedCount = useMemo(
    () => filteredIds.reduce((n, id) => n + (selected.has(id) ? 1 : 0), 0),
    [filteredIds, selected],
  );
  const allFilteredSelected =
    filteredIds.length > 0 && filteredSelectedCount === filteredIds.length;
  const someFilteredSelected =
    filteredSelectedCount > 0 && !allFilteredSelected;

  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = someFilteredSelected;
    }
  }, [someFilteredSelected]);

  const toggleSelect = useCallback((id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleSelectAll = useCallback(() => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (allFilteredSelected) {
        for (const id of filteredIds) next.delete(id);
      } else {
        for (const id of filteredIds) next.add(id);
      }
      return next;
    });
  }, [allFilteredSelected, filteredIds]);

  const clearSelection = useCallback(() => setSelected(new Set()), []);

  const credentialReady =
    addMethod === "api_key"
      ? Boolean(form.api_key?.trim())
      : addMethod === "oauth"
        ? Boolean(form.auth_json?.trim())
        : false;

  const stopDevicePoll = useCallback(() => {
    if (devicePollTimer.current != null) {
      window.clearTimeout(devicePollTimer.current);
      devicePollTimer.current = null;
    }
    setDevicePolling(false);
  }, []);

  const resetAddForm = () => {
    stopDevicePoll();
    setForm(EMPTY_FORM);
    setModelDraft("");
    setAddMethod("oauth_link");
    setDeviceStep("idle");
    setDeviceSession(null);
    setDeviceStarting(false);
    setSsoTokens("");
    setSsoResult(null);
    setSsoImporting(false);
  };

  useEffect(() => () => stopDevicePoll(), [stopDevicePoll]);

  const addModels = (raw: string) => {
    const tokens = parseModelTokens(raw);
    if (tokens.length === 0) return;
    setForm((f) => {
      const seen = new Set((f.models ?? []).map((m) => m.toLowerCase()));
      const merged = [...(f.models ?? [])];
      for (const tok of tokens) {
        if (!seen.has(tok.toLowerCase())) {
          seen.add(tok.toLowerCase());
          merged.push(tok);
        }
      }
      return { ...f, models: merged };
    });
    setModelDraft("");
  };

  const removeModel = (model: string) =>
    setForm((f) => ({
      ...f,
      models: (f.models ?? []).filter((m) => m !== model),
    }));

  const handleFetchModels = async () => {
    if (!credentialReady) return;
    setModelsLoading(true);
    try {
      const payload: AddGrokAccountRequest = {
        ...form,
        auth_kind: addMethod === "api_key" ? "api_key" : "oauth",
      };
      const res = await api.fetchGrokModels(payload);
      setForm((f) => ({ ...f, models: res.models ?? [] }));
      showToast(t("grok.modelsFetched", { count: (res.models ?? []).length }));
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setModelsLoading(false);
    }
  };

  // 编辑已存在的 Grok 账号：声明模型白名单 / base_url / 代理 / 映射。
  // 后端 UpdateGrokAccount 会整体重写这几项，所以表单需回填当前值再整体提交，避免清空。
  const [editAccount, setEditAccount] = useState<AccountRow | null>(null);
  const [editForm, setEditForm] = useState<{
    models: string[];
    base_url: string;
    model_mapping: string;
    proxy_url: string;
  }>({ models: [], base_url: "", model_mapping: "", proxy_url: "" });
  const [editModelDraft, setEditModelDraft] = useState("");
  const [editSubmitting, setEditSubmitting] = useState(false);

  const openEdit = (account: AccountRow) => {
    setEditAccount(account);
    setEditForm({
      models: account.models ?? [],
      base_url: account.base_url ?? "",
      model_mapping: account.model_mapping ?? "",
      proxy_url: account.proxy_url ?? "",
    });
    setEditModelDraft("");
  };

  const mergeModels = (existing: string[], incoming: string[]): string[] => {
    const seen = new Set(existing.map((m) => m.toLowerCase()));
    const merged = [...existing];
    for (const tok of incoming) {
      if (!seen.has(tok.toLowerCase())) {
        seen.add(tok.toLowerCase());
        merged.push(tok);
      }
    }
    return merged;
  };

  const editAddModels = (raw: string) => {
    const tokens = parseModelTokens(raw);
    if (tokens.length === 0) return;
    setEditForm((f) => ({ ...f, models: mergeModels(f.models, tokens) }));
    setEditModelDraft("");
  };

  const editRemoveModel = (model: string) =>
    setEditForm((f) => ({ ...f, models: f.models.filter((m) => m !== model) }));

  const editFillCommonModels = () =>
    setEditForm((f) => ({
      ...f,
      models: mergeModels(f.models, DEFAULT_GROK_TEST_MODELS),
    }));

  const handleSaveEdit = async () => {
    if (!editAccount) return;
    setEditSubmitting(true);
    try {
      await api.updateGrokAccount(editAccount.id, {
        auth_kind: (editAccount.grok_auth_kind ??
          "oauth") as AddGrokAccountRequest["auth_kind"],
        models: editForm.models,
        base_url: editForm.base_url.trim(),
        model_mapping: editForm.model_mapping.trim(),
        proxy_url: editForm.proxy_url.trim(),
      });
      showToast(t("grok.editSaved"));
      setEditAccount(null);
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setEditSubmitting(false);
    }
  };

  const handleAdd = async () => {
    if (addMethod === "oauth_link") return;
    if (!credentialReady) return;
    setSubmitting(true);
    try {
      await api.addGrokAccount({
        ...form,
        auth_kind: addMethod === "api_key" ? "api_key" : "oauth",
      });
      showToast(t("grok.addSuccess"));
      setShowAdd(false);
      resetAddForm();
      void reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSubmitting(false);
    }
  };

  const handleImportSSO = async () => {
    if (!ssoTokens.trim()) return;
    setSsoImporting(true);
    setSsoResult(null);
    try {
      const res = await api.importGrokSSO({
        tokens: ssoTokens,
        base_url: form.base_url?.trim() || undefined,
        models: form.models?.length ? form.models : undefined,
        proxy_url: form.proxy_url?.trim() || undefined,
      });
      setSsoResult(res);
      if (res.imported > 0) {
        showToast(t("grok.ssoImportDone", { imported: res.imported, total: res.total }));
        void reload();
      }
      if (res.imported === res.total) {
        // 全部成功：清空输入，方便继续导入下一批
        setSsoTokens("");
      }
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSsoImporting(false);
    }
  };

  // runImport 统一跑一次导入调用：置忙、展示结果、成功后刷新列表。
  const runImport = async (
    fn: () => Promise<{
      total: number;
      imported: number;
      failed: number;
      items: GrokSSOImportItem[];
    }>,
  ) => {
    setImportBusy(true);
    setImportResult(null);
    setShowImportPicker(false);
    try {
      const res = await fn();
      setImportResult(res);
      if (res.imported > 0) {
        showToast(
          t("grok.fileImportDone", { imported: res.imported, total: res.total }),
        );
        void reload();
      }
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setImportBusy(false);
    }
  };

  // JSON 凭据文件（CPA / auth.json，可多选）
  const handleImportAuthFiles = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const files = await Promise.all(
      Array.from(fileList).map((file) => file.text()),
    );
    if (authFileInputRef.current) authFileInputRef.current.value = "";
    await runImport(() => api.batchImportGrokAccounts({ files }));
  };

  // sso.txt（每行一个 sso token，自动转 Build 账号）
  const handleImportSsoFile = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const text = await fileList[0].text();
    if (ssoFileInputRef.current) ssoFileInputRef.current.value = "";
    await runImport(() => api.importGrokSSO({ tokens: text }));
  };

  // refreshtoken.txt（每行一个 refresh_token，刷出 OAuth 账号）
  const handleImportRefreshFile = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const text = await fileList[0].text();
    if (refreshFileInputRef.current) refreshFileInputRef.current.value = "";
    await runImport(() => api.importGrokRefreshTokens({ tokens: text }));
  };

  const scheduleDevicePoll = useCallback(
    (sessionId: string, intervalSec: number) => {
      stopDevicePoll();
      const delay = Math.max(3, intervalSec) * 1000;
      devicePollTimer.current = window.setTimeout(() => {
        void (async () => {
          setDevicePolling(true);
          try {
            const result = await api.pollGrokDeviceAuth({
              session_id: sessionId,
              proxy_url: form.proxy_url?.trim() || undefined,
              name: form.name?.trim() || undefined,
            });
            if (result.status === "authorized") {
              stopDevicePoll();
              showToast(
                result.email
                  ? t("grok.oauthSuccess", { email: result.email })
                  : t("grok.addSuccess"),
              );
              setShowAdd(false);
              resetAddForm();
              void reload();
              return;
            }
            // pending — continue
            const nextInterval =
              result.slow_down
                ? Math.max(intervalSec + 5, 10)
                : result.interval ?? intervalSec;
            setDeviceSession((prev) =>
              prev
                ? { ...prev, interval: nextInterval, user_code: result.user_code || prev.user_code }
                : prev,
            );
            scheduleDevicePoll(sessionId, nextInterval);
          } catch (err) {
            stopDevicePoll();
            showToast(getErrorMessage(err), "error");
            setDeviceStep("idle");
            setDeviceSession(null);
          } finally {
            setDevicePolling(false);
          }
        })();
      }, delay);
    },
    [form.name, form.proxy_url, reload, showToast, stopDevicePoll, t],
  );

  const handleDeviceStart = async () => {
    setDeviceStarting(true);
    stopDevicePoll();
    try {
      const result = await api.startGrokDeviceAuth({
        proxy_url: form.proxy_url?.trim() || undefined,
        name: form.name?.trim() || undefined,
        base_url: form.base_url?.trim() || undefined,
        models: form.models?.length ? form.models : undefined,
      });
      const session = {
        session_id: result.session_id,
        user_code: result.user_code,
        verification_url: result.verification_url,
        interval: result.interval || 5,
      };
      setDeviceSession(session);
      setDeviceStep("waiting");
      // 自动打开验证页
      window.open(result.verification_url, "_blank", "noopener,noreferrer");
      scheduleDevicePoll(session.session_id, session.interval);
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setDeviceStarting(false);
    }
  };

  const handleDeviceCopyCode = async () => {
    if (!deviceSession?.user_code) return;
    try {
      await copyTextToClipboard(deviceSession.user_code);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const handleDeviceRestart = async () => {
    stopDevicePoll();
    setDeviceSession(null);
    setDeviceStep("idle");
    await handleDeviceStart();
  };

  const handleToggleEnabled = async (account: AccountRow) => {
    setBusyId(account.id);
    const next = account.enabled === false;
    try {
      await api.toggleAccountEnabled(account.id, next);
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleRefresh = async (account: AccountRow) => {
    setBusyId(account.id);
    try {
      await api.refreshAccount(account.id);
      showToast(t("grok.refreshDone"));
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleDelete = async (account: AccountRow) => {
    if (!window.confirm(t("grok.deleteConfirm"))) return;
    setBusyId(account.id);
    try {
      await api.deleteAccount(account.id);
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBusyId(null);
    }
  };

  const handleBatchTest = async (testIds?: number[]) => {
    if (accounts.length === 0) return;
    setBatchTesting(true);
    try {
      // 未指定则测当前 Grok 账号全集——必须显式传 ids，否则后端会连 Codex 账号一起测。
      const ids =
        testIds && testIds.length > 0 ? testIds : accounts.map((a) => a.id);
      // 流式调用驱动右上角进度浮层（与 Codex 账号页一致）。
      const result = await runStreamingOperation(
        "/accounts/batch-test?stream=true",
        { ids },
        t("accounts.batchTestProgressTitle"),
      );
      showToast(
        t("accounts.batchTestDone", {
          success: result?.success ?? 0,
          banned: result?.banned ?? 0,
          rateLimited: result?.rate_limited ?? 0,
          failed: result?.failed ?? 0,
        }),
      );
      await reload();
    } catch (err) {
      showToast(
        t("accounts.batchTestFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchTesting(false);
    }
  };

  const selectedIds = useMemo(() => Array.from(selected), [selected]);

  const handleBatchRefresh = async () => {
    // 仅 OAuth 账号可刷新（API Key 无 refresh_token），先过滤避免徒增失败计数。
    const oauthIds = filteredAccounts
      .filter((a) => selected.has(a.id) && a.grok_auth_kind === "oauth")
      .map((a) => a.id);
    if (oauthIds.length === 0) {
      showToast(t("grok.batchNoOAuth"), "error");
      return;
    }
    setBatchBusy(true);
    try {
      const res = await api.batchRefreshAccounts(oauthIds);
      showToast(
        t("accounts.batchRefreshDone", {
          success: res.success ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      clearSelection();
      await reload();
    } catch (err) {
      showToast(
        t("accounts.batchRefreshFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchBusy(false);
    }
  };

  const handleBatchResetStatus = async () => {
    if (selectedIds.length === 0) return;
    setBatchBusy(true);
    try {
      const res = await api.batchResetStatus(selectedIds);
      showToast(
        t("accounts.batchResetStatusDone", {
          success: res.success ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      clearSelection();
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBatchBusy(false);
    }
  };

  const handleBatchEnabled = async (enabled: boolean) => {
    if (selectedIds.length === 0) return;
    setBatchBusy(true);
    try {
      await api.batchUpdateAccounts({ ids: selectedIds, enabled });
      clearSelection();
      await reload();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setBatchBusy(false);
    }
  };

  const handleBatchDelete = async () => {
    if (selectedIds.length === 0) return;
    if (!window.confirm(t("accounts.batchDeleteDesc", { count: selectedIds.length })))
      return;
    setBatchBusy(true);
    try {
      const res = await api.batchDeleteAccounts(selectedIds);
      showToast(
        t("accounts.batchDeleteDone", {
          success: res.success ?? res.deleted ?? 0,
          fail: res.failed ?? 0,
        }),
      );
      clearSelection();
      await reload();
    } catch (err) {
      showToast(
        t("accounts.batchDeleteFailed", { error: getErrorMessage(err) }),
        "error",
      );
    } finally {
      setBatchBusy(false);
    }
  };

  return (
    <div className="relative @container/accounts">
      <OperationProgressToast
        progress={operationProgress}
        onClose={closeOperationProgress}
      />
      <StateShell
        variant="page"
        loading={loading}
        error={error}
        onRetry={() => void reload()}
        loadingTitle={t("grok.loadingTitle")}
        loadingDescription={t("grok.loadingDesc")}
        errorTitle={t("grok.errorTitle")}
      >
        <PageHeader
          title={t("grok.pageTitle")}
          description={t("grok.pageSubtitle")}
          onRefresh={() => void reload()}
          hideTitle
          titleAdornment={headerSlot}
          actions={
            <div className="flex flex-wrap items-center gap-1.5">
              <Button size="sm" onClick={() => setShowAdd(true)}>
                <Plus className="size-3.5" />
                {t("grok.addAccount")}
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={importBusy}
                onClick={() => setShowImportPicker(true)}
              >
                {importBusy ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <Upload className="size-3.5" />
                )}
                <span className="hidden sm:inline">
                  {importBusy
                    ? t("grok.fileImporting")
                    : t("grok.fileImportBtn")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchTesting || accounts.length === 0}
                onClick={() => void handleBatchTest()}
              >
                <FlaskConical
                  className={cn("size-3.5", batchTesting && "animate-pulse")}
                />
                <span className="hidden sm:inline">
                  {batchTesting
                    ? t("accounts.batchTesting")
                    : t("accounts.testConnection")}
                </span>
              </Button>
            </div>
          }
        />

        <div className="mb-4 grid grid-cols-2 gap-2 sm:gap-3 xl:grid-cols-4">
          <CompactStat
            label={t("grok.statTotal")}
            chipLabel={t("accounts.filterAll")}
            value={stats.total}
            tone="neutral"
            active={statusFilter === "all"}
            onClick={() => setStatusFilter("all")}
          />
          <CompactStat
            label={t("grok.statActive")}
            chipLabel={t("accounts.filterNormal")}
            value={stats.active}
            tone="success"
            active={statusFilter === "active"}
            onClick={() => setStatusFilter("active")}
          />
          <CompactStat
            label={t("grok.statOAuth")}
            chipLabel={t("grok.authKindOAuth")}
            value={stats.oauth}
            tone="neutral"
            active={authFilter === "oauth"}
            onClick={() =>
              setAuthFilter((prev) => (prev === "oauth" ? "all" : "oauth"))
            }
          />
          <CompactStat
            label={t("grok.statApiKey")}
            chipLabel={t("grok.authKindApiKey")}
            value={stats.apiKey}
            tone="neutral"
            active={authFilter === "api_key"}
            onClick={() =>
              setAuthFilter((prev) => (prev === "api_key" ? "all" : "api_key"))
            }
          />
        </div>

        <div className="toolbar-surface mb-3 flex flex-col gap-2.5">
          <div className="flex items-center gap-1.5 overflow-x-auto [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
            <span className="shrink-0 whitespace-nowrap text-[12px] font-semibold text-foreground">
              {t("accounts.filter")}
            </span>
            {(
              [
                ["all", t("accounts.filterAll"), stats.total],
                ["active", t("accounts.filterNormal"), stats.active],
                ["disabled", t("accounts.filterDisabled"), stats.disabled],
                ["error", t("accounts.filterError"), stats.errored],
              ] as const
            ).map(([key, label, count]) => (
              <button
                key={key}
                type="button"
                onClick={() => setStatusFilter(key)}
                className={cn(
                  "shrink-0 whitespace-nowrap rounded-lg px-2.5 py-1.5 text-[12px] font-semibold transition-colors",
                  statusFilter === key
                    ? "bg-primary text-primary-foreground"
                    : "bg-muted/50 text-muted-foreground hover:bg-muted",
                )}
              >
                {label} {count}
              </button>
            ))}
          </div>

          <div className="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
            <div className="relative w-full shrink-0 sm:w-64">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="h-9 rounded-lg pl-9 text-[13px] sm:h-8"
                placeholder={t("grok.searchPlaceholder")}
                value={searchQuery}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  setSearchQuery(e.target.value)
                }
              />
            </div>
            <div className="flex max-w-full shrink-0 items-center gap-0.5 overflow-x-auto rounded-lg border border-border bg-muted/30 p-0.5">
              {(
                [
                  ["all", t("accounts.filterAll")],
                  ["oauth", t("grok.authKindOAuth")],
                  ["api_key", t("grok.authKindApiKey")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setAuthFilter(key)}
                  className={cn(
                    "shrink-0 whitespace-nowrap rounded-md px-2.5 py-1.5 text-[12px] font-medium transition-colors",
                    authFilter === key
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
            <div className="hidden shrink-0 items-center rounded-md border border-border bg-muted/50 p-0.5 lg:inline-flex lg:ml-auto">
              {(
                [
                  ["table", Rows3, t("accounts.viewModeTable")],
                  ["grid", LayoutGrid, t("accounts.viewModeGrid")],
                ] as const
              ).map(([key, Icon, label]) => (
                <button
                  key={key}
                  type="button"
                  onClick={() => setViewMode(key)}
                  title={label}
                  aria-label={label}
                  aria-pressed={viewMode === key}
                  className={cn(
                    "inline-flex items-center gap-1 rounded-sm px-2 py-1 text-[12px] font-medium transition-colors",
                    viewMode === key
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <Icon className="size-3.5" />
                  {label}
                </button>
              ))}
            </div>
          </div>
        </div>

        {selected.size > 0 && (
          <div className="sticky top-2 z-20 mb-3 flex items-center justify-between gap-3 rounded-xl border border-primary/20 bg-card/95 px-3 py-2 text-sm shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
            <span className="font-semibold text-primary">
              {t("common.selected", { count: selected.size })}
            </span>
            <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchRefresh()}
              >
                <RefreshCw
                  className={cn("size-3.5", batchBusy && "animate-spin")}
                />
                <span className="hidden sm:inline">
                  {t("accounts.batchRefresh")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchTest(selectedIds)}
              >
                <FlaskConical className="size-3.5" />
                <span className="hidden sm:inline">
                  {batchTesting
                    ? t("accounts.batchTesting")
                    : t("accounts.batchTest")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchEnabled(true)}
              >
                <Power className="size-3.5" />
                <span className="hidden sm:inline">{t("accounts.enable")}</span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchEnabled(false)}
              >
                <PowerOff className="size-3.5" />
                <span className="hidden sm:inline">{t("accounts.disable")}</span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchResetStatus()}
              >
                <RotateCcw className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("accounts.batchResetStatus")}
                </span>
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                disabled={batchBusy || batchTesting}
                onClick={() => void handleBatchDelete()}
              >
                <Trash2 className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("accounts.batchDelete")}
                </span>
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={batchBusy}
                onClick={clearSelection}
              >
                <X className="size-3.5" />
                <span className="hidden sm:inline">
                  {t("accounts.clearSelection")}
                </span>
              </Button>
            </div>
          </div>
        )}

        <StateShell
          variant="section"
          isEmpty={filteredAccounts.length === 0}
          emptyTitle={
            accounts.length === 0
              ? t("grok.emptyTitle")
              : t("grok.noMatchTitle")
          }
          emptyDescription={
            accounts.length === 0
              ? t("grok.emptyDesc")
              : t("grok.noMatchDesc")
          }
          action={
            accounts.length === 0 ? (
              <div className="flex flex-wrap items-center justify-center gap-1.5">
                <Button onClick={() => setShowAdd(true)}>
                  <Plus className="size-3.5" />
                  {t("grok.addAccount")}
                </Button>
                <Button
                  variant="outline"
                  disabled={importBusy}
                  onClick={() => setShowImportPicker(true)}
                >
                  {importBusy ? (
                    <Loader2 className="size-3.5 animate-spin" />
                  ) : (
                    <Upload className="size-3.5" />
                  )}
                  {importBusy
                    ? t("grok.fileImporting")
                    : t("grok.fileImportBtn")}
                </Button>
              </div>
            ) : (
              <Button
                variant="outline"
                onClick={() => {
                  setSearchQuery("");
                  setStatusFilter("all");
                  setAuthFilter("all");
                }}
              >
                {t("grok.clearFilters")}
              </Button>
            )
          }
        >
          {viewMode === "table" ? (
            <div className="data-table-shell hidden lg:block">
              <Table className="[&_td]:px-2.5 [&_th]:px-2.5 [&_td]:py-4">
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-9">
                      <input
                        ref={selectAllRef}
                        type="checkbox"
                        className="size-4 cursor-pointer rounded border-border accent-primary"
                        aria-label={t("accounts.selectAll")}
                        title={t("accounts.selectAll")}
                        checked={allFilteredSelected}
                        onChange={toggleSelectAll}
                      />
                    </TableHead>
                    <TableHead className="w-10 text-[13px] font-semibold">
                      {t("accounts.sequence")}
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">
                      {t("grok.colAccount")}
                    </TableHead>
                    <TableHead className="text-center text-[13px] font-semibold">
                      {t("grok.colPlan")}
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">
                      {t("grok.colStatus")}
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">
                      {t("accounts.requests")}
                    </TableHead>
                    <TableHead className="min-w-[170px] text-[13px] font-semibold">
                      {t("accounts.usage")}
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">
                      {t("grok.colModels")}
                    </TableHead>
                    <TableHead className="text-[13px] font-semibold">
                      {t("grok.colUpdated")}
                    </TableHead>
                    <TableHead className="text-right text-[13px] font-semibold">
                      {t("accounts.actions")}
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filteredAccounts.map((account, index) => (
                    <GrokAccountTableRow
                      key={account.id}
                      account={account}
                      sequence={index + 1}
                      busy={busyId === account.id}
                      batchTesting={batchTesting}
                      selected={selected.has(account.id)}
                      onToggleSelect={() => toggleSelect(account.id)}
                      healthBuckets={healthBars[String(account.id)]}
                      onTest={() => setTestingAccount(account)}
                      onRefresh={() => void handleRefresh(account)}
                      onToggleEnabled={() => void handleToggleEnabled(account)}
                      onEdit={() => openEdit(account)}
                      onDelete={() => void handleDelete(account)}
                      onUsageRefreshed={() => void reload()}
                    />
                  ))}
                </TableBody>
              </Table>
            </div>
          ) : null}
          <div
            className={cn(
              "grid grid-cols-1 gap-3 xl:grid-cols-2",
              viewMode === "table" && "lg:hidden",
            )}
          >
            {filteredAccounts.map((account, index) => (
              <GrokAccountCard
                key={account.id}
                account={account}
                sequence={index + 1}
                busy={busyId === account.id}
                batchTesting={batchTesting}
                selected={selected.has(account.id)}
                onToggleSelect={() => toggleSelect(account.id)}
                onTest={() => setTestingAccount(account)}
                onRefresh={() => void handleRefresh(account)}
                onToggleEnabled={() => void handleToggleEnabled(account)}
                onEdit={() => openEdit(account)}
                onDelete={() => void handleDelete(account)}
                onUsageRefreshed={() => void reload()}
              />
            ))}
          </div>
        </StateShell>
      </StateShell>

      <Modal
        show={showAdd}
        title={t("grok.addTitle")}
        contentClassName="sm:max-w-[560px]"
        onClose={() => {
          setShowAdd(false);
          resetAddForm();
        }}
        footer={
          <>
            <Button
              variant="outline"
              onClick={() => {
                setShowAdd(false);
                resetAddForm();
              }}
            >
              {t("common.cancel")}
            </Button>
            {addMethod === "oauth_link" ? (
              deviceStep === "idle" ? (
                <Button
                  onClick={() => void handleDeviceStart()}
                  disabled={deviceStarting}
                >
                  {deviceStarting
                    ? t("grok.oauthGenerating")
                    : t("grok.oauthGenerateBtn")}
                </Button>
              ) : (
                <Button
                  variant="outline"
                  onClick={() => void handleDeviceRestart()}
                  disabled={deviceStarting}
                >
                  {deviceStarting
                    ? t("grok.oauthGenerating")
                    : t("grok.oauthRestart")}
                </Button>
              )
            ) : addMethod === "sso" ? (
              <Button
                onClick={() => void handleImportSSO()}
                disabled={ssoImporting || !ssoTokens.trim()}
              >
                {ssoImporting ? (
                  <>
                    <Loader2 className="size-3.5 animate-spin" />
                    {t("grok.ssoImporting")}
                  </>
                ) : (
                  <>
                    <Upload className="size-3.5" />
                    {t("grok.ssoImportBtn")}
                  </>
                )}
              </Button>
            ) : (
              <Button
                onClick={() => void handleAdd()}
                disabled={submitting || !credentialReady}
              >
                {submitting ? t("grok.adding") : t("grok.submit")}
              </Button>
            )}
          </>
        }
      >
        <div className="space-y-4">
          <div>
            <label className="mb-2 block text-sm font-medium text-muted-foreground">
              {t("grok.authKind")}
            </label>
            <div className="grid grid-cols-2 gap-1 rounded-xl border border-border bg-muted/30 p-1 sm:grid-cols-4">
              {(
                [
                  {
                    kind: "oauth_link" as AddMethod,
                    icon: Link2,
                    label: t("grok.authKindLink"),
                  },
                  {
                    kind: "oauth" as AddMethod,
                    icon: FileJson,
                    label: t("grok.authKindOAuth"),
                  },
                  {
                    kind: "api_key" as AddMethod,
                    icon: KeyRound,
                    label: t("grok.authKindApiKey"),
                  },
                  {
                    kind: "sso" as AddMethod,
                    icon: Upload,
                    label: t("grok.authKindSSO"),
                  },
                ] as const
              ).map(({ kind, icon: Icon, label }) => (
                <button
                  key={kind}
                  type="button"
                  onClick={() => {
                    setAddMethod(kind);
                    if (kind !== "oauth_link") {
                      stopDevicePoll();
                      setDeviceStep("idle");
                      setDeviceSession(null);
                    }
                    if (kind !== "sso") {
                      setSsoResult(null);
                    }
                    setForm((f) => ({
                      ...f,
                      auth_kind: kind === "api_key" ? "api_key" : "oauth",
                    }));
                  }}
                  className={cn(
                    "inline-flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-sm font-semibold transition-all",
                    addMethod === kind
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  <Icon className="size-3.5" />
                  <span className="truncate">{label}</span>
                </button>
              ))}
            </div>
          </div>

          {addMethod === "oauth_link" ? (
            <div className="space-y-4">
              {deviceStep === "idle" ? (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="mb-1 font-semibold text-foreground">
                      {t("grok.oauthStep1Title")}
                    </p>
                    <p>{t("grok.oauthStep1Desc")}</p>
                  </div>
                  <div>
                    <label className="mb-2 block text-sm font-medium text-muted-foreground">
                      {t("grok.nameLabel")}
                    </label>
                    <Input
                      placeholder={t("grok.namePlaceholder")}
                      value={form.name ?? ""}
                      onChange={(e: ChangeEvent<HTMLInputElement>) =>
                        setForm((f) => ({ ...f, name: e.target.value }))
                      }
                    />
                  </div>
                  <div>
                    <label className="mb-2 block text-sm font-medium text-muted-foreground">
                      {t("grok.proxyUrl")}
                    </label>
                    <Input
                      placeholder="http://user:pass@host:port"
                      value={form.proxy_url ?? ""}
                      onChange={(e: ChangeEvent<HTMLInputElement>) =>
                        setForm((f) => ({ ...f, proxy_url: e.target.value }))
                      }
                    />
                  </div>
                </>
              ) : (
                <>
                  <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                    <p className="mb-1 font-semibold text-foreground">
                      {t("grok.oauthStep2Title")}
                    </p>
                    <p>{t("grok.oauthStep2Desc")}</p>
                  </div>
                  {deviceSession ? (
                    <>
                      <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-4">
                        <p className="mb-2 text-xs font-semibold text-muted-foreground">
                          {t("grok.oauthUserCodeLabel")}
                        </p>
                        <div className="flex flex-wrap items-center gap-2">
                          <code className="rounded-lg bg-background px-3 py-2 font-mono text-lg font-bold tracking-wider text-foreground">
                            {deviceSession.user_code}
                          </code>
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            onClick={() => void handleDeviceCopyCode()}
                          >
                            <Copy className="size-3.5" />
                            {t("common.copy")}
                          </Button>
                        </div>
                      </div>
                      <div className="rounded-xl border border-border px-4 py-3">
                        <p className="mb-2 text-xs font-semibold text-muted-foreground">
                          {t("grok.oauthOpenLink")}
                        </p>
                        <a
                          href={deviceSession.verification_url}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="inline-flex items-start gap-1.5 text-sm font-semibold text-primary hover:underline"
                        >
                          <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                          <span className="break-all">
                            {deviceSession.verification_url}
                          </span>
                        </a>
                      </div>
                      <div className="flex items-center gap-2 text-sm text-muted-foreground">
                        <Loader2
                          className={cn(
                            "size-4",
                            devicePolling || devicePollTimer.current
                              ? "animate-spin"
                              : "",
                          )}
                        />
                        {t("grok.oauthWaiting")}
                      </div>
                    </>
                  ) : null}
                </>
              )}
            </div>
          ) : addMethod === "sso" ? (
            <div className="space-y-4">
              <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                <p className="mb-1 font-semibold text-foreground">
                  {t("grok.ssoTitle")}
                </p>
                <p>{t("grok.ssoDesc")}</p>
              </div>
              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.ssoTokensLabel")} *
                </label>
                <textarea
                  className="min-h-[140px] w-full rounded-lg border border-input bg-transparent px-3 py-2 font-mono text-sm shadow-xs outline-none transition-[color,box-shadow] focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
                  placeholder={t("grok.ssoTokensPlaceholder")}
                  value={ssoTokens}
                  onChange={(e: ChangeEvent<HTMLTextAreaElement>) =>
                    setSsoTokens(e.target.value)
                  }
                />
                <p className="mt-1.5 text-xs text-muted-foreground">
                  {t("grok.ssoTokensHint")}
                </p>
              </div>

              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.baseUrl")}
                </label>
                <Input
                  placeholder={t("grok.baseUrlPlaceholder")}
                  value={form.base_url ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, base_url: e.target.value }))
                  }
                />
              </div>

              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.proxyUrl")}
                </label>
                <Input
                  placeholder="http://user:pass@host:port"
                  value={form.proxy_url ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, proxy_url: e.target.value }))
                  }
                />
              </div>

              {ssoResult ? (
                <div className="space-y-2 rounded-xl border border-border bg-muted/20 px-4 py-3">
                  <p className="text-sm font-semibold text-foreground">
                    {t("grok.ssoResultSummary", {
                      imported: ssoResult.imported,
                      total: ssoResult.total,
                    })}
                  </p>
                  <div className="max-h-40 space-y-1 overflow-y-auto">
                    {ssoResult.items.map((item, index) => (
                      <div
                        key={index}
                        className="flex items-start gap-1.5 text-xs"
                      >
                        {item.ok ? (
                          <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-emerald-500" />
                        ) : (
                          <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                        )}
                        <span className="min-w-0 flex-1 break-all text-muted-foreground">
                          {item.email || item.name || `#${index + 1}`}
                          {item.ok ? null : item.error ? ` — ${item.error}` : ""}
                        </span>
                      </div>
                    ))}
                  </div>
                </div>
              ) : null}
            </div>
          ) : (
            <>
              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.nameLabel")}
                </label>
                <Input
                  placeholder={t("grok.namePlaceholder")}
                  value={form.name ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, name: e.target.value }))
                  }
                />
              </div>

              {addMethod === "oauth" ? (
                <div>
                  <label className="mb-2 block text-sm font-medium text-muted-foreground">
                    {t("grok.authJson")} *
                  </label>
                  <textarea
                    className="min-h-[120px] w-full rounded-lg border border-input bg-transparent px-3 py-2 font-mono text-sm shadow-xs outline-none transition-[color,box-shadow] focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50"
                    placeholder={t("grok.authJsonPlaceholder")}
                    value={form.auth_json ?? ""}
                    onChange={(e: ChangeEvent<HTMLTextAreaElement>) =>
                      setForm((f) => ({ ...f, auth_json: e.target.value }))
                    }
                  />
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("grok.authJsonHint")}
                  </p>
                </div>
              ) : (
                <div>
                  <label className="mb-2 block text-sm font-medium text-muted-foreground">
                    {t("grok.apiKey")} *
                  </label>
                  <Input
                    type="password"
                    placeholder="xai-..."
                    value={form.api_key ?? ""}
                    onChange={(e: ChangeEvent<HTMLInputElement>) =>
                      setForm((f) => ({ ...f, api_key: e.target.value }))
                    }
                  />
                </div>
              )}

              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.baseUrl")}
                </label>
                <Input
                  placeholder={t("grok.baseUrlPlaceholder")}
                  value={form.base_url ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, base_url: e.target.value }))
                  }
                />
                <p className="mt-1.5 text-xs text-muted-foreground">
                  {t("grok.baseUrlHint")}
                </p>
              </div>

              <div>
                <div className="mb-2 flex items-center justify-between gap-2">
                  <label className="text-sm font-medium text-muted-foreground">
                    {t("grok.models")}
                  </label>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => void handleFetchModels()}
                    disabled={modelsLoading || !credentialReady}
                  >
                    <RefreshCw
                      className={cn("size-3", modelsLoading && "animate-spin")}
                    />
                    {modelsLoading
                      ? t("grok.modelsFetching")
                      : t("grok.modelsFetch")}
                  </Button>
                </div>
                <div className="mb-2 flex gap-2">
                  <Input
                    placeholder={t("grok.modelsPlaceholder")}
                    value={modelDraft}
                    onChange={(e: ChangeEvent<HTMLInputElement>) =>
                      setModelDraft(e.target.value)
                    }
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        e.preventDefault();
                        addModels(modelDraft);
                      }
                    }}
                  />
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => addModels(modelDraft)}
                    disabled={!modelDraft.trim()}
                  >
                    <Plus className="size-3.5" />
                  </Button>
                </div>
                {(form.models ?? []).length === 0 ? (
                  <p className="text-xs text-muted-foreground">
                    {t("grok.modelsEmpty")}
                  </p>
                ) : (
                  <div className="flex flex-wrap gap-1.5">
                    {(form.models ?? []).map((model) => (
                      <span
                        key={model}
                        className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-0.5 text-xs font-medium"
                      >
                        {model}
                        <button
                          type="button"
                          onClick={() => removeModel(model)}
                          className="text-muted-foreground hover:text-foreground"
                        >
                          <X className="size-3" />
                        </button>
                      </span>
                    ))}
                  </div>
                )}
              </div>

              <div>
                <label className="mb-2 block text-sm font-medium text-muted-foreground">
                  {t("grok.proxyUrl")}
                </label>
                <Input
                  placeholder="http://user:pass@host:port"
                  value={form.proxy_url ?? ""}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setForm((f) => ({ ...f, proxy_url: e.target.value }))
                  }
                />
              </div>
            </>
          )}
        </div>
      </Modal>

      {testingAccount ? (
        <GrokTestConnectionModal
          account={testingAccount}
          onClose={() => setTestingAccount(null)}
          onSettled={() => void reload()}
        />
      ) : null}

      {/* 导入来源选择弹窗（点「导入文件」先弹提示，风格对齐 Codex 导入） */}
      <Modal
        show={showImportPicker}
        title={t("grok.importPickerTitle")}
        onClose={() => setShowImportPicker(false)}
        contentClassName="sm:max-w-[560px]"
      >
        <p className="mb-4 text-sm text-muted-foreground">
          {t("grok.importPickerDesc")}
        </p>
        <div className="grid gap-3 sm:grid-cols-2">
          <button
            type="button"
            className="flex items-start gap-3 rounded-xl border border-border px-4 py-3 text-left transition-colors hover:bg-muted/50"
            onClick={() => authFileInputRef.current?.click()}
          >
            <FileJson className="size-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">
                {t("grok.importOptJson")}
              </div>
              <div className="text-[11px] text-muted-foreground">
                {t("grok.importOptJsonDesc")}
              </div>
            </div>
          </button>
          <button
            type="button"
            className="flex items-start gap-3 rounded-xl border border-border px-4 py-3 text-left transition-colors hover:bg-muted/50"
            onClick={() => ssoFileInputRef.current?.click()}
          >
            <FileText className="size-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">{t("grok.importOptSso")}</div>
              <div className="text-[11px] text-muted-foreground">
                {t("grok.importOptSsoDesc")}
              </div>
            </div>
          </button>
          <button
            type="button"
            className="flex items-start gap-3 rounded-xl border border-border px-4 py-3 text-left transition-colors hover:bg-muted/50"
            onClick={() => refreshFileInputRef.current?.click()}
          >
            <KeyRound className="size-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <div className="text-sm font-medium">
                {t("grok.importOptRefresh")}
              </div>
              <div className="text-[11px] text-muted-foreground">
                {t("grok.importOptRefreshDesc")}
              </div>
            </div>
          </button>
        </div>
      </Modal>

      {/* 隐藏文件输入：三种来源 */}
      <input
        ref={authFileInputRef}
        type="file"
        accept=".json,application/json"
        multiple
        className="hidden"
        onChange={(e) => void handleImportAuthFiles(e.target.files)}
      />
      <input
        ref={ssoFileInputRef}
        type="file"
        accept=".txt,text/plain"
        className="hidden"
        onChange={(e) => void handleImportSsoFile(e.target.files)}
      />
      <input
        ref={refreshFileInputRef}
        type="file"
        accept=".txt,text/plain"
        className="hidden"
        onChange={(e) => void handleImportRefreshFile(e.target.files)}
      />

      <Modal
        show={Boolean(importResult)}
        title={t("grok.fileImportTitle")}
        onClose={() => setImportResult(null)}
        contentClassName="sm:max-w-[520px]"
        footer={
          <Button variant="outline" onClick={() => setImportResult(null)}>
            {t("common.close")}
          </Button>
        }
      >
        {importResult ? (
          <div className="space-y-3">
            <p className="text-sm font-semibold text-foreground">
              {t("grok.ssoResultSummary", {
                imported: importResult.imported,
                total: importResult.total,
              })}
            </p>
            <div className="max-h-72 space-y-1 overflow-y-auto rounded-lg border border-border bg-muted/20 px-3 py-2">
              {importResult.items.map((item, index) => (
                <div key={index} className="flex items-start gap-1.5 text-xs">
                  {item.ok ? (
                    <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-emerald-500" />
                  ) : (
                    <XCircle className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                  )}
                  <span className="min-w-0 flex-1 break-all text-muted-foreground">
                    {item.email || item.name || `#${index + 1}`}
                    {item.ok ? null : item.error ? ` — ${item.error}` : ""}
                  </span>
                </div>
              ))}
            </div>
          </div>
        ) : null}
      </Modal>

      <Modal
        show={editAccount !== null}
        title={t("grok.editTitle")}
        contentClassName="sm:max-w-[560px]"
        onClose={() => setEditAccount(null)}
        footer={
          <>
            <Button variant="outline" onClick={() => setEditAccount(null)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => void handleSaveEdit()}
              disabled={editSubmitting}
            >
              {editSubmitting ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : null}
              {t("common.save")}
            </Button>
          </>
        }
      >
        {editAccount ? (
          <div className="space-y-4">
            <div className="rounded-lg bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
              {accountLabel(editAccount)}
            </div>

            <div>
              <div className="mb-2 flex items-center justify-between gap-2">
                <label className="text-sm font-medium text-muted-foreground">
                  {t("grok.models")}
                </label>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={editFillCommonModels}
                >
                  <Plus className="size-3" />
                  {t("grok.modelsQuickAdd")}
                </Button>
              </div>
              <div className="mb-2 flex gap-2">
                <Input
                  placeholder={t("grok.modelsPlaceholder")}
                  value={editModelDraft}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setEditModelDraft(e.target.value)
                  }
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      editAddModels(editModelDraft);
                    }
                  }}
                />
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => editAddModels(editModelDraft)}
                  disabled={!editModelDraft.trim()}
                >
                  <Plus className="size-3.5" />
                </Button>
              </div>
              {editForm.models.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  {t("grok.editModelsEmptyHint")}
                </p>
              ) : (
                <div className="flex flex-wrap gap-1.5">
                  {editForm.models.map((model) => (
                    <span
                      key={model}
                      className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-0.5 text-xs font-medium"
                    >
                      {model}
                      <button
                        type="button"
                        onClick={() => editRemoveModel(model)}
                        className="text-muted-foreground hover:text-foreground"
                      >
                        <X className="size-3" />
                      </button>
                    </span>
                  ))}
                </div>
              )}
            </div>

            <div>
              <label className="mb-2 block text-sm font-medium text-muted-foreground">
                {t("grok.baseUrl")}
              </label>
              <Input
                placeholder={t("grok.baseUrlPlaceholder")}
                value={editForm.base_url}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  setEditForm((f) => ({ ...f, base_url: e.target.value }))
                }
              />
            </div>

            <div>
              <label className="mb-2 block text-sm font-medium text-muted-foreground">
                {t("grok.proxyUrl")}
              </label>
              <Input
                placeholder="http://user:pass@host:port"
                value={editForm.proxy_url}
                onChange={(e: ChangeEvent<HTMLInputElement>) =>
                  setEditForm((f) => ({ ...f, proxy_url: e.target.value }))
                }
              />
            </div>
          </div>
        ) : null}
      </Modal>
    </div>
  );
}

function GrokAccountCard({
  account,
  sequence,
  busy,
  batchTesting,
  selected,
  onToggleSelect,
  onTest,
  onRefresh,
  onToggleEnabled,
  onEdit,
  onDelete,
  onUsageRefreshed,
}: {
  account: AccountRow;
  sequence: number;
  busy: boolean;
  batchTesting: boolean;
  selected: boolean;
  onToggleSelect: () => void;
  onTest: () => void;
  onRefresh: () => void;
  onToggleEnabled: () => void;
  onEdit: () => void;
  onDelete: () => void;
  onUsageRefreshed: () => void;
}) {
  const { t } = useTranslation();
  const disabled = account.enabled === false;
  const isOAuth = account.grok_auth_kind === "oauth";
  const models = account.models ?? [];
  const host = shortHost(account.base_url);
  const label = accountLabel(account);

  return (
    <article
      className={cn(
        "group relative flex min-w-0 flex-col overflow-hidden rounded-xl border bg-card shadow-sm transition-[border-color,box-shadow,background-color] duration-200",
        selected
          ? "border-primary/60 ring-1 ring-primary/30"
          : disabled
            ? "border-border/70 opacity-80"
            : "border-border hover:border-border hover:shadow-md",
      )}
    >
      <div className="flex flex-1 flex-col gap-3.5 p-4 sm:p-5">
        {/* Header: identity + status + actions */}
        <div className="flex min-w-0 items-start gap-3">
          <input
            type="checkbox"
            className="mt-1 size-4 shrink-0 cursor-pointer rounded border-border accent-primary"
            aria-label={t("accounts.selectAll")}
            checked={selected}
            onChange={onToggleSelect}
          />
          <ModelLogo
            model="grok"
            size={44}
            variant="ring"
            title="Grok"
            className={cn(
              "shrink-0 shadow-sm",
              disabled && "opacity-60 grayscale",
            )}
          />

          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="rounded-md bg-muted/80 px-1.5 py-0.5 font-mono text-[11px] font-semibold text-muted-foreground">
                #{sequence}
              </span>
              <StatusBadge
                status={disabled ? "paused" : (account.status ?? "unknown")}
                errorMessage={account.error_message}
              />
            </div>
            <h3
              className="mt-1.5 break-all text-[15px] font-semibold leading-snug tracking-tight text-foreground sm:text-base"
              title={label}
            >
              {label}
            </h3>
            {host ? (
              <p
                className="mt-1 max-w-full truncate font-mono text-[11px] leading-tight text-muted-foreground/75"
                title={account.base_url ?? undefined}
              >
                {host}
              </p>
            ) : null}
          </div>

          <div className="flex shrink-0 items-center gap-0.5 rounded-lg border border-border/80 bg-muted/30 p-0.5">
            <GrokAccountActions
              account={account}
              busy={busy}
              batchTesting={batchTesting}
              onTest={onTest}
              onRefresh={onRefresh}
              onToggleEnabled={onToggleEnabled}
              onEdit={onEdit}
              onDelete={onDelete}
            />
          </div>
        </div>

        {/* Meta badges */}
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="inline-flex items-center gap-0.5 rounded-md bg-zinc-900 px-1.5 py-0.5 text-[10px] font-medium text-white ring-1 ring-inset ring-zinc-700 dark:bg-white dark:text-zinc-900 dark:ring-zinc-300">
            <Sparkles className="size-2.5" />
            Grok
          </span>
          <span
            className={cn(
              "inline-flex items-center gap-0.5 rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
              isOAuth
                ? "bg-violet-50 text-violet-700 ring-violet-600/20 dark:bg-violet-950 dark:text-violet-300 dark:ring-violet-400/20"
                : "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20",
            )}
          >
            {isOAuth ? (
              <FileJson className="size-2.5" />
            ) : (
              <KeyRound className="size-2.5" />
            )}
            {isOAuth
              ? t("grok.authKindOAuthShort")
              : t("grok.authKindApiKey")}
          </span>
          <GrokPlanBadge plan={account.plan_type} compact />
          {disabled ? (
            <span className="inline-flex items-center rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-300">
              <PowerOff className="mr-0.5 size-2.5" />
              {t("accounts.disabled")}
            </span>
          ) : null}
        </div>

        {/* Usage panel */}
        <div className="rounded-lg border border-border/70 bg-muted/25 px-3 py-2.5">
          <GrokUsageCell
            account={account}
            compact
            detailed
            onRefreshed={onUsageRefreshed}
          />
        </div>

        {/* Footer: models + updated */}
        <div className="mt-auto flex min-w-0 flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-3">
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1">
            <span className="shrink-0 text-[11px] font-medium text-muted-foreground">
              {t("grok.colModels")}
            </span>
            {models.length === 0 ? (
              <span className="text-[11px] text-muted-foreground/70">
                {t("grok.noModels")}
              </span>
            ) : (
              <>
                {models.slice(0, 4).map((model) => (
                  <span
                    key={model}
                    className="max-w-[9rem] truncate rounded-md bg-background px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground ring-1 ring-inset ring-border"
                    title={model}
                  >
                    {model}
                  </span>
                ))}
                {models.length > 4 ? (
                  <span className="text-[10px] font-medium text-muted-foreground">
                    +{models.length - 4}
                  </span>
                ) : null}
              </>
            )}
          </div>
          <span
            className="shrink-0 text-[11px] text-muted-foreground"
            title={
              account.updated_at
                ? formatBeijingTime(account.updated_at) || undefined
                : undefined
            }
          >
            {account.updated_at
              ? t("grok.updatedAgo", {
                  time: formatRelativeTime(account.updated_at),
                })
              : "—"}
          </span>
        </div>
      </div>
    </article>
  );
}

// 卡片右上角与表格行共用同一组操作按钮，避免两处漂移。
function GrokAccountActions({
  account,
  busy,
  batchTesting,
  onTest,
  onRefresh,
  onToggleEnabled,
  onEdit,
  onDelete,
}: {
  account: AccountRow;
  busy: boolean;
  batchTesting: boolean;
  onTest: () => void;
  onRefresh: () => void;
  onToggleEnabled: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const disabled = account.enabled === false;
  const isOAuth = account.grok_auth_kind === "oauth";

  return (
    <>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={t("accounts.testConnection")}
        disabled={busy || batchTesting}
        onClick={onTest}
      >
        <Zap className="size-3.5" />
      </Button>
      {isOAuth ? (
        <Button
          variant="ghost"
          size="icon-sm"
          className="size-8"
          title={t("grok.actionRefresh")}
          disabled={busy}
          onClick={onRefresh}
        >
          <RefreshCw className={cn("size-3.5", busy && "animate-spin")} />
        </Button>
      ) : null}
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={disabled ? t("grok.actionEnable") : t("grok.actionDisable")}
        disabled={busy}
        onClick={onToggleEnabled}
      >
        {disabled ? (
          <Power className="size-3.5" />
        ) : (
          <PowerOff className="size-3.5" />
        )}
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        title={t("grok.actionEdit")}
        disabled={busy}
        onClick={onEdit}
      >
        <Pencil className="size-3.5" />
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8 text-destructive hover:bg-destructive/10 hover:text-destructive"
        title={t("grok.actionDelete")}
        disabled={busy}
        onClick={onDelete}
      >
        <Trash2 className="size-3.5" />
      </Button>
    </>
  );
}

// 表格行：与 Codex 账号表格同风格的列表布局（仅桌面端渲染）。
function GrokAccountTableRow({
  account,
  sequence,
  busy,
  batchTesting,
  selected,
  onToggleSelect,
  healthBuckets,
  onTest,
  onRefresh,
  onToggleEnabled,
  onEdit,
  onDelete,
  onUsageRefreshed,
}: {
  account: AccountRow;
  sequence: number;
  busy: boolean;
  batchTesting: boolean;
  selected: boolean;
  onToggleSelect: () => void;
  healthBuckets?: AccountHealthBucket[];
  onTest: () => void;
  onRefresh: () => void;
  onToggleEnabled: () => void;
  onEdit: () => void;
  onDelete: () => void;
  onUsageRefreshed: () => void;
}) {
  const { t } = useTranslation();
  const disabled = account.enabled === false;
  const isOAuth = account.grok_auth_kind === "oauth";
  const models = account.models ?? [];
  const host = shortHost(account.base_url);
  const label = accountLabel(account);

  return (
    <TableRow
      className={cn(disabled && "opacity-70", selected && "bg-primary/5")}
    >
      <TableCell className="w-9">
        <input
          type="checkbox"
          className="size-4 cursor-pointer rounded border-border accent-primary"
          aria-label={t("accounts.selectAll")}
          checked={selected}
          onChange={onToggleSelect}
        />
      </TableCell>
      <TableCell className="font-mono text-[12px] text-muted-foreground">
        #{sequence}
      </TableCell>
      <TableCell>
        <div className="flex min-w-0 items-center gap-2.5">
          <ModelLogo
            model="grok"
            size={32}
            variant="ring"
            title="Grok"
            className={cn("shrink-0", disabled && "opacity-60 grayscale")}
          />
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-1.5">
              <span
                className="max-w-[200px] truncate text-[13px] font-semibold text-foreground"
                title={label}
              >
                {label}
              </span>
              <span
                className={cn(
                  "inline-flex shrink-0 items-center gap-0.5 whitespace-nowrap rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1 ring-inset",
                  isOAuth
                    ? "bg-violet-50 text-violet-700 ring-violet-600/20 dark:bg-violet-950 dark:text-violet-300 dark:ring-violet-400/20"
                    : "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-950 dark:text-sky-300 dark:ring-sky-400/20",
                )}
                title={t("grok.authKind")}
              >
                {isOAuth ? (
                  <FileJson className="size-2.5" />
                ) : (
                  <KeyRound className="size-2.5" />
                )}
                {isOAuth
                  ? t("grok.authKindOAuthShort")
                  : t("grok.authKindApiKey")}
              </span>
            </div>
            {host ? (
              <div
                className="max-w-[200px] truncate font-mono text-[11px] text-muted-foreground/75"
                title={account.base_url ?? undefined}
              >
                {host}
              </div>
            ) : null}
          </div>
        </div>
      </TableCell>
      <TableCell className="text-center">
        <GrokPlanBadge plan={account.plan_type} />
      </TableCell>
      <TableCell>
        <div className="space-y-1.5">
          <StatusBadge
            status={disabled ? "paused" : (account.status ?? "unknown")}
            errorMessage={account.error_message}
          />
          <AccountHealthBar buckets={healthBuckets} />
        </div>
      </TableCell>
      <TableCell>
        <div className="space-y-0.5 text-[13px]">
          <div className="flex items-center gap-1.5 whitespace-nowrap">
            <span className="font-medium text-emerald-600">
              {account.success_requests ?? 0}
            </span>
            <span className="text-muted-foreground">/</span>
            <span className="font-medium text-red-500">
              {account.error_requests ?? 0}
            </span>
          </div>
          {((account.retry_error_requests ?? 0) > 0 ||
            (account.rate_limit_attempts ?? 0) > 0) && (
            <div className="whitespace-nowrap text-[11px] text-muted-foreground">
              retry {account.retry_error_requests ?? 0} · 429{" "}
              {account.rate_limit_attempts ?? 0}
            </div>
          )}
        </div>
      </TableCell>
      <TableCell className="min-w-[170px]">
        <GrokUsageCell account={account} onRefreshed={onUsageRefreshed} />
      </TableCell>
      <TableCell>
        {models.length === 0 ? (
          <span className="text-[12px] text-muted-foreground/70">
            {t("grok.noModels")}
          </span>
        ) : (
          <div className="flex max-w-[150px] flex-wrap items-center gap-1">
            {models.slice(0, 2).map((model) => (
              <span
                key={model}
                className="max-w-[9rem] truncate rounded-md bg-muted/50 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground ring-1 ring-inset ring-border"
                title={model}
              >
                {model}
              </span>
            ))}
            {models.length > 2 ? (
              <span
                className="text-[10px] font-medium text-muted-foreground"
                title={models.join(", ")}
              >
                +{models.length - 2}
              </span>
            ) : null}
          </div>
        )}
      </TableCell>
      <TableCell>
        <span
          className="whitespace-nowrap text-[12px] text-muted-foreground"
          title={
            account.updated_at
              ? formatBeijingTime(account.updated_at) || undefined
              : undefined
          }
        >
          {account.updated_at
            ? formatRelativeTime(account.updated_at)
            : "—"}
        </span>
      </TableCell>
      <TableCell className="text-right">
        <div className="inline-flex items-center gap-0.5">
          <GrokAccountActions
            account={account}
            busy={busy}
            batchTesting={batchTesting}
            onTest={onTest}
            onRefresh={onRefresh}
            onToggleEnabled={onToggleEnabled}
            onEdit={onEdit}
            onDelete={onDelete}
          />
        </div>
      </TableCell>
    </TableRow>
  );
}

function grokFormatDollars(cents?: number | null): string {
  if (cents === null || cents === undefined || !Number.isFinite(cents))
    return "--";
  return `$${(cents / 100).toFixed(2)}`;
}

function GrokUsageCell({
  account,
  onRefreshed,
  compact = false,
  detailed = false,
}: {
  account: AccountRow;
  onRefreshed?: () => void;
  compact?: boolean;
  // detailed 展示 billing 完整视图（产品用量、按量付费、月度金额），卡片视图启用。
  detailed?: boolean;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [refreshing, setRefreshing] = useState(false);

  const handleRefreshUsage = async () => {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await api.refreshAccountUsage(account.id);
      onRefreshed?.();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setRefreshing(false);
    }
  };

  const billing = account.grok_billing;
  const weeklyPct = billing?.weekly_percent ?? account.usage_percent_5h;
  const monthlyPct = billing?.monthly_percent ?? account.usage_percent_7d;
  const weeklyResetAt = billing?.weekly_period_end ?? account.reset_5h_at;
  const monthlyResetAt = billing?.monthly_period_end ?? account.reset_7d_at;
  const products = detailed ? (billing?.product_usage ?? []) : [];
  const paygCap = detailed ? (billing?.on_demand_cap_cents ?? null) : null;
  const paygUsed = billing?.on_demand_used_cents ?? null;
  const paygEnabled = paygCap !== null && paygCap > 0;
  const monthlyAmount =
    billing?.monthly_used_cents !== null &&
    billing?.monthly_used_cents !== undefined &&
    billing?.monthly_limit_cents !== null &&
    billing?.monthly_limit_cents !== undefined
      ? `${grokFormatDollars(Math.min(billing.monthly_used_cents, billing.monthly_limit_cents))} / ${grokFormatDollars(billing.monthly_limit_cents)}`
      : undefined;
  const weeklyPeriodTitle =
    billing?.weekly_period_start && billing?.weekly_period_end
      ? `${formatBeijingTime(billing.weekly_period_start, "")} ~ ${formatBeijingTime(billing.weekly_period_end, "")}`
      : undefined;

  const hasWeekly = weeklyPct !== null && weeklyPct !== undefined;
  const hasMonthly = monthlyPct !== null && monthlyPct !== undefined;

  // free 档 token 用量条(滚动 24h 窗口):429 错误体解析的权威值优先(耗尽期间),
  // 否则用逐请求 x-ratelimit 头快照;两者都有时取观测时间较新者。
  const isFreePlan = (account.plan_type ?? "").trim().toLowerCase() === "free";
  const rl = account.grok_rate_limit;
  const fq = account.grok_free_quota;
  const fqFresh =
    fq && Date.now() - new Date(fq.exhausted_at).getTime() < 24 * 3600 * 1000;
  const rlUsable = rl && (rl.limit_tokens ?? 0) > 0;
  // 账号是否处于"限流"状态(涵盖 rate_limited/usage_limited 等所有渲染成"限流"的状态)。
  const usageLimited =
    GROK_LIMITED_STATUSES.has(account.status ?? "") ||
    GROK_LIMITED_STATUSES.has(account.cooldown_reason ?? "");
  let freeQuotaBar: {
    pct: number;
    used: number;
    limit: number;
    observedAt?: string;
    exhausted: boolean;
  } | null = null;
  if (isFreePlan || fqFresh) {
    const preferFq =
      fqFresh &&
      (!rlUsable ||
        !rl?.updated_at ||
        new Date(fq!.exhausted_at).getTime() >=
          new Date(rl.updated_at).getTime());
    if (preferFq) {
      freeQuotaBar = {
        pct: Math.min(100, (fq!.used_tokens / fq!.limit_tokens) * 100),
        used: fq!.used_tokens,
        limit: fq!.limit_tokens,
        observedAt: fq!.exhausted_at,
        exhausted: true,
      };
    } else if (rlUsable && !(isFreePlan && usageLimited)) {
      // 限流的 free 账号不采用 x-ratelimit 快照:它多半是限流前的过时观测(remaining≈满),
      // 会算出误导性的低用量(0.0%)与"限流"徽章矛盾;此时落到下面的"耗尽"兜底灰条。
      const used = Math.max(0, rl!.limit_tokens! - (rl!.remaining_tokens ?? 0));
      freeQuotaBar = {
        pct: Math.min(100, (used / rl!.limit_tokens!) * 100),
        used,
        limit: rl!.limit_tokens!,
        observedAt: rl!.updated_at,
        exhausted: false,
      };
    }
  }

  // 兜底:free 账号显示"限流"却拿不到任何可信用量数字时——超支 402、普通 429、批量测试旧路径
  // 误标 rate_limited、手动置限流、或只有过时的乐观 rl 快照——画一条满格灰条表意"已耗尽",
  // 避免与"限流"徽章观感割裂(退化成 "usage —" 或误显示 0.0%)。不依赖账号重测纠正状态。
  const freeQuotaExhaustedNoDetail =
    !freeQuotaBar && isFreePlan && usageLimited;

  const refreshButton = (
    <button
      type="button"
      onClick={() => void handleRefreshUsage()}
      disabled={refreshing}
      title={t("accounts.refreshUsage")}
      aria-label={t("accounts.refreshUsage")}
      className="shrink-0 rounded-md p-1 text-muted-foreground transition-colors hover:bg-background hover:text-foreground disabled:opacity-50"
    >
      <RefreshCw className={cn("size-3.5", refreshing && "animate-spin")} />
    </button>
  );

  if (
    !hasWeekly &&
    !hasMonthly &&
    products.length === 0 &&
    !freeQuotaBar &&
    !freeQuotaExhaustedNoDetail
  ) {
    return (
      <div className="flex items-center justify-between gap-2">
        <span className="text-[12px] text-muted-foreground">
          {t("accounts.usage")} —
        </span>
        {refreshButton}
      </div>
    );
  }

  // 表格视图用单行内联条压缩行高，明细与重置时间进 tooltip；卡片视图完整展示。
  const inline = !detailed;

  const bars: ReactNode[] = [];
  if (freeQuotaExhaustedNoDetail) {
    // 满格灰条(muted):表意"限流/已耗尽"但无上游用量明细,右侧显示"耗尽"而非百分比。
    bars.push(
      <GrokUsageBar
        key="free-quota-exhausted"
        label={t("grok.freeQuota")}
        shortLabel={t("grok.freeQuotaShort")}
        pct={100}
        tone="muted"
        valueLabel={t("grok.freeQuotaExhaustedShort")}
        titleText={[t("grok.freeQuotaWindow"), t("grok.freeQuotaNoDetail")].join(
          " · ",
        )}
        inline={inline}
      />,
    );
  } else if (freeQuotaBar) {
    bars.push(
      <GrokUsageBar
        key="free-quota"
        label={t("grok.freeQuota")}
        shortLabel={t("grok.freeQuotaShort")}
        pct={freeQuotaBar.pct}
        amountText={
          detailed
            ? `${grokFormatCompactNumber(freeQuotaBar.used)} / ${grokFormatCompactNumber(freeQuotaBar.limit)} ${t("accounts.usageTokUnit")}`
            : undefined
        }
        titleText={[
          t("grok.freeQuotaWindow"),
          `${grokFormatCompactNumber(freeQuotaBar.used)} / ${grokFormatCompactNumber(freeQuotaBar.limit)} ${t("accounts.usageTokUnit")}`,
          freeQuotaBar.exhausted ? t("grok.freeQuotaExhausted") : null,
          freeQuotaBar.observedAt
            ? `${t("grok.rateLimitUpdated")} ${formatBeijingTime(freeQuotaBar.observedAt, "")}`
            : null,
        ]
          .filter(Boolean)
          .join(" · ")}
        inline={inline}
      />,
    );
  }
  if (hasWeekly) {
    bars.push(
      <GrokUsageBar
        key="weekly"
        label={t("grok.quotaWeekly")}
        shortLabel={t("grok.quotaWeeklyShort")}
        pct={weeklyPct!}
        resetAt={weeklyResetAt}
        detail={account.usage_5h_detail}
        titleText={weeklyPeriodTitle}
        inline={inline}
      />,
    );
  }
  for (const [index, item] of products.entries()) {
    bars.push(
      <GrokUsageBar
        key={`product-${index}-${item.product}`}
        label={t("grok.productUsage", { product: item.product })}
        shortLabel={t("grok.productUsage", { product: item.product })}
        pct={item.usage_percent ?? null}
      />,
    );
  }
  if (detailed && paygEnabled) {
    bars.push(
      <GrokUsageBar
        key="payg"
        label={t("grok.payAsYouGo")}
        shortLabel={t("grok.payAsYouGo")}
        pct={
          paygUsed !== null && paygCap! > 0
            ? Math.min(100, Math.max(0, (paygUsed / paygCap!) * 100))
            : null
        }
        amountText={`${grokFormatDollars(paygUsed ?? 0)} / ${grokFormatDollars(paygCap)}`}
      />,
    );
  }
  if (hasMonthly) {
    bars.push(
      <GrokUsageBar
        key="monthly"
        label={t("grok.quotaMonthly")}
        shortLabel={t("grok.quotaMonthlyShort")}
        pct={monthlyPct!}
        resetAt={monthlyResetAt}
        detail={account.usage_7d_detail}
        amountText={detailed ? monthlyAmount : undefined}
        titleText={detailed ? undefined : monthlyAmount}
        inline={inline}
      />,
    );
  }

  return (
    <div className="flex items-center gap-2">
      <div
        className={cn(
          "min-w-0 flex-1",
          compact && bars.length >= 2
            ? "grid grid-cols-1 gap-2.5 sm:grid-cols-2 sm:gap-3"
            : "space-y-2",
        )}
      >
        {bars}
        {detailed && !paygEnabled && billing ? (
          <div className="flex items-center gap-1.5 self-end text-[11px] text-muted-foreground">
            <span className="font-semibold">{t("grok.payAsYouGo")}</span>
            <span>{t("grok.paygDisabled")}</span>
          </div>
        ) : null}
        {detailed && account.grok_rate_limit ? (
          <div
            className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 self-end text-[11px] text-muted-foreground sm:col-span-2"
            title={
              account.grok_rate_limit.updated_at
                ? `${t("grok.rateLimitUpdated")} ${formatBeijingTime(account.grok_rate_limit.updated_at, "")}`
                : undefined
            }
          >
            <span className="font-semibold">{t("grok.rateLimitLabel")}</span>
            <span className="tabular-nums">
              {grokFormatCompactNumber(account.grok_rate_limit.remaining_tokens)}
              /
              {grokFormatCompactNumber(account.grok_rate_limit.limit_tokens)}{" "}
              {t("accounts.usageTokUnit")}
            </span>
            <span className="text-muted-foreground/60">·</span>
            <span className="tabular-nums">
              {grokFormatCompactNumber(account.grok_rate_limit.remaining_requests)}
              /
              {grokFormatCompactNumber(account.grok_rate_limit.limit_requests)}{" "}
              {t("accounts.usageReqUnit")}
            </span>
          </div>
        ) : null}
      </div>
      {refreshButton}
    </div>
  );
}

function grokUsageBarColor(pct: number): string {
  if (pct >= 90) return "bg-red-500";
  if (pct >= 70) return "bg-amber-500";
  return "bg-emerald-500";
}

function grokUsageTrackColor(pct: number): string {
  if (pct >= 90) return "bg-red-500/15";
  if (pct >= 70) return "bg-amber-500/15";
  return "bg-emerald-500/15";
}

function grokUsageTextColor(pct: number): string {
  if (pct >= 90) return "text-red-600 dark:text-red-400";
  if (pct >= 70) return "text-amber-700 dark:text-amber-400";
  return "text-emerald-700 dark:text-emerald-400";
}

function grokFormatResetAt(
  resetAt?: string | null,
): { label: string; title: string } | null {
  if (!resetAt) return null;
  const d = new Date(resetAt);
  if (Number.isNaN(d.getTime()) || d.getTime() <= Date.now()) return null;
  const full = formatBeijingTime(resetAt, "");
  if (!full) return null;
  return { label: full.slice(5), title: full };
}

function grokFormatCompactNumber(value?: number): string {
  const n = Number(value || 0);
  if (n >= 1_000_000)
    return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`;
  return String(n);
}

function GrokUsageBar({
  label,
  shortLabel,
  pct,
  resetAt,
  detail,
  amountText,
  titleText,
  inline = false,
  tone,
  valueLabel,
}: {
  label: string;
  shortLabel: string;
  // pct 为 null 时表示上游未给出该项用量（渲染 "--" 与空进度条）。
  pct: number | null;
  resetAt?: string | null;
  detail?: AccountRow["usage_5h_detail"];
  amountText?: string;
  titleText?: string;
  // inline 渲染单行紧凑条（表格视图），明细/重置时间收进 tooltip。
  inline?: boolean;
  // tone="muted" 用中性灰渲染整条（无权威用量、仅表意"耗尽"时用）。
  tone?: "muted";
  // valueLabel 覆盖右侧的百分比文本（如"耗尽"），不传则显示 pct%。
  valueLabel?: string;
}) {
  const { t } = useTranslation();
  const resetTime = grokFormatResetAt(resetAt);
  const hasDetail = Boolean(
    detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0),
  );
  const detailText = hasDetail
    ? `${grokFormatCompactNumber(detail?.requests)} ${t("accounts.usageReqUnit")} / ${grokFormatCompactNumber(detail?.tokens)} ${t("accounts.usageTokUnit")}`
    : "";
  const clamped = pct === null ? 0 : Math.min(100, Math.max(0, pct));
  const muted = tone === "muted";
  const barColor = muted ? "bg-muted-foreground/40" : grokUsageBarColor(clamped);
  const trackColor = muted
    ? "bg-muted"
    : pct === null
      ? "bg-muted/60"
      : grokUsageTrackColor(clamped);
  const textColor =
    muted || pct === null ? "text-muted-foreground" : grokUsageTextColor(clamped);
  const valueText = valueLabel ?? (pct === null ? "--" : `${pct.toFixed(1)}%`);

  if (inline) {
    const tooltip = [label, titleText, detailText || null]
      .filter(Boolean)
      .join(" · ");
    return (
      <div className="min-w-0" title={tooltip || undefined}>
        <div className="flex min-w-0 items-center gap-1.5">
          <span className="w-7 shrink-0 text-[11px] font-semibold text-muted-foreground">
            {shortLabel}
          </span>
          <div
            className={cn(
              "h-2 min-w-0 flex-1 overflow-hidden rounded-full",
              trackColor,
            )}
          >
            <div
              className={cn(
                "h-full rounded-full transition-all duration-300",
                barColor,
              )}
              style={{ width: `${clamped}%` }}
            />
          </div>
          <span
            className={cn(
              "w-11 shrink-0 text-right text-[11px] font-semibold tabular-nums",
              textColor,
            )}
          >
            {valueText}
          </span>
        </div>
        {resetTime ? (
          <div
            className="mt-0.5 pl-[34px] text-[10px] font-medium text-muted-foreground/80"
            title={resetTime.title}
          >
            {/* 表格空间紧张，重置时间去掉秒（完整时间在 tooltip） */}
            ⏱ {t("grok.quotaReset")} {resetTime.label.slice(0, 11)}
          </div>
        ) : null}
      </div>
    );
  }

  return (
    <div className="min-w-0">
      <div className="mb-1 flex items-center justify-between gap-2">
        <span
          className="truncate text-[11px] font-semibold text-muted-foreground"
          title={titleText ?? label}
        >
          <span className="sm:hidden">{shortLabel}</span>
          <span className="hidden sm:inline">{label}</span>
        </span>
        <span className="flex min-w-0 shrink-0 items-baseline gap-1.5">
          {amountText ? (
            <span className="text-[10px] font-medium tabular-nums text-muted-foreground">
              {amountText}
            </span>
          ) : null}
          <span
            className={cn("text-[12px] font-semibold tabular-nums", textColor)}
          >
            {valueText}
          </span>
        </span>
      </div>
      <div className={cn("h-2 overflow-hidden rounded-full", trackColor)}>
        <div
          className={cn(
            "h-full rounded-full transition-all duration-300",
            barColor,
          )}
          style={{ width: `${clamped}%` }}
        />
      </div>
      {detailText ? (
        <div className="mt-1 text-[10px] font-medium text-muted-foreground">
          {detailText}
        </div>
      ) : null}
      {resetTime ? (
        <div
          className="mt-0.5 text-[10px] font-medium text-muted-foreground/80"
          title={resetTime.title}
        >
          ⏱ {t("grok.quotaReset")} {resetTime.label}
        </div>
      ) : null}
    </div>
  );
}

type TestEvent = {
  type: string;
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
};

function GrokTestConnectionModal({
  account,
  onClose,
  onSettled,
}: {
  account: AccountRow;
  onClose: () => void;
  onSettled: () => void;
}) {
  const { t } = useTranslation();
  const [output, setOutput] = useState<string[]>([]);
  const [status, setStatus] = useState<
    "connecting" | "streaming" | "success" | "error"
  >("connecting");
  const [errorMsg, setErrorMsg] = useState("");
  const [model, setModel] = useState("");
  const [selectedModel, setSelectedModel] = useState("");
  const [modelOptions, setModelOptions] = useState<string[]>([]);
  const [modelOptionsReady, setModelOptionsReady] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const outputEndRef = useRef<HTMLDivElement>(null);
  const settledRef = useRef(false);
  const onSettledRef = useRef(onSettled);
  onSettledRef.current = onSettled;

  const markSettled = useCallback(() => {
    if (settledRef.current) return;
    settledRef.current = true;
    onSettledRef.current();
  }, []);

  useEffect(() => {
    const accountModels = (account.models ?? []).filter(
      (m) => m.trim() && !m.toLowerCase().includes("image"),
    );
    const next =
      accountModels.length > 0
        ? accountModels
        : [...DEFAULT_GROK_TEST_MODELS];
    setModelOptions(next);
    setSelectedModel(next[0] ?? "");
    setModelOptionsReady(true);
  }, [account.models]);

  useEffect(() => {
    if (!modelOptionsReady || !selectedModel) return;

    setOutput([]);
    setStatus("connecting");
    setErrorMsg("");
    setModel(selectedModel);
    settledRef.current = false;

    const controller = new AbortController();
    abortRef.current = controller;

    const run = async () => {
      if (controller.signal.aborted) return;
      try {
        const params = new URLSearchParams({ model: selectedModel });
        const res = await fetch(
          `/api/admin/accounts/${account.id}/test?${params.toString()}`,
          {
            signal: controller.signal,
            headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
          },
        );
        if (!res.ok) {
          const body = await res.text();
          let msg = `HTTP ${res.status}`;
          try {
            const parsed = JSON.parse(body);
            if (parsed.error) msg = parsed.error;
          } catch {
            /* ignore */
          }
          setStatus("error");
          setErrorMsg(msg);
          markSettled();
          return;
        }

        const reader = res.body?.getReader();
        if (!reader) {
          setStatus("error");
          setErrorMsg(t("accounts.browserStreamingUnsupported"));
          markSettled();
          return;
        }

        const decoder = new TextDecoder();
        let buffer = "";
        let receivedTerminal = false;

        const processLines = (lines: string[]) => {
          for (const line of lines) {
            const trimmed = line.trim();
            if (!trimmed.startsWith("data: ")) continue;
            try {
              const event: TestEvent = JSON.parse(trimmed.slice(6));
              switch (event.type) {
                case "test_start":
                  setModel(event.model || selectedModel);
                  setStatus("streaming");
                  break;
                case "content":
                  if (event.text) setOutput((prev) => [...prev, event.text!]);
                  break;
                case "test_complete":
                  receivedTerminal = true;
                  setStatus(event.success ? "success" : "error");
                  markSettled();
                  break;
                case "error":
                  receivedTerminal = true;
                  setStatus("error");
                  setErrorMsg(event.error || t("accounts.unknownError"));
                  markSettled();
                  break;
              }
            } catch {
              /* ignore */
            }
          }
        };

        while (true) {
          const { done, value } = await reader.read();
          if (done) {
            buffer += decoder.decode();
            break;
          }
          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop() || "";
          processLines(lines);
        }
        if (buffer.trim()) processLines([buffer]);
        if (!receivedTerminal) {
          setStatus("error");
          setErrorMsg(t("accounts.connectionEndedUnexpectedly"));
          markSettled();
        }
      } catch (err: unknown) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setStatus("error");
        setErrorMsg(
          err instanceof Error ? err.message : t("accounts.connectionFailed"),
        );
        markSettled();
      }
    };

    const timer = window.setTimeout(() => void run(), 50);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [account.id, markSettled, modelOptionsReady, selectedModel, t]);

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const statusText = {
    connecting: t("accounts.connecting"),
    streaming: t("accounts.receivingResponse"),
    success: t("accounts.testSuccess"),
    error: t("accounts.testFailed"),
  }[status];
  const StatusIcon = {
    connecting: Loader2,
    streaming: Loader2,
    success: CheckCircle2,
    error: XCircle,
  }[status];
  const statusIconSpin = status === "connecting" || status === "streaming";
  const statusColor = {
    connecting: "text-muted-foreground",
    streaming: "text-blue-500",
    success: "text-emerald-500",
    error: "text-red-500",
  }[status];

  return (
    <Modal
      show
      title={t("accounts.testConnectionTitle", {
        account: accountLabel(account),
      })}
      onClose={() => {
        abortRef.current?.abort();
        onClose();
      }}
      footer={
        <Button
          variant="outline"
          onClick={() => {
            abortRef.current?.abort();
            onClose();
          }}
        >
          {t("common.close")}
        </Button>
      }
      contentClassName="sm:max-w-[680px]"
    >
      <div className="space-y-4">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <span
            className={cn(
              "flex items-center gap-1.5 text-sm font-semibold",
              statusColor,
            )}
          >
            <StatusIcon
              className={cn("size-4", statusIconSpin && "animate-spin")}
            />
            {statusText}
          </span>
          <Select
            className="w-52 max-w-full"
            compact
            value={selectedModel}
            onValueChange={setSelectedModel}
            options={modelOptions.map((item) => ({
              label: item,
              value: item,
            }))}
            placeholder={model || t("settings.testModel")}
            disabled={!modelOptionsReady || modelOptions.length === 0}
          />
        </div>

        {(output.length > 0 ||
          status === "connecting" ||
          status === "streaming") && (
          <div
            className="max-h-[240px] min-h-[80px] overflow-auto rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed break-all whitespace-pre-wrap"
            style={{ fontFamily: "var(--font-geist-mono)" }}
          >
            {output.length === 0 && status === "connecting" ? (
              <span className="animate-pulse text-muted-foreground">
                {t("accounts.sendingTestRequest")}
              </span>
            ) : (
              output.join("")
            )}
            <div ref={outputEndRef} />
          </div>
        )}

        {status === "error" && errorMsg ? (
          <div className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
            {errorMsg}
          </div>
        ) : null}
      </div>
    </Modal>
  );
}

function CompactStat({
  label,
  chipLabel,
  value,
  tone,
  active = false,
  onClick,
}: {
  label: string;
  chipLabel?: string;
  value: number;
  tone: "neutral" | "success" | "warning" | "danger";
  active?: boolean;
  onClick?: () => void;
}) {
  const toneStyle = {
    neutral: {
      chip: "bg-muted text-muted-foreground",
      dot: "bg-slate-500",
    },
    success: {
      chip: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
      dot: "bg-emerald-500",
    },
    warning: {
      chip: "bg-amber-500/10 text-amber-700 dark:text-amber-300",
      dot: "bg-amber-500",
    },
    danger: {
      chip: "bg-red-500/10 text-red-700 dark:text-red-300",
      dot: "bg-red-500",
    },
  }[tone];

  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "flex min-h-[72px] w-full items-center justify-between gap-2 rounded-xl border px-2.5 py-2 text-left shadow-sm transition-[border-color,box-shadow,background-color,transform] duration-200 sm:min-h-[84px] sm:gap-3 sm:px-3 sm:py-2.5",
        active
          ? "border-primary/40 bg-primary/5 shadow-sm ring-1 ring-primary/25"
          : "border-border bg-card/85 hover:border-border hover:bg-card hover:shadow-sm",
        onClick &&
          "cursor-pointer active:scale-[0.99] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
      )}
    >
      <div className="min-w-0">
        <div className="truncate text-[11px] font-medium text-muted-foreground sm:text-[12px]">
          {label}
        </div>
        <div className="mt-1.5 text-[22px] font-semibold leading-none tracking-tight text-foreground tabular-nums sm:text-[26px]">
          {value}
        </div>
      </div>
      <div
        className={cn(
          "inline-flex items-center gap-1.5 rounded-full px-1.5 py-0.5 text-[11px] font-medium sm:px-2 sm:py-1 sm:text-[12px]",
          toneStyle.chip,
        )}
      >
        <span className={cn("size-1.5 rounded-full", toneStyle.dot)} />
        <span className="max-w-[4.5rem] truncate sm:max-w-none">
          {chipLabel ?? label}
        </span>
      </div>
    </button>
  );
}
