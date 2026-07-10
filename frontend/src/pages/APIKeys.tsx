import type { ChangeEvent, FormEvent, ReactNode } from "react";
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import APIKeyTokenUsagePanel from "../components/APIKeyTokenUsagePanel";
import ChipInput from "../components/ChipInput";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StateShell from "../components/StateShell";
import StatCard from "../components/StatCard";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { useDataLoader } from "../hooks/useDataLoader";
import { useToast } from "../hooks/useToast";
import type {
  AccountGroup,
  APIKeyLimits,
  APIKeyRow,
  APIKeyWindowUsage,
  SystemSettings,
} from "../types";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select, type SelectOption } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";
import {
  Check,
  Copy,
  CalendarClock,
  CircleDollarSign,
  ChevronDown,
  Eye,
  EyeOff,
  ExternalLink,
  ArrowUpDown,
  Gauge,
  KeyRound,
  Loader2,
  LockKeyhole,
  Pencil,
  Plus,
  RotateCcw,
  Search,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Trash2,
  XCircle,
} from "lucide-react";

type ExpireMode = "never" | "7" | "30" | "90" | "custom";
type TokenLimitUnit = "token" | "k" | "m" | "b";
type StatusFilter = "all" | "active" | "expired" | "quota_exhausted" | "expiring_soon";
type APIKeyStatus = "active" | "expired" | "quota_exhausted";
type SortMode = "created_desc" | "last_used_desc" | "quota_usage_desc" | "name_asc";

const KEY_REVEAL_MS = 30_000;
const EXPIRING_SOON_MS = 7 * 24 * 60 * 60 * 1000;

interface CreateKeyFormState {
  name: string;
  key: string;
  quotaLimit: string;
  expireMode: ExpireMode;
  expiresAt: string;
  allowedGroupIds: number[];
  limits: LimitsFormState;
}

interface EditKeyFormState {
  name: string;
  quotaLimit: string;
  expireMode: ExpireMode;
  expiresAt: string;
  allowedGroupIds: number[];
  limits: LimitsFormState;
}

interface LimitsFormState {
  modelAllow: string[];
  modelDeny: string[];
  planAllow: string[];
  rpm: string;
  rpd: string;
  maxConcurrency: string;
  costLimit5h: string;
  costLimit7d: string;
  costLimit30d: string;
  tokenLimit5h: string;
  tokenLimit5hUnit: TokenLimitUnit;
  tokenLimit7d: string;
  tokenLimit7dUnit: TokenLimitUnit;
  tokenLimit30d: string;
  tokenLimit30dUnit: TokenLimitUnit;
  disableImageGeneration: boolean;
}

const TOKEN_LIMIT_UNIT_MULTIPLIERS: Record<TokenLimitUnit, number> = {
  token: 1,
  k: 1_000,
  m: 1_000_000,
  b: 1_000_000_000,
};

const TOKEN_LIMIT_UNIT_ORDER: TokenLimitUnit[] = ["b", "m", "k", "token"];

const emptyLimitsForm: LimitsFormState = {
  modelAllow: [],
  modelDeny: [],
  planAllow: [],
  rpm: "",
  rpd: "",
  maxConcurrency: "",
  costLimit5h: "",
  costLimit7d: "",
  costLimit30d: "",
  tokenLimit5h: "",
  tokenLimit5hUnit: "token",
  tokenLimit7d: "",
  tokenLimit7dUnit: "token",
  tokenLimit30d: "",
  tokenLimit30dUnit: "token",
  disableImageGeneration: false,
};

const initialCreateForm: CreateKeyFormState = {
  name: "",
  key: "",
  quotaLimit: "",
  expireMode: "never",
  expiresAt: "",
  allowedGroupIds: [],
  limits: emptyLimitsForm,
};

const initialEditForm: EditKeyFormState = {
  name: "",
  quotaLimit: "",
  expireMode: "never",
  expiresAt: "",
  allowedGroupIds: [],
  limits: emptyLimitsForm,
};

export default function APIKeys() {
  const { t } = useTranslation();
  const [createDialogOpen, setCreateDialogOpen] = useState(false);
  const [createForm, setCreateForm] =
    useState<CreateKeyFormState>(initialCreateForm);
  const [createdKeyId, setCreatedKeyId] = useState<number | null>(null);
  const [createdReveal, setCreatedReveal] = useState<{
    id: number;
    name: string;
    key: string;
  } | null>(null);
  const [createdRevealAck, setCreatedRevealAck] = useState(false);
  const [visibleKeys, setVisibleKeys] = useState<Set<number>>(new Set());
  const revealTimers = useRef<Map<number, number>>(new Map());
  const [activeTab, setActiveTab] = useState<"keys" | "token-usage">("keys");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [searchQuery, setSearchQuery] = useState("");
  const [sortMode, setSortMode] = useState<SortMode>("created_desc");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [deletingIds, setDeletingIds] = useState<Set<number>>(new Set());
  const [editingKey, setEditingKey] = useState<APIKeyRow | null>(null);
  const [editForm, setEditForm] = useState<EditKeyFormState>(initialEditForm);
  const [editTab, setEditTab] = useState<"basic" | "limits">("basic");
  const [editDirty, setEditDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [savingPublicUsagePage, setSavingPublicUsagePage] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  useEffect(() => {
    return () => {
      revealTimers.current.forEach((timer) => window.clearTimeout(timer));
      revealTimers.current.clear();
    };
  }, []);

  const loadKeys = useCallback(async () => {
    const [keysResponse, groupsResponse, modelsResponse, settingsResponse] = await Promise.all([
      api.getAPIKeys(),
      api.listAccountGroups().catch(() => ({ groups: [] })),
      api
        .getModels()
        .catch(() => ({ models: [] as string[] })) as Promise<{
        models?: string[];
      }>,
      api.getSettings().catch((): SystemSettings | null => null),
    ]);
    return {
      keys: keysResponse.keys ?? [],
      groups: groupsResponse.groups ?? [],
      modelOptions: modelsResponse.models ?? [],
      settings: settingsResponse,
    };
  }, []);

  const { data, setData, loading, error, reload, reloadSilently } = useDataLoader<{
    keys: APIKeyRow[];
    groups: AccountGroup[];
    modelOptions: string[];
    settings: SystemSettings | null;
  }>({
    initialData: { keys: [], groups: [], modelOptions: [], settings: null },
    load: loadKeys,
  });
  const keys = data.keys;
  const groups = data.groups;
  const modelOptions = data.modelOptions;
  const publicUsagePageEnabled = data.settings?.public_key_usage_page_enabled ?? true;
  const showInitialSkeleton = loading && keys.length === 0;

  const handleRefresh = useCallback(async () => {
    setRefreshing(true);
    try {
      await reloadSilently();
    } finally {
      setRefreshing(false);
    }
  }, [reloadSilently]);

  const keyUsageUrl = useMemo(() => {
    if (typeof window === "undefined") return "/key-usage";
    return `${window.location.origin}/key-usage`;
  }, []);

  const statusCounts = useMemo(() => {
    const counts = {
      all: keys.length,
      active: 0,
      expired: 0,
      quota_exhausted: 0,
      expiring_soon: 0,
    };
    const now = Date.now();
    for (const keyRow of keys) {
      const status = getAPIKeyStatus(keyRow);
      if (status === "active") counts.active += 1;
      else if (status === "expired") counts.expired += 1;
      else counts.quota_exhausted += 1;

      if (
        status === "active" &&
        keyRow.expires_at &&
        new Date(keyRow.expires_at).getTime() - now <= EXPIRING_SOON_MS &&
        new Date(keyRow.expires_at).getTime() > now
      ) {
        counts.expiring_soon += 1;
      }
    }
    return counts;
  }, [keys]);

  const filteredKeys = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    const now = Date.now();
    const filtered = keys.filter((keyRow) => {
      const status = getAPIKeyStatus(keyRow);
      if (statusFilter === "active" && status !== "active") return false;
      if (statusFilter === "expired" && status !== "expired") return false;
      if (statusFilter === "quota_exhausted" && status !== "quota_exhausted")
        return false;
      if (statusFilter === "expiring_soon") {
        if (status !== "active" || !keyRow.expires_at) return false;
        const expiresAt = new Date(keyRow.expires_at).getTime();
        if (!(expiresAt > now && expiresAt - now <= EXPIRING_SOON_MS))
          return false;
      }
      if (!q) return true;
      const haystack = [
        keyRow.name,
        keyRow.key,
        keyRow.raw_key,
        String(keyRow.id),
      ]
        .filter(Boolean)
        .join(" ")
        .toLowerCase();
      return haystack.includes(q);
    });

    const sorted = filtered.slice();
    sorted.sort((a, b) => {
      switch (sortMode) {
        case "name_asc":
          return a.name.localeCompare(b.name, undefined, {
            sensitivity: "base",
          });
        case "last_used_desc": {
          const aTime = a.last_used_at
            ? new Date(a.last_used_at).getTime()
            : 0;
          const bTime = b.last_used_at
            ? new Date(b.last_used_at).getTime()
            : 0;
          if (bTime !== aTime) return bTime - aTime;
          return (
            new Date(b.created_at || 0).getTime() -
            new Date(a.created_at || 0).getTime()
          );
        }
        case "quota_usage_desc": {
          const aRatio =
            a.quota_limit > 0 ? a.quota_used / a.quota_limit : -1;
          const bRatio =
            b.quota_limit > 0 ? b.quota_used / b.quota_limit : -1;
          if (bRatio !== aRatio) return bRatio - aRatio;
          return b.total_used - a.total_used;
        }
        case "created_desc":
        default:
          return (
            new Date(b.created_at || 0).getTime() -
            new Date(a.created_at || 0).getTime()
          );
      }
    });
    return sorted;
  }, [keys, searchQuery, sortMode, statusFilter]);

  const sortOptions = useMemo(
    () => [
      { label: t("apiKeys.sortCreated"), value: "created_desc" },
      { label: t("apiKeys.sortLastUsed"), value: "last_used_desc" },
      { label: t("apiKeys.sortQuotaUsage"), value: "quota_usage_desc" },
      { label: t("apiKeys.sortName"), value: "name_asc" },
    ],
    [t],
  );

  const expireOptions = useMemo(
    () => [
      { label: t("apiKeys.expireNever"), value: "never" },
      { label: t("apiKeys.expire7Days"), value: "7" },
      { label: t("apiKeys.expire30Days"), value: "30" },
      { label: t("apiKeys.expire90Days"), value: "90" },
      { label: t("apiKeys.expireCustom"), value: "custom" },
    ],
    [t],
  );

  const updateCreateForm = (patch: Partial<CreateKeyFormState>) => {
    setCreateForm((current) => ({ ...current, ...patch }));
  };

  const closeCreateDialog = () => {
    if (creating) return;
    setCreateDialogOpen(false);
  };

  const handleTogglePublicUsagePage = async () => {
    const nextEnabled = !publicUsagePageEnabled;
    setSavingPublicUsagePage(true);
    try {
      await api.updateSettings({ public_key_usage_page_enabled: nextEnabled });
      showToast(
        nextEnabled
          ? t("apiKeys.publicUsageEnabledToast")
          : t("apiKeys.publicUsageDisabledToast"),
        "success",
      );
      setData((current) => ({
        ...current,
        settings: current.settings
          ? {
              ...current.settings,
              public_key_usage_page_enabled: nextEnabled,
            }
          : current.settings,
      }));
      await reloadSilently();
    } catch (err) {
      showToast(getErrorMessage(err), "error");
    } finally {
      setSavingPublicUsagePage(false);
    }
  };

  const handleCreateKey = async (event?: FormEvent<HTMLFormElement>) => {
    event?.preventDefault();
    setCreating(true);
    try {
      const quotaLimitText = createForm.quotaLimit.trim();
      let quotaLimit: number | undefined;
      if (quotaLimitText) {
        quotaLimit = Number(quotaLimitText);
        if (!Number.isFinite(quotaLimit) || quotaLimit < 0) {
          showToast(t("apiKeys.quotaInvalid"), "error");
          return;
        }
      }

      const expirationPayload = buildExpirationPayload(createForm, t) as {
        expires_in_days?: number;
        expires_at?: string;
      };
      const payload = {
        name: createForm.name.trim() || t("apiKeys.defaultName"),
        ...(createForm.key.trim() ? { key: createForm.key.trim() } : {}),
        ...(quotaLimit && quotaLimit > 0 ? { quota_limit: quotaLimit } : {}),
        allowed_group_ids: createForm.allowedGroupIds,
        limits: limitsFormToPayload(createForm.limits),
        ...expirationPayload,
      };

      const result = await api.createAPIKey(payload);
      setCreatedKeyId(result.id);
      setCreatedReveal({
        id: result.id,
        name: result.name,
        key: result.key,
      });
      setCreatedRevealAck(false);
      setCreateForm(initialCreateForm);
      setCreateDialogOpen(false);
      setStatusFilter("all");
      setSearchQuery("");
      setActiveTab("keys");
      showToast(t("apiKeys.keyCreateSuccess"));
      void reloadSilently();
    } catch (error) {
      showToast(
        `${t("apiKeys.createFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setCreating(false);
    }
  };

  const handleDeleteKey = async (id: number) => {
    const confirmed = await confirm({
      title: t("apiKeys.deleteKeyTitle"),
      description: t("apiKeys.deleteKeyDesc"),
      confirmText: t("apiKeys.confirmDelete"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;

    setDeletingIds((prev) => new Set(prev).add(id));
    const previous = keys;
    setData((current) => ({
      ...current,
      keys: current.keys.filter((item) => item.id !== id),
    }));
    try {
      await api.deleteAPIKey(id);
      showToast(t("apiKeys.keyDeleted"));
      if (createdKeyId === id) setCreatedKeyId(null);
      setVisibleKeys((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
      void reloadSilently();
    } catch (error) {
      setData((current) => ({ ...current, keys: previous }));
      showToast(
        `${t("apiKeys.deleteFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setDeletingIds((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }
  };

  const [resettingIds, setResettingIds] = useState<Set<number>>(new Set());

  const handleResetQuota = async (keyRow: APIKeyRow) => {
    const confirmed = await confirm({
      title: t("apiKeys.resetQuotaTitle"),
      description: t("apiKeys.resetQuotaDesc"),
      confirmText: t("apiKeys.resetQuotaConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;

    setResettingIds((prev) => new Set(prev).add(keyRow.id));
    const previousUsed = keyRow.quota_used;
    setData((current) => ({
      ...current,
      keys: current.keys.map((item) =>
        item.id === keyRow.id ? { ...item, quota_used: 0 } : item,
      ),
    }));
    try {
      await api.updateAPIKey(keyRow.id, { reset_quota: true });
      showToast(t("apiKeys.resetQuotaSuccess"));
      void reloadSilently();
    } catch (error) {
      setData((current) => ({
        ...current,
        keys: current.keys.map((item) =>
          item.id === keyRow.id
            ? { ...item, quota_used: previousUsed }
            : item,
        ),
      }));
      showToast(
        `${t("apiKeys.resetQuotaFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setResettingIds((prev) => {
        const next = new Set(prev);
        next.delete(keyRow.id);
        return next;
      });
    }
  };

  const handleCopy = async (text: string) => {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
        showToast(t("common.copied"));
        return;
      }

      const textarea = document.createElement("textarea");
      textarea.value = text;
      textarea.setAttribute("readonly", "true");
      textarea.style.position = "fixed";
      textarea.style.opacity = "0";
      textarea.style.pointerEvents = "none";
      document.body.appendChild(textarea);
      textarea.select();
      textarea.setSelectionRange(0, text.length);
      const copied = document.execCommand("copy");
      document.body.removeChild(textarea);

      if (!copied) throw new Error("copy failed");
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const hideKey = useCallback((id: number) => {
    const existing = revealTimers.current.get(id);
    if (existing) {
      window.clearTimeout(existing);
      revealTimers.current.delete(id);
    }
    setVisibleKeys((prev) => {
      if (!prev.has(id)) return prev;
      const next = new Set(prev);
      next.delete(id);
      return next;
    });
  }, []);

  const toggleVisible = (id: number) => {
    setVisibleKeys((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        const existing = revealTimers.current.get(id);
        if (existing) {
          window.clearTimeout(existing);
          revealTimers.current.delete(id);
        }
        next.delete(id);
        return next;
      }
      next.add(id);
      const existing = revealTimers.current.get(id);
      if (existing) window.clearTimeout(existing);
      const timer = window.setTimeout(() => {
        hideKey(id);
      }, KEY_REVEAL_MS);
      revealTimers.current.set(id, timer);
      return next;
    });
  };

  const closeCreatedReveal = () => {
    if (!createdRevealAck) {
      showToast(t("apiKeys.createdRevealAckRequired"), "error");
      return;
    }
    setCreatedReveal(null);
    setCreatedRevealAck(false);
  };

  const startEditing = (keyRow: APIKeyRow) => {
    setEditingKey(keyRow);
    setEditForm({
      name: keyRow.name,
      quotaLimit: keyRow.quota_limit > 0 ? String(keyRow.quota_limit) : "",
      expireMode: keyRow.expires_at ? "custom" : "never",
      expiresAt: toDateTimeLocalValue(keyRow.expires_at),
      allowedGroupIds: keyRow.allowed_group_ids ?? [],
      limits: limitsFromAPIKey(keyRow.limits),
    });
    setEditDirty(false);
    setEditTab("basic");
  };

  const closeEditDialog = async () => {
    if (saving) return;
    if (editDirty) {
      const confirmed = await confirm({
        title: t("apiKeys.discardEditTitle"),
        description: t("apiKeys.discardEditDesc"),
        confirmText: t("apiKeys.discardEditConfirm"),
        tone: "warning",
      });
      if (!confirmed) return;
    }
    setEditingKey(null);
    setEditForm(initialEditForm);
    setEditTab("basic");
    setEditDirty(false);
  };

  const updateEditForm = (patch: Partial<EditKeyFormState>) => {
    setEditForm((current) => ({ ...current, ...patch }));
    setEditDirty(true);
  };

  const handleSaveEdit = async (event?: FormEvent<HTMLFormElement>) => {
    event?.preventDefault();
    if (!editingKey) return;
    const trimmed = editForm.name.trim();
    if (!trimmed) {
      showToast(t("apiKeys.nameRequired"), "error");
      return;
    }
    setSaving(true);
    try {
      const quotaLimit = parseQuotaLimit(editForm.quotaLimit, t);
      const expirationPayload = buildExpirationPayload(editForm, t, {
        clearNever: true,
      });
      const limitsPayload = limitsFormToPayload(editForm.limits);
      await api.updateAPIKey(editingKey.id, {
        name: trimmed,
        quota_limit: quotaLimit,
        allowed_group_ids: editForm.allowedGroupIds,
        limits: limitsPayload,
        ...expirationPayload,
      });

      // 乐观更新列表行，再静默同步
      setData((current) => ({
        ...current,
        keys: current.keys.map((item) => {
          if (item.id !== editingKey.id) return item;
          const nextExpires =
            "expires_at" in expirationPayload
              ? (expirationPayload.expires_at as string | null | undefined)
              : "expires_in_days" in expirationPayload &&
                  expirationPayload.expires_in_days
                ? new Date(
                    Date.now() +
                      Number(expirationPayload.expires_in_days) *
                        24 *
                        60 *
                        60 *
                        1000,
                  ).toISOString()
                : item.expires_at;
          return {
            ...item,
            name: trimmed,
            quota_limit: quotaLimit,
            allowed_group_ids: editForm.allowedGroupIds,
            limits: limitsPayload,
            expires_at: nextExpires ?? null,
          };
        }),
      }));

      showToast(t("apiKeys.keyUpdated"));
      setEditingKey(null);
      setEditForm(initialEditForm);
      setEditTab("basic");
      setEditDirty(false);
      void reloadSilently();
    } catch (error) {
      showToast(
        `${t("apiKeys.updateFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setSaving(false);
    }
  };

  if (showInitialSkeleton) {
    return <APIKeysSkeleton />;
  }

  return (
    <StateShell
      variant="page"
      loading={false}
      error={error && keys.length === 0 ? error : null}
      onRetry={() => void reload()}
      loadingTitle={t("apiKeys.loadingTitle")}
      loadingDescription={t("apiKeys.loadingDesc")}
      errorTitle={t("apiKeys.errorTitle")}
    >
      <>
        <PageHeader
          title={t("apiKeys.title")}
          description={t("apiKeys.description")}
          onRefresh={() => void handleRefresh()}
          refreshLabel={
            refreshing ? t("common.loading") : t("common.refresh")
          }
          actionMeta={
            <span className="inline-flex items-center gap-1.5">
              <span
                className={cn(
                  "size-1.5 rounded-full",
                  keys.length > 0 ? "bg-[hsl(var(--success))]" : "bg-[hsl(var(--warning))]",
                )}
              />
              {keys.length > 0
                ? t("apiKeys.authEnabledDesc")
                : t("apiKeys.authDisabledDesc")}
            </span>
          }
          actions={
            <Button
              onClick={() => setCreateDialogOpen(true)}
              className="max-sm:w-full"
            >
              <Plus className="size-3.5" />
              {t("apiKeys.createKey")}
            </Button>
          }
        />

        <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-4">
          <button
            type="button"
            className="text-left"
            onClick={() => {
              setStatusFilter("all");
              setActiveTab("keys");
            }}
          >
            <StatCard
              icon={<KeyRound className="size-[18px]" />}
              iconClass="purple"
              label={t("apiKeys.totalKeys")}
              value={statusCounts.all}
              sub={
                keys.length > 0
                  ? t("apiKeys.totalKeysDesc")
                  : t("apiKeys.noKeysShort")
              }
              className={cn(
                "h-full cursor-pointer",
                statusFilter === "all" && "ring-2 ring-primary/30",
              )}
            />
          </button>
          <button
            type="button"
            className="text-left"
            onClick={() => {
              setStatusFilter("active");
              setActiveTab("keys");
            }}
          >
            <StatCard
              icon={<ShieldCheck className="size-[18px]" />}
              iconClass="green"
              label={t("apiKeys.status.active")}
              value={statusCounts.active}
              sub={t("apiKeys.filterActiveDesc")}
              className={cn(
                "h-full cursor-pointer",
                statusFilter === "active" && "ring-2 ring-primary/30",
              )}
            />
          </button>
          <button
            type="button"
            className="text-left"
            onClick={() => {
              setStatusFilter("expiring_soon");
              setActiveTab("keys");
            }}
          >
            <StatCard
              icon={<CalendarClock className="size-[18px]" />}
              iconClass="amber"
              label={t("apiKeys.status.expiring_soon")}
              value={statusCounts.expiring_soon}
              sub={t("apiKeys.filterExpiringDesc")}
              className={cn(
                "h-full cursor-pointer",
                statusFilter === "expiring_soon" && "ring-2 ring-primary/30",
              )}
            />
          </button>
          <button
            type="button"
            className="text-left"
            onClick={() => {
              setStatusFilter(
                statusCounts.quota_exhausted > 0
                  ? "quota_exhausted"
                  : "expired",
              );
              setActiveTab("keys");
            }}
          >
            <StatCard
              icon={<XCircle className="size-[18px]" />}
              iconClass="red"
              label={t("apiKeys.filterIssues")}
              value={statusCounts.expired + statusCounts.quota_exhausted}
              sub={t("apiKeys.filterIssuesDesc", {
                expired: statusCounts.expired,
                exhausted: statusCounts.quota_exhausted,
              })}
              className={cn(
                "h-full cursor-pointer",
                (statusFilter === "expired" ||
                  statusFilter === "quota_exhausted") &&
                  "ring-2 ring-primary/30",
              )}
            />
          </button>
        </div>

        <div className="space-y-4">
          <div className="inline-flex items-center gap-0.5 rounded-xl border border-border bg-muted/30 p-0.5">
            {(
              [
                ["keys", t("apiKeys.tabKeys")],
                ["token-usage", t("apiKeys.tabTokenUsage")],
              ] as const
            ).map(([key, label]) => (
              <button
                key={key}
                type="button"
                onClick={() => setActiveTab(key)}
                className={cn(
                  "rounded-lg px-3.5 py-1.5 text-[13px] font-semibold transition-colors",
                  activeTab === key
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                {label}
              </button>
            ))}
          </div>

          {activeTab === "keys" && (
            <>
              <div className="toolbar-surface flex flex-col gap-2.5">
                <div className="flex items-center gap-1.5 overflow-x-auto [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
                  <span className="shrink-0 whitespace-nowrap text-[12px] font-semibold text-foreground">
                    {t("apiKeys.filter")}
                  </span>
                  {(
                    [
                      ["all", t("common.all"), statusCounts.all],
                      [
                        "active",
                        t("apiKeys.status.active"),
                        statusCounts.active,
                      ],
                      [
                        "expiring_soon",
                        t("apiKeys.status.expiring_soon"),
                        statusCounts.expiring_soon,
                      ],
                      [
                        "expired",
                        t("apiKeys.status.expired"),
                        statusCounts.expired,
                      ],
                      [
                        "quota_exhausted",
                        t("apiKeys.status.quota_exhausted"),
                        statusCounts.quota_exhausted,
                      ],
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
                <div className="flex flex-wrap items-center gap-2">
                  <div className="relative min-w-0 flex-1 sm:max-w-sm">
                    <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input
                      value={searchQuery}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setSearchQuery(event.target.value)
                      }
                      placeholder={t("apiKeys.searchPlaceholder")}
                      className="h-8 pl-8 text-[13px]"
                    />
                  </div>
                  <div className="flex min-w-[10.5rem] items-center gap-1.5">
                    <ArrowUpDown className="size-3.5 shrink-0 text-muted-foreground" />
                    <Select
                      value={sortMode}
                      onValueChange={(value) =>
                        setSortMode(value as SortMode)
                      }
                      options={sortOptions}
                      compact
                    />
                  </div>
                  <Badge variant="secondary" className="tabular-nums">
                    {t("apiKeys.showingCount", {
                      shown: filteredKeys.length,
                      total: keys.length,
                    })}
                  </Badge>
                  {refreshing ? (
                    <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
                  ) : null}
                </div>
              </div>

              <StateShell
                variant="section"
                isEmpty={keys.length === 0}
                emptyTitle={t("apiKeys.noKeys")}
                emptyDescription={t("apiKeys.noKeysDesc")}
                action={
                  <Button onClick={() => setCreateDialogOpen(true)}>
                    <Plus className="size-3.5" />
                    {t("apiKeys.createKey")}
                  </Button>
                }
              >
                {keys.length > 0 && filteredKeys.length === 0 ? (
                  <div className="flex min-h-[220px] flex-col items-center justify-center gap-2 rounded-2xl border border-dashed border-border bg-card/60 px-6 py-10 text-center">
                    <Search className="size-5 text-muted-foreground" />
                    <div className="text-sm font-semibold text-foreground">
                      {t("apiKeys.noFilterResults")}
                    </div>
                    <p className="max-w-sm text-sm text-muted-foreground">
                      {t("apiKeys.noFilterResultsDesc")}
                    </p>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => {
                        setStatusFilter("all");
                        setSearchQuery("");
                      }}
                    >
                      {t("apiKeys.clearFilters")}
                    </Button>
                  </div>
                ) : (
                  <>
                    {/* Mobile cards */}
                    <div className="grid gap-3 lg:hidden">
                      {filteredKeys.map((keyRow) => {
                        const isVisible = visibleKeys.has(keyRow.id);
                        const isNew = createdKeyId === keyRow.id;
                        const isBusy =
                          deletingIds.has(keyRow.id) ||
                          resettingIds.has(keyRow.id);
                        const displayKey = isVisible
                          ? keyRow.raw_key || keyRow.key
                          : keyRow.key;
                        const copyValue = keyRow.raw_key || keyRow.key;
                        return (
                          <div
                            key={keyRow.id}
                            className={cn(
                              "rounded-2xl border border-border/80 bg-card p-3.5 shadow-sm transition-opacity",
                              isNew &&
                                "ring-1 ring-[hsl(var(--success))]/35 bg-[hsl(var(--success-bg))]",
                              isBusy && "opacity-70",
                            )}
                          >
                            <div className="flex items-start justify-between gap-2">
                              <div className="min-w-0">
                                <div className="flex flex-wrap items-center gap-1.5">
                                  <span className="truncate text-sm font-semibold text-foreground">
                                    {keyRow.name}
                                  </span>
                                  {isNew ? (
                                    <Badge
                                      variant="outline"
                                      className="border-transparent bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]"
                                    >
                                      {t("apiKeys.newBadge")}
                                    </Badge>
                                  ) : null}
                                  <KeyStatusBadge
                                    status={getAPIKeyStatus(keyRow)}
                                    t={t}
                                  />
                                  {isBusy ? (
                                    <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
                                  ) : null}
                                </div>
                                <div className="mt-1 text-[11px] text-muted-foreground">
                                  {t("apiKeys.lastUsedLabel")}:{" "}
                                  {formatLastUsed(keyRow, t)}
                                  {" · "}
                                  {formatExpiration(keyRow, t)}
                                </div>
                              </div>
                            </div>

                            <div className="mt-3 flex items-center gap-1.5">
                              <code
                                className="min-w-0 flex-1 truncate rounded-lg bg-muted/70 px-2.5 py-1.5 font-mono text-[12px] tabular-nums text-foreground"
                                title={displayKey}
                              >
                                {displayKey}
                              </code>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                disabled={!keyRow.raw_key}
                                onClick={() => toggleVisible(keyRow.id)}
                                title={
                                  isVisible
                                    ? t("apiKeys.hideKey")
                                    : t("apiKeys.showKey")
                                }
                              >
                                {isVisible ? (
                                  <EyeOff className="size-3.5" />
                                ) : (
                                  <Eye className="size-3.5" />
                                )}
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                onClick={() => void handleCopy(copyValue)}
                                title={t("common.copy")}
                              >
                                <Copy className="size-3.5" />
                              </Button>
                            </div>

                            <div className="mt-3 space-y-2">
                              <div className="flex flex-wrap items-center gap-1.5">
                                <AllowedGroupsDisplay
                                  ids={keyRow.allowed_group_ids ?? []}
                                  groups={groups}
                                  t={t}
                                />
                              </div>
                              <div className="rounded-xl border border-border/70 bg-muted/20 px-2.5 py-2">
                                <div className="flex items-center justify-between gap-2 text-[11px]">
                                  <span className="font-semibold text-muted-foreground">
                                    {t("apiKeys.quotaColumn")}
                                  </span>
                                  <span className="font-medium text-foreground">
                                    {formatQuotaLimit(keyRow, t)}
                                  </span>
                                </div>
                                {keyRow.quota_limit > 0 ? (
                                  <UsageBar
                                    used={keyRow.quota_used}
                                    limit={keyRow.quota_limit}
                                    className="mt-1.5"
                                  />
                                ) : null}
                                {keyRow.window_usage && keyRow.limits ? (
                                  <WindowCostBars
                                    limits={keyRow.limits}
                                    usage={keyRow.window_usage}
                                  />
                                ) : null}
                              </div>
                            </div>

                            <div className="mt-3 flex flex-wrap gap-1.5">
                              {keyRow.quota_limit > 0 ? (
                                <Button
                                  variant="outline"
                                  size="sm"
                                  disabled={resettingIds.has(keyRow.id)}
                                  onClick={() => void handleResetQuota(keyRow)}
                                  className="min-w-[7rem] flex-1"
                                >
                                  {resettingIds.has(keyRow.id) ? (
                                    <Loader2 className="size-3.5 animate-spin" />
                                  ) : (
                                    <RotateCcw className="size-3.5" />
                                  )}
                                  {t("apiKeys.resetQuota")}
                                </Button>
                              ) : null}
                              <Button
                                variant="outline"
                                size="sm"
                                disabled={isBusy}
                                onClick={() => startEditing(keyRow)}
                                className="min-w-[6rem] flex-1"
                              >
                                <Pencil className="size-3.5" />
                                {t("apiKeys.editKey")}
                              </Button>
                              <Button
                                variant="destructive"
                                size="sm"
                                disabled={deletingIds.has(keyRow.id)}
                                onClick={() => void handleDeleteKey(keyRow.id)}
                                className="min-w-[6rem] flex-1"
                              >
                                {deletingIds.has(keyRow.id) ? (
                                  <Loader2 className="size-3.5 animate-spin" />
                                ) : (
                                  <Trash2 className="size-3.5" />
                                )}
                                {t("common.delete")}
                              </Button>
                            </div>
                          </div>
                        );
                      })}
                    </div>

                    {/* Desktop table */}
                    <div className="data-table-shell hidden lg:block">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>{t("common.name")}</TableHead>
                            <TableHead>{t("apiKeys.keyColumn")}</TableHead>
                            <TableHead>{t("apiKeys.allowedGroups")}</TableHead>
                            <TableHead>{t("apiKeys.quotaColumn")}</TableHead>
                            <TableHead>{t("apiKeys.lastUsedColumn")}</TableHead>
                            <TableHead>{t("apiKeys.expiresColumn")}</TableHead>
                            <TableHead className="text-right">
                              {t("common.actions")}
                            </TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {filteredKeys.map((keyRow) => {
                            const isVisible = visibleKeys.has(keyRow.id);
                            const isNew = createdKeyId === keyRow.id;
                            const isBusy =
                              deletingIds.has(keyRow.id) ||
                              resettingIds.has(keyRow.id);
                            const displayKey = isVisible
                              ? keyRow.raw_key || keyRow.key
                              : keyRow.key;
                            const copyValue = keyRow.raw_key || keyRow.key;
                            return (
                              <TableRow
                                key={keyRow.id}
                                className={cn(
                                  isNew && "bg-[hsl(var(--success-bg))]",
                                  isBusy && "opacity-70",
                                )}
                              >
                                <TableCell className="font-medium text-foreground">
                                  <div className="flex min-w-[140px] flex-col gap-1.5">
                                    <div className="flex flex-wrap items-center gap-1.5">
                                      <span className="truncate">
                                        {keyRow.name}
                                      </span>
                                      {isNew ? (
                                        <Badge
                                          variant="outline"
                                          className="border-transparent bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]"
                                        >
                                          {t("apiKeys.newBadge")}
                                        </Badge>
                                      ) : null}
                                      {isBusy ? (
                                        <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
                                      ) : null}
                                    </div>
                                    <KeyStatusBadge
                                      status={getAPIKeyStatus(keyRow)}
                                      t={t}
                                    />
                                  </div>
                                </TableCell>
                                <TableCell>
                                  <div className="flex min-w-[240px] items-center gap-1">
                                    <code
                                      className="min-w-0 max-w-[360px] truncate rounded-md bg-muted/70 px-2 py-1 font-mono text-[12px] tabular-nums text-foreground"
                                      title={displayKey}
                                    >
                                      {displayKey}
                                    </code>
                                    <Button
                                      variant="ghost"
                                      size="icon-xs"
                                      disabled={!keyRow.raw_key}
                                      onClick={() => toggleVisible(keyRow.id)}
                                      title={
                                        isVisible
                                          ? t("apiKeys.hideKey")
                                          : t("apiKeys.showKey")
                                      }
                                    >
                                      {isVisible ? (
                                        <EyeOff className="size-3.5" />
                                      ) : (
                                        <Eye className="size-3.5" />
                                      )}
                                    </Button>
                                    <Button
                                      variant="ghost"
                                      size="icon-xs"
                                      onClick={() =>
                                        void handleCopy(copyValue)
                                      }
                                      title={t("common.copy")}
                                    >
                                      <Copy className="size-3.5" />
                                    </Button>
                                  </div>
                                </TableCell>
                                <TableCell className="min-w-[160px]">
                                  <AllowedGroupsDisplay
                                    ids={keyRow.allowed_group_ids ?? []}
                                    groups={groups}
                                    t={t}
                                  />
                                </TableCell>
                                <TableCell className="min-w-[160px] text-sm text-muted-foreground">
                                  <div className="space-y-1">
                                    <div className="font-medium text-foreground">
                                      {formatQuotaLimit(keyRow, t)}
                                    </div>
                                    {keyRow.quota_limit > 0 ? (
                                      <UsageBar
                                        used={keyRow.quota_used}
                                        limit={keyRow.quota_limit}
                                        className="w-28"
                                        showPercent
                                      />
                                    ) : null}
                                    {keyRow.total_used > 0 ? (
                                      <div className="text-[11px] text-muted-foreground">
                                        {t("apiKeys.totalUsedLabel", {
                                          amount: formatUSD(keyRow.total_used),
                                        })}
                                        {keyRow.reset_count > 0 ? (
                                          <span className="ml-1.5">
                                            (
                                            {t("apiKeys.resetCountLabel", {
                                              count: keyRow.reset_count,
                                            })}
                                            )
                                          </span>
                                        ) : null}
                                      </div>
                                    ) : null}
                                    {keyRow.window_usage && keyRow.limits ? (
                                      <WindowCostBars
                                        limits={keyRow.limits}
                                        usage={keyRow.window_usage}
                                      />
                                    ) : null}
                                  </div>
                                </TableCell>
                                <TableCell className="whitespace-nowrap text-muted-foreground">
                                  <div className="flex flex-col gap-0.5">
                                    <span>{formatLastUsed(keyRow, t)}</span>
                                    <span className="text-[11px] text-muted-foreground/80">
                                      {t("apiKeys.createdLabel")}:{" "}
                                      {formatRelativeTime(keyRow.created_at, {
                                        variant: "compact",
                                      })}
                                    </span>
                                  </div>
                                </TableCell>
                                <TableCell className="text-muted-foreground">
                                  {formatExpiration(keyRow, t)}
                                </TableCell>
                                <TableCell>
                                  <div className="flex flex-wrap items-center justify-end gap-1.5">
                                    {keyRow.quota_limit > 0 ? (
                                      <Button
                                        variant="outline"
                                        size="sm"
                                        disabled={resettingIds.has(keyRow.id)}
                                        onClick={() =>
                                          void handleResetQuota(keyRow)
                                        }
                                        title={t("apiKeys.resetQuota")}
                                      >
                                        {resettingIds.has(keyRow.id) ? (
                                          <Loader2 className="size-3.5 animate-spin" />
                                        ) : (
                                          <RotateCcw className="size-3.5" />
                                        )}
                                        {t("apiKeys.resetQuota")}
                                      </Button>
                                    ) : null}
                                    <Button
                                      variant="outline"
                                      size="sm"
                                      disabled={isBusy}
                                      onClick={() => startEditing(keyRow)}
                                      title={t("apiKeys.editKey")}
                                    >
                                      <Pencil className="size-3.5" />
                                      {t("apiKeys.editKey")}
                                    </Button>
                                    <Button
                                      variant="destructive"
                                      size="sm"
                                      disabled={deletingIds.has(keyRow.id)}
                                      onClick={() =>
                                        void handleDeleteKey(keyRow.id)
                                      }
                                      title={t("common.delete")}
                                    >
                                      {deletingIds.has(keyRow.id) ? (
                                        <Loader2 className="size-3.5 animate-spin" />
                                      ) : (
                                        <Trash2 className="size-3.5" />
                                      )}
                                      {t("common.delete")}
                                    </Button>
                                  </div>
                                </TableCell>
                              </TableRow>
                            );
                          })}
                        </TableBody>
                      </Table>
                    </div>
                  </>
                )}
              </StateShell>

              <div className="overflow-hidden rounded-xl border border-border bg-card/85 shadow-sm">
                <button
                  type="button"
                  onClick={() => setSettingsOpen((open) => !open)}
                  className="flex w-full items-center justify-between gap-3 px-4 py-3 text-left transition-colors hover:bg-muted/30"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                      <LockKeyhole className="size-3.5" />
                    </div>
                    <div className="min-w-0">
                      <div className="text-sm font-semibold text-foreground">
                        {t("apiKeys.securityTitle")}
                      </div>
                      <div className="mt-0.5 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                        <span>
                          {keys.length > 0
                            ? t("apiKeys.authEnabled")
                            : t("apiKeys.authDisabled")}
                        </span>
                        <span className="text-border">·</span>
                        <Badge
                          variant={
                            publicUsagePageEnabled ? "secondary" : "outline"
                          }
                          className={
                            publicUsagePageEnabled
                              ? "text-[hsl(var(--success))]"
                              : "text-muted-foreground"
                          }
                        >
                          {publicUsagePageEnabled
                            ? t("apiKeys.publicUsageEnabled")
                            : t("apiKeys.publicUsageDisabled")}
                        </Badge>
                      </div>
                    </div>
                  </div>
                  <ChevronDown
                    className={cn(
                      "size-4 shrink-0 text-muted-foreground transition-transform",
                      settingsOpen && "rotate-180",
                    )}
                  />
                </button>
                {settingsOpen ? (
                  <div className="space-y-3 border-t border-border px-4 py-3">
                    <p className="text-sm leading-relaxed text-muted-foreground">
                      {t("apiKeys.keyAuthNote")}
                    </p>
                    <div className="flex flex-col gap-3 rounded-xl border border-border/80 bg-muted/20 p-3 sm:flex-row sm:items-center sm:justify-between">
                      <div className="min-w-0">
                        <div className="text-sm font-semibold text-foreground">
                          {t("apiKeys.publicUsageTitle")}
                        </div>
                        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                          {t("apiKeys.publicUsageDesc")}
                        </p>
                        {publicUsagePageEnabled ? (
                          <div className="mt-2 flex flex-wrap items-center gap-2">
                            <code
                              className="min-w-0 max-w-full truncate rounded-md bg-muted px-2 py-1 font-mono text-[12px] text-foreground"
                              title={keyUsageUrl}
                            >
                              {keyUsageUrl}
                            </code>
                            <Button
                              variant="ghost"
                              size="icon-xs"
                              onClick={() => void handleCopy(keyUsageUrl)}
                              title={t("apiKeys.publicUsageCopyUrl")}
                            >
                              <Copy className="size-3.5" />
                            </Button>
                            <a
                              href={keyUsageUrl}
                              target="_blank"
                              rel="noopener noreferrer"
                              className="inline-flex items-center gap-1 text-xs font-semibold text-primary hover:underline"
                            >
                              <ExternalLink className="size-3.5" />
                              {t("apiKeys.publicUsageOpen")}
                            </a>
                          </div>
                        ) : null}
                      </div>
                      <Button
                        variant={
                          publicUsagePageEnabled ? "outline" : "default"
                        }
                        size="sm"
                        className="shrink-0"
                        onClick={() => void handleTogglePublicUsagePage()}
                        disabled={savingPublicUsagePage}
                      >
                        {publicUsagePageEnabled ? (
                          <EyeOff className="size-3.5" />
                        ) : (
                          <Eye className="size-3.5" />
                        )}
                        {savingPublicUsagePage
                          ? t("common.saving")
                          : publicUsagePageEnabled
                            ? t("apiKeys.disablePublicUsage")
                            : t("apiKeys.enablePublicUsage")}
                      </Button>
                    </div>
                  </div>
                ) : null}
              </div>
            </>
          )}

          {activeTab === "token-usage" && (
            <Card>
              <CardContent className="p-3 sm:p-4">
                <APIKeyTokenUsagePanel />
              </CardContent>
            </Card>
          )}
        </div>

        <Modal
          show={createDialogOpen}
          title={t("apiKeys.createTitle")}
          onClose={closeCreateDialog}
          contentClassName="sm:max-w-[620px]"
          footer={
            <>
              <Button
                type="button"
                variant="outline"
                onClick={closeCreateDialog}
                disabled={creating}
              >
                {t("common.cancel")}
              </Button>
              <Button
                type="submit"
                form="create-api-key-form"
                disabled={creating}
              >
                <Plus className="size-3.5" />
                {creating ? t("apiKeys.creating") : t("apiKeys.createKey")}
              </Button>
            </>
          }
        >
          <form
            id="create-api-key-form"
            className="space-y-5"
            onSubmit={(event) => void handleCreateKey(event)}
          >
            <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/20 p-3">
              <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                <Plus className="size-4" />
              </div>
              <p className="text-sm leading-relaxed text-muted-foreground">
                {t("apiKeys.createDesc")}
              </p>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <FormField label={t("apiKeys.nameLabel")}>
                <Input
                  placeholder={t("apiKeys.keyNamePlaceholder")}
                  value={createForm.name}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    updateCreateForm({ name: event.target.value })
                  }
                />
              </FormField>
              <FormField label={t("apiKeys.keyLabel")}>
                <Input
                  placeholder={t("apiKeys.keyValuePlaceholder")}
                  value={createForm.key}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    updateCreateForm({ key: event.target.value })
                  }
                />
              </FormField>
            </div>

            <div className="grid gap-4 sm:grid-cols-2">
              <FormField
                label={t("apiKeys.quotaLimitLabel")}
                icon={<CircleDollarSign className="size-3.5" />}
              >
                <Input
                  type="number"
                  min="0"
                  step="0.000001"
                  inputMode="decimal"
                  placeholder={t("apiKeys.quotaLimitPlaceholder")}
                  value={createForm.quotaLimit}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    updateCreateForm({ quotaLimit: event.target.value })
                  }
                />
              </FormField>
              <FormField
                label={t("apiKeys.expireModeLabel")}
                icon={<CalendarClock className="size-3.5" />}
              >
                <Select
                  value={createForm.expireMode}
                  onValueChange={(value) =>
                    updateCreateForm({ expireMode: value as ExpireMode })
                  }
                  options={expireOptions}
                  compact
                />
              </FormField>
            </div>

            {createForm.expireMode === "custom" ? (
              <FormField label={t("apiKeys.expiresAtLabel")}>
                <Input
                  type="datetime-local"
                  value={createForm.expiresAt}
                  onChange={(event: ChangeEvent<HTMLInputElement>) =>
                    updateCreateForm({ expiresAt: event.target.value })
                  }
                />
              </FormField>
            ) : null}

            <FormField
              label={t("apiKeys.allowedGroupsLabel")}
              icon={<ShieldCheck className="size-3.5" />}
              as="div"
            >
              <GroupMultiSelect
                groups={groups}
                value={createForm.allowedGroupIds}
                onChange={(allowedGroupIds) =>
                  updateCreateForm({ allowedGroupIds })
                }
                allLabel={t("apiKeys.allowedGroupsAll")}
                placeholder={t("apiKeys.allowedGroupsPlaceholder")}
                emptyLabel={t("accounts.groupsNone")}
              />
              <p className="mt-1.5 text-xs text-muted-foreground">
                {t("apiKeys.allowedGroupsHint")}
              </p>
            </FormField>

            <LimitsEditor
              value={createForm.limits}
              onChange={(limits) => updateCreateForm({ limits })}
              modelOptions={modelOptions}
            />
          </form>
        </Modal>

        <Modal
          show={Boolean(editingKey)}
          title={t("apiKeys.editTitle")}
          onClose={() => void closeEditDialog()}
          contentClassName="sm:max-w-[640px]"
          footer={
            <>
              <Button
                type="button"
                variant="outline"
                onClick={() => void closeEditDialog()}
                disabled={saving}
              >
                {t("common.cancel")}
              </Button>
              <Button
                type="submit"
                form="edit-api-key-form"
                disabled={saving || !editForm.name.trim() || !editDirty}
              >
                {saving ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : null}
                {saving ? t("common.saving") : t("common.save")}
              </Button>
            </>
          }
        >
          {editingKey ? (
            <form
              id="edit-api-key-form"
              className="space-y-5"
              onSubmit={(event) => void handleSaveEdit(event)}
            >
              <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/20 p-3">
                <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                  <Pencil className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="truncate text-sm font-semibold text-foreground">
                    {editingKey.name}
                  </div>
                  <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
                    {t("apiKeys.editDesc")}
                  </p>
                </div>
              </div>

              <div className="flex gap-1 rounded-xl border border-border bg-muted/50 p-1">
                <button
                  type="button"
                  onClick={() => setEditTab("basic")}
                  className={`flex-1 rounded-lg px-3 py-2 text-sm font-semibold transition-all ${
                    editTab === "basic"
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground"
                  }`}
                >
                  {t("apiKeys.editTabBasic")}
                </button>
                <button
                  type="button"
                  onClick={() => setEditTab("limits")}
                  className={`flex-1 rounded-lg px-3 py-2 text-sm font-semibold transition-all ${
                    editTab === "limits"
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground"
                  }`}
                >
                  {t("apiKeys.editTabLimits")}
                </button>
              </div>

              {editTab === "basic" ? (
                <>
                  <div className="grid gap-4 sm:grid-cols-2">
                    <FormField label={t("apiKeys.nameLabel")}>
                      <Input
                        placeholder={t("apiKeys.keyNamePlaceholder")}
                        value={editForm.name}
                        onChange={(event: ChangeEvent<HTMLInputElement>) =>
                          updateEditForm({ name: event.target.value })
                        }
                        autoFocus
                      />
                    </FormField>
                    <FormField
                      label={t("apiKeys.quotaLimitLabel")}
                      icon={<CircleDollarSign className="size-3.5" />}
                    >
                      <Input
                        type="number"
                        min="0"
                        step="0.000001"
                        inputMode="decimal"
                        placeholder={t("apiKeys.quotaLimitPlaceholder")}
                        value={editForm.quotaLimit}
                        onChange={(event: ChangeEvent<HTMLInputElement>) =>
                          updateEditForm({ quotaLimit: event.target.value })
                        }
                      />
                    </FormField>
                  </div>

                  <div className="grid gap-4 sm:grid-cols-2">
                    <FormField
                      label={t("apiKeys.expireModeLabel")}
                      icon={<CalendarClock className="size-3.5" />}
                    >
                      <Select
                        value={editForm.expireMode}
                        onValueChange={(value) =>
                          updateEditForm({ expireMode: value as ExpireMode })
                        }
                        options={expireOptions}
                        compact
                      />
                    </FormField>
                    {editForm.expireMode === "custom" ? (
                      <FormField label={t("apiKeys.expiresAtLabel")}>
                        <Input
                          type="datetime-local"
                          value={editForm.expiresAt}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            updateEditForm({ expiresAt: event.target.value })
                          }
                        />
                      </FormField>
                    ) : editForm.expireMode === "never" ? (
                      <div className="rounded-lg border border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                        {t("apiKeys.clearExpirationHint")}
                      </div>
                    ) : (
                      <div className="rounded-lg border border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                        {t("apiKeys.relativeExpirationHint", {
                          days: editForm.expireMode,
                        })}
                      </div>
                    )}
                  </div>

                  <FormField
                    label={t("apiKeys.allowedGroupsLabel")}
                    icon={<ShieldCheck className="size-3.5" />}
                    as="div"
                  >
                    <GroupMultiSelect
                      groups={groups}
                      value={editForm.allowedGroupIds}
                      onChange={(allowedGroupIds) =>
                        updateEditForm({ allowedGroupIds })
                      }
                      allLabel={t("apiKeys.allowedGroupsAll")}
                      placeholder={t("apiKeys.allowedGroupsPlaceholder")}
                      emptyLabel={t("accounts.groupsNone")}
                    />
                    <p className="mt-1.5 text-xs text-muted-foreground">
                      {t("apiKeys.allowedGroupsHint")}
                    </p>
                  </FormField>
                </>
              ) : (
                <LimitsEditor
                  value={editForm.limits}
                  onChange={(limits) => updateEditForm({ limits })}
                  modelOptions={modelOptions}
                  expanded
                />
              )}
            </form>
          ) : null}
        </Modal>

        <Modal
          show={Boolean(createdReveal)}
          title={t("apiKeys.createdRevealTitle")}
          onClose={closeCreatedReveal}
          contentClassName="sm:max-w-[560px]"
          footer={
            <>
              <Button
                type="button"
                variant="outline"
                onClick={() => {
                  if (createdReveal) void handleCopy(createdReveal.key);
                }}
              >
                <Copy className="size-3.5" />
                {t("common.copy")}
              </Button>
              <Button
                type="button"
                onClick={closeCreatedReveal}
                disabled={!createdRevealAck}
              >
                <Check className="size-3.5" />
                {t("apiKeys.createdRevealDone")}
              </Button>
            </>
          }
        >
          {createdReveal ? (
            <div className="space-y-4">
              <div className="flex items-start gap-3 rounded-xl border border-[hsl(var(--warning))]/25 bg-[hsl(var(--warning-bg))] p-3">
                <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-[hsl(var(--warning))]/15 text-[hsl(var(--warning))]">
                  <ShieldAlert className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="text-sm font-semibold text-foreground">
                    {t("apiKeys.createdRevealWarnTitle")}
                  </div>
                  <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
                    {t("apiKeys.createdRevealWarnDesc")}
                  </p>
                </div>
              </div>

              <div className="space-y-1.5">
                <div className="text-xs font-semibold text-muted-foreground">
                  {createdReveal.name}
                </div>
                <div className="flex items-center gap-2">
                  <code className="min-w-0 flex-1 break-all rounded-xl border border-border bg-muted/50 px-3 py-2.5 font-mono text-[13px] leading-relaxed text-foreground">
                    {createdReveal.key}
                  </code>
                  <Button
                    variant="outline"
                    size="icon"
                    onClick={() => void handleCopy(createdReveal.key)}
                    title={t("common.copy")}
                  >
                    <Copy className="size-4" />
                  </Button>
                </div>
              </div>

              <label className="flex cursor-pointer items-start gap-2.5 rounded-xl border border-border bg-card px-3 py-2.5">
                <input
                  type="checkbox"
                  className="mt-0.5 size-4 rounded border-border accent-primary"
                  checked={createdRevealAck}
                  onChange={(event) =>
                    setCreatedRevealAck(event.target.checked)
                  }
                />
                <span className="text-sm leading-relaxed text-foreground">
                  {t("apiKeys.createdRevealAck")}
                </span>
              </label>
            </div>
          ) : null}
        </Modal>

        {confirmDialog}
      </>
    </StateShell>
  );
}

type Translator = (key: string, options?: Record<string, unknown>) => string;

function parseQuotaLimit(raw: string, t: Translator): number {
  const quotaLimitText = raw.trim();
  if (!quotaLimitText) return 0;
  const quotaLimit = Number(quotaLimitText);
  if (!Number.isFinite(quotaLimit) || quotaLimit < 0) {
    throw new Error(t("apiKeys.quotaInvalid"));
  }
  return quotaLimit;
}

function buildExpirationPayload(
  form: Pick<CreateKeyFormState, "expireMode" | "expiresAt">,
  t: Translator,
  options: { clearNever?: boolean } = {},
): { expires_in_days?: number; expires_at?: string | null } {
  if (form.expireMode === "never")
    return options.clearNever ? { expires_at: null } : {};
  if (form.expireMode !== "custom") {
    return { expires_in_days: Number(form.expireMode) };
  }
  if (!form.expiresAt) {
    throw new Error(t("apiKeys.expiresAtRequired"));
  }
  const date = new Date(form.expiresAt);
  if (!Number.isFinite(date.getTime())) {
    throw new Error(t("apiKeys.expiresAtInvalid"));
  }
  if (date.getTime() <= Date.now()) {
    throw new Error(t("apiKeys.expiresAtPast"));
  }
  return { expires_at: date.toISOString() };
}

function limitsFromAPIKey(limits: APIKeyLimits | undefined): LimitsFormState {
  if (!limits) return emptyLimitsForm;
  const token5h = formatTokenLimitForForm(limits.token_limit_5h);
  const token7d = formatTokenLimitForForm(limits.token_limit_7d);
  const token30d = formatTokenLimitForForm(limits.token_limit_30d);
  return {
    modelAllow: Array.isArray(limits.model_allow) ? limits.model_allow : [],
    modelDeny: Array.isArray(limits.model_deny) ? limits.model_deny : [],
    planAllow: Array.isArray(limits.plan_allow) ? limits.plan_allow : [],
    rpm: limits.rpm && limits.rpm > 0 ? String(limits.rpm) : "",
    rpd: limits.rpd && limits.rpd > 0 ? String(limits.rpd) : "",
    maxConcurrency:
      limits.max_concurrency && limits.max_concurrency > 0
        ? String(limits.max_concurrency)
        : "",
    costLimit5h:
      limits.cost_limit_5h && limits.cost_limit_5h > 0
        ? String(limits.cost_limit_5h)
        : "",
    costLimit7d:
      limits.cost_limit_7d && limits.cost_limit_7d > 0
        ? String(limits.cost_limit_7d)
        : "",
    costLimit30d:
      limits.cost_limit_30d && limits.cost_limit_30d > 0
        ? String(limits.cost_limit_30d)
        : "",
    tokenLimit5h: token5h.value,
    tokenLimit5hUnit: token5h.unit,
    tokenLimit7d: token7d.value,
    tokenLimit7dUnit: token7d.unit,
    tokenLimit30d: token30d.value,
    tokenLimit30dUnit: token30d.unit,
    disableImageGeneration: limits.disable_image_generation === true,
  };
}

function formatTokenLimitForForm(value?: number): {
  value: string;
  unit: TokenLimitUnit;
} {
  if (!value || value <= 0 || !Number.isFinite(value)) {
    return { value: "", unit: "token" };
  }
  const integerValue = Math.trunc(value);
  const unit =
    TOKEN_LIMIT_UNIT_ORDER.find(
      (candidate) => integerValue % TOKEN_LIMIT_UNIT_MULTIPLIERS[candidate] === 0,
    ) ?? "token";
  return {
    value: String(integerValue / TOKEN_LIMIT_UNIT_MULTIPLIERS[unit]),
    unit,
  };
}

function parseTokenLimit(value: string, unit: TokenLimitUnit): number {
  const trimmed = value.trim();
  if (!trimmed) return 0;
  const amount = Number(trimmed);
  if (!Number.isFinite(amount) || amount <= 0) return 0;
  const tokens = amount * TOKEN_LIMIT_UNIT_MULTIPLIERS[unit];
  return Number.isInteger(tokens) && tokens > 0 ? tokens : 0;
}

// limitsFormToPayload 把表单值转为后端期望的 APIKeyLimits。
// 空字符串或 0 在后端被视为 "未配置";所以不一一过滤,直接把全部字段都发出去。
// (sanitizeAPIKeyLimits 在后端会把负值与空白清理掉)
function limitsFormToPayload(form: LimitsFormState): APIKeyLimits {
  const num = (s: string) => {
    const n = Number(s.trim());
    return Number.isFinite(n) && n > 0 ? n : 0;
  };
  const intNum = (s: string) => {
    const n = Number(s.trim());
    return Number.isInteger(n) && n > 0 ? n : 0;
  };
  return {
    model_allow: form.modelAllow.map((m) => m.trim()).filter(Boolean),
    model_deny: form.modelDeny.map((m) => m.trim()).filter(Boolean),
    plan_allow: form.planAllow.map((p) => p.trim()).filter(Boolean),
    rpm: intNum(form.rpm),
    rpd: intNum(form.rpd),
    max_concurrency: intNum(form.maxConcurrency),
    cost_limit_5h: num(form.costLimit5h),
    cost_limit_7d: num(form.costLimit7d),
    cost_limit_30d: num(form.costLimit30d),
    token_limit_5h: parseTokenLimit(form.tokenLimit5h, form.tokenLimit5hUnit),
    token_limit_7d: parseTokenLimit(form.tokenLimit7d, form.tokenLimit7dUnit),
    token_limit_30d: parseTokenLimit(form.tokenLimit30d, form.tokenLimit30dUnit),
    disable_image_generation: form.disableImageGeneration || undefined,
  };
}

function toDateTimeLocalValue(value?: string | null) {
  if (!value) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "";
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000);
  return local.toISOString().slice(0, 16);
}

function getAPIKeyStatus(keyRow: APIKeyRow): APIKeyStatus {
  if (keyRow.status === "expired" || keyRow.status === "quota_exhausted") {
    return keyRow.status;
  }
  if (
    keyRow.expires_at &&
    new Date(keyRow.expires_at).getTime() <= Date.now()
  ) {
    return "expired";
  }
  if (keyRow.quota_limit > 0 && keyRow.quota_used >= keyRow.quota_limit) {
    return "quota_exhausted";
  }
  return "active";
}

function usageToneClass(pct: number) {
  if (pct >= 90) return "bg-destructive";
  if (pct >= 70) return "bg-[hsl(var(--warning))]";
  return "bg-[hsl(var(--success))]";
}

function UsageBar({
  used,
  limit,
  className,
  showPercent = false,
}: {
  used: number;
  limit: number;
  className?: string;
  showPercent?: boolean;
}) {
  if (!limit || limit <= 0) return null;
  const pct = Math.min(100, Math.max(0, (used / limit) * 100));
  return (
    <div className={cn("space-y-1", className)}>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
        <div
          className={cn("h-full rounded-full transition-all", usageToneClass(pct))}
          style={{ width: `${pct}%` }}
        />
      </div>
      {showPercent ? (
        <div className="text-[10px] font-medium tabular-nums text-muted-foreground">
          {pct.toFixed(0)}%
        </div>
      ) : null}
    </div>
  );
}

function KeyStatusBadge({
  status,
  t,
}: {
  status: APIKeyStatus;
  t: Translator;
}) {
  const config = {
    active: {
      dot: "bg-[hsl(var(--success))]",
      className:
        "border-transparent bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]",
    },
    expired: {
      dot: "bg-muted-foreground",
      className: "border-transparent bg-muted text-muted-foreground",
    },
    quota_exhausted: {
      dot: "bg-destructive",
      className: "border-transparent bg-destructive/10 text-destructive",
    },
  }[status];

  return (
    <Badge
      variant="outline"
      className={cn("gap-1.5 px-1.5 py-0 text-[11px] font-semibold", config.className)}
    >
      <span className={cn("size-1.5 rounded-full", config.dot)} />
      {t(`apiKeys.status.${status}`)}
    </Badge>
  );
}

function formatQuotaLimit(keyRow: APIKeyRow, t: Translator) {
  if (!keyRow.quota_limit || keyRow.quota_limit <= 0) {
    return t("apiKeys.unlimited");
  }
  return t("apiKeys.quotaUsedOfLimit", {
    used: formatUSD(keyRow.quota_used),
    limit: formatUSD(keyRow.quota_limit),
  });
}

function formatExpiration(keyRow: APIKeyRow, t: Translator) {
  if (!keyRow.expires_at) {
    return t("apiKeys.neverExpires");
  }
  return formatBeijingTime(keyRow.expires_at);
}

function formatLastUsed(keyRow: APIKeyRow, t: Translator) {
  if (!keyRow.last_used_at) {
    return t("apiKeys.neverUsed");
  }
  return formatRelativeTime(keyRow.last_used_at, { variant: "compact" });
}

function formatUSD(value: number) {
  if (!Number.isFinite(value)) return "$0";
  if (value >= 1) return `$${value.toFixed(2)}`;
  if (value >= 0.01) return `$${value.toFixed(4)}`;
  return `$${value.toFixed(6)}`;
}

function WindowCostBars({
  limits,
  usage,
}: {
  limits: APIKeyLimits;
  usage: APIKeyWindowUsage;
}) {
  const bars: { label: string; used: number; limit: number }[] = [];
  if (limits.cost_limit_5h && limits.cost_limit_5h > 0) {
    bars.push({ label: "5h", used: usage.cost_5h, limit: limits.cost_limit_5h });
  }
  if (limits.cost_limit_7d && limits.cost_limit_7d > 0) {
    bars.push({ label: "7d", used: usage.cost_7d, limit: limits.cost_limit_7d });
  }
  if (limits.cost_limit_30d && limits.cost_limit_30d > 0) {
    bars.push({
      label: "30d",
      used: usage.cost_30d,
      limit: limits.cost_limit_30d,
    });
  }
  if (bars.length === 0) return null;
  return (
    <div className="mt-1.5 space-y-1">
      {bars.map((bar) => {
        const pct = Math.min(100, Math.max(0, (bar.used / bar.limit) * 100));
        return (
          <div key={bar.label} className="flex items-center gap-1.5">
            <span className="w-6 text-[10px] font-medium text-muted-foreground">
              {bar.label}
            </span>
            <div className="h-1.5 w-20 overflow-hidden rounded-full bg-muted">
              <div
                className={cn(
                  "h-full rounded-full transition-all",
                  usageToneClass(pct),
                )}
                style={{ width: `${pct}%` }}
              />
            </div>
            <span className="text-[10px] tabular-nums text-muted-foreground">
              {formatUSD(bar.used)}/{formatUSD(bar.limit)}
            </span>
          </div>
        );
      })}
    </div>
  );
}

function AllowedGroupsDisplay({
  ids,
  groups,
  t,
}: {
  ids: number[];
  groups: AccountGroup[];
  t: Translator;
}) {
  const selected = resolveGroups(ids, groups);
  if (ids.length === 0) {
    return <Badge variant="secondary">{t("apiKeys.allowedGroupsAll")}</Badge>;
  }
  if (selected.length === 0) {
    return (
      <Badge variant="destructive">{t("apiKeys.allowedGroupsMissing")}</Badge>
    );
  }
  return (
    <div className="flex flex-wrap gap-1">
      {selected.slice(0, 2).map((group) => (
        <span
          key={group.id}
          className="inline-flex items-center rounded-md bg-primary/10 px-1.5 py-0.5 text-[11px] font-semibold text-primary"
        >
          {group.name}
        </span>
      ))}
      {selected.length > 2 ? (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-semibold text-muted-foreground">
          +{selected.length - 2}
        </span>
      ) : null}
    </div>
  );
}

function resolveGroups(ids: number[], groups: AccountGroup[]): AccountGroup[] {
  const byID = new Map(groups.map((group) => [group.id, group]));
  return ids.map((id) => byID.get(id)).filter(Boolean) as AccountGroup[];
}

function GroupMultiSelect({
  groups,
  value,
  onChange,
  allLabel,
  placeholder,
  emptyLabel,
}: {
  groups: AccountGroup[];
  value: number[];
  onChange: (value: number[]) => void;
  allLabel: string;
  placeholder: string;
  emptyLabel: string;
}) {
  const selected = resolveGroups(value, groups);
  const summary =
    value.length === 0
      ? allLabel
      : selected.length > 0
        ? selected.map((group) => group.name).join(", ")
        : placeholder;

  return (
    <div className="rounded-lg border border-border bg-background p-2">
      <div className="mb-2 truncate text-sm font-medium text-foreground">
        {summary}
      </div>
      {groups.length === 0 ? (
        <div className="rounded-md bg-muted/50 px-2 py-2 text-sm text-muted-foreground">
          {emptyLabel}
        </div>
      ) : (
        <div className="flex flex-wrap gap-1.5">
          <button
            type="button"
            onClick={() => onChange([])}
            className={`rounded-md border px-2.5 py-1 text-xs font-semibold transition-colors ${
              value.length === 0
                ? "border-primary bg-primary text-primary-foreground"
                : "border-border bg-muted/30 text-muted-foreground hover:text-foreground"
            }`}
          >
            {allLabel}
          </button>
          {groups.map((group) => {
            const active = value.includes(group.id);
            return (
              <button
                key={group.id}
                type="button"
                onClick={() =>
                  onChange(
                    active
                      ? value.filter((id) => id !== group.id)
                      : [...value, group.id],
                  )
                }
                className={`rounded-md border px-2.5 py-1 text-xs font-semibold transition-colors ${
                  active
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border bg-muted/30 text-muted-foreground hover:text-foreground"
                }`}
              >
                {group.name}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

// LimitsEditor 渲染 API Key 的"高级限制"配置:模型策略 / 限流 / 成本 / Token 分区卡片。
// 默认折叠,有任一字段非默认时展开。
function LimitsEditor({
  value,
  onChange,
  modelOptions,
  expanded,
}: {
  value: LimitsFormState;
  onChange: (next: LimitsFormState) => void;
  modelOptions: string[];
  expanded?: boolean;
}) {
  const { t } = useTranslation();
  const hasAny =
    value.modelAllow.length > 0 ||
    value.modelDeny.length > 0 ||
    value.planAllow.length > 0 ||
    value.rpm !== "" ||
    value.rpd !== "" ||
    value.maxConcurrency !== "" ||
    value.costLimit5h !== "" ||
    value.costLimit7d !== "" ||
    value.costLimit30d !== "" ||
    value.tokenLimit5h !== "" ||
    value.tokenLimit7d !== "" ||
    value.tokenLimit30d !== "" ||
    value.disableImageGeneration;
  const [open, setOpen] = useState(hasAny || !!expanded);
  const tokenUnitOptions = useMemo(
    () =>
      (["token", "k", "m", "b"] as TokenLimitUnit[]).map((unit) => ({
        label: t(`apiKeys.limits.tokenUnits.${unit}`),
        triggerLabel: t(`apiKeys.limits.tokenUnitShort.${unit}`),
        value: unit,
      })),
    [t],
  );

  const patch = (next: Partial<LimitsFormState>) =>
    onChange({ ...value, ...next });

  const limitsContent = (
    <div
      className={cn(
        "space-y-3",
        expanded ? "" : "border-t border-border p-3",
      )}
    >
      <p className="text-[11px] leading-relaxed text-muted-foreground">
        {t("apiKeys.limits.desc")}
      </p>

      <LimitSection
        icon={<SlidersHorizontal className="size-3.5" />}
        title={t("apiKeys.limits.sectionModels")}
        description={t("apiKeys.limits.sectionModelsDesc")}
      >
        <div className="space-y-3">
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-foreground">
              {t("apiKeys.limits.modelAllow")}
            </label>
            <ChipInput
              value={value.modelAllow}
              onChange={(modelAllow) => patch({ modelAllow })}
              options={modelOptions}
              placeholder={t("apiKeys.limits.modelAllowPlaceholder")}
            />
            <p className="text-[10px] text-muted-foreground">
              {t("apiKeys.limits.modelAllowHint")}
            </p>
          </div>
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-foreground">
              {t("apiKeys.limits.modelDeny")}
            </label>
            <ChipInput
              value={value.modelDeny}
              onChange={(modelDeny) => patch({ modelDeny })}
              options={modelOptions}
              placeholder={t("apiKeys.limits.modelDenyPlaceholder")}
            />
            <p className="text-[10px] text-muted-foreground">
              {t("apiKeys.limits.modelDenyHint")}
            </p>
          </div>
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-foreground">
              {t("apiKeys.limits.planAllow")}
            </label>
            <PlanMultiSelect
              value={value.planAllow}
              onChange={(planAllow) => patch({ planAllow })}
              allLabel={t("apiKeys.limits.planAllowAll")}
            />
            <p className="text-[10px] text-muted-foreground">
              {t("apiKeys.limits.planAllowHint")}
            </p>
          </div>
          <div className="flex items-start justify-between gap-3 rounded-md border border-border/60 px-3 py-2">
            <div className="space-y-0.5">
              <label className="text-xs font-medium text-foreground">
                {t("apiKeys.limits.disableImageGeneration")}
              </label>
              <p className="text-[10px] text-muted-foreground">
                {t("apiKeys.limits.disableImageGenerationHint")}
              </p>
            </div>
            <Switch
              checked={value.disableImageGeneration}
              onCheckedChange={(disableImageGeneration) => patch({ disableImageGeneration })}
            />
          </div>
        </div>
      </LimitSection>

      <LimitSection
        icon={<Gauge className="size-3.5" />}
        title={t("apiKeys.limits.sectionRate")}
        description={t("apiKeys.limits.sectionRateDesc")}
      >
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <LimitNumberField
            label={t("apiKeys.limits.rpm")}
            value={value.rpm}
            onChange={(rpm) => patch({ rpm })}
            suffix={t("apiKeys.limits.rpmSuffix")}
          />
          <LimitNumberField
            label={t("apiKeys.limits.rpd")}
            value={value.rpd}
            onChange={(rpd) => patch({ rpd })}
            suffix={t("apiKeys.limits.rpdSuffix")}
          />
          <LimitNumberField
            label={t("apiKeys.limits.maxConcurrency")}
            value={value.maxConcurrency}
            onChange={(maxConcurrency) => patch({ maxConcurrency })}
            suffix={t("apiKeys.limits.concurrencySuffix")}
          />
        </div>
      </LimitSection>

      <LimitSection
        icon={<CircleDollarSign className="size-3.5" />}
        title={t("apiKeys.limits.sectionCost")}
        description={t("apiKeys.limits.sectionCostDesc")}
      >
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <LimitNumberField
            label={t("apiKeys.limits.cost5h")}
            value={value.costLimit5h}
            onChange={(costLimit5h) => patch({ costLimit5h })}
            suffix="$"
            step="0.01"
          />
          <LimitNumberField
            label={t("apiKeys.limits.cost7d")}
            value={value.costLimit7d}
            onChange={(costLimit7d) => patch({ costLimit7d })}
            suffix="$"
            step="0.01"
          />
          <LimitNumberField
            label={t("apiKeys.limits.cost30d")}
            value={value.costLimit30d}
            onChange={(costLimit30d) => patch({ costLimit30d })}
            suffix="$"
            step="0.01"
          />
        </div>
      </LimitSection>

      <LimitSection
        icon={<KeyRound className="size-3.5" />}
        title={t("apiKeys.limits.sectionTokens")}
        description={t("apiKeys.limits.sectionTokensDesc")}
      >
        <div className="grid grid-cols-1 gap-3">
          <TokenLimitField
            label={t("apiKeys.limits.tokens5h")}
            value={value.tokenLimit5h}
            unit={value.tokenLimit5hUnit}
            unitOptions={tokenUnitOptions}
            onValueChange={(tokenLimit5h) => patch({ tokenLimit5h })}
            onUnitChange={(tokenLimit5hUnit) => patch({ tokenLimit5hUnit })}
          />
          <TokenLimitField
            label={t("apiKeys.limits.tokens7d")}
            value={value.tokenLimit7d}
            unit={value.tokenLimit7dUnit}
            unitOptions={tokenUnitOptions}
            onValueChange={(tokenLimit7d) => patch({ tokenLimit7d })}
            onUnitChange={(tokenLimit7dUnit) => patch({ tokenLimit7dUnit })}
          />
          <TokenLimitField
            label={t("apiKeys.limits.tokens30d")}
            value={value.tokenLimit30d}
            unit={value.tokenLimit30dUnit}
            unitOptions={tokenUnitOptions}
            onValueChange={(tokenLimit30d) => patch({ tokenLimit30d })}
            onUnitChange={(tokenLimit30dUnit) => patch({ tokenLimit30dUnit })}
          />
        </div>
      </LimitSection>
    </div>
  );

  if (expanded) return limitsContent;

  return (
    <div className="overflow-hidden rounded-xl border border-border">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center justify-between px-3 py-2.5 text-left text-sm font-medium transition-colors hover:bg-muted/30"
      >
        <span className="flex items-center gap-2">
          <span>{t("apiKeys.limits.title")}</span>
          {hasAny && (
            <span className="inline-flex items-center rounded-full bg-primary/10 px-1.5 py-0.5 text-[10px] font-semibold text-primary">
              {t("apiKeys.limits.active")}
            </span>
          )}
        </span>
        <span className="text-xs text-muted-foreground">
          {open ? t("apiKeys.limits.hide") : t("apiKeys.limits.show")}
        </span>
      </button>
      {open && limitsContent}
    </div>
  );
}

function LimitSection({
  icon,
  title,
  description,
  children,
}: {
  icon: ReactNode;
  title: string;
  description: string;
  children: ReactNode;
}) {
  return (
    <div className="rounded-xl border border-border/80 bg-muted/15 p-3">
      <div className="mb-3 flex items-start gap-2.5">
        <div className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg bg-background text-primary shadow-sm ring-1 ring-border/70">
          {icon}
        </div>
        <div className="min-w-0">
          <div className="text-xs font-semibold uppercase tracking-wide text-foreground">
            {title}
          </div>
          <p className="mt-0.5 text-[11px] leading-relaxed text-muted-foreground">
            {description}
          </p>
        </div>
      </div>
      {children}
    </div>
  );
}

function APIKeysSkeleton() {
  return (
    <div className="space-y-4" aria-busy="true" aria-live="polite">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div className="space-y-2">
          <div className="h-8 w-40 animate-pulse rounded-lg bg-muted" />
          <div className="h-4 w-72 max-w-full animate-pulse rounded-md bg-muted/70" />
        </div>
        <div className="flex gap-2">
          <div className="h-8 w-20 animate-pulse rounded-lg bg-muted" />
          <div className="h-8 w-28 animate-pulse rounded-lg bg-muted" />
        </div>
      </div>
      <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <Card key={i} className="py-0">
            <CardContent className="space-y-3 p-4">
              <div className="h-3 w-16 animate-pulse rounded bg-muted" />
              <div className="h-7 w-12 animate-pulse rounded bg-muted" />
              <div className="h-3 w-24 animate-pulse rounded bg-muted/70" />
            </CardContent>
          </Card>
        ))}
      </div>
      <div className="h-9 w-52 animate-pulse rounded-xl bg-muted/60" />
      <div className="toolbar-surface space-y-2.5">
        <div className="flex gap-2">
          {[0, 1, 2, 3, 4].map((i) => (
            <div
              key={i}
              className="h-7 w-16 animate-pulse rounded-lg bg-muted"
            />
          ))}
        </div>
        <div className="h-8 w-full max-w-sm animate-pulse rounded-lg bg-muted/70" />
      </div>
      <div className="data-table-shell hidden overflow-hidden lg:block">
        <div className="space-y-0">
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <div
              key={i}
              className="flex items-center gap-4 border-b border-border/70 px-4 py-3.5 last:border-b-0"
            >
              <div className="h-4 w-28 animate-pulse rounded bg-muted" />
              <div className="h-4 w-40 animate-pulse rounded bg-muted/70" />
              <div className="h-4 w-20 animate-pulse rounded bg-muted/60" />
              <div className="h-4 w-24 animate-pulse rounded bg-muted/50" />
              <div className="ml-auto h-4 w-16 animate-pulse rounded bg-muted/40" />
            </div>
          ))}
        </div>
      </div>
      <div className="grid gap-3 lg:hidden">
        {[0, 1, 2].map((i) => (
          <div
            key={i}
            className="space-y-3 rounded-2xl border border-border/80 bg-card p-3.5"
          >
            <div className="h-4 w-32 animate-pulse rounded bg-muted" />
            <div className="h-8 w-full animate-pulse rounded-lg bg-muted/70" />
            <div className="h-12 w-full animate-pulse rounded-xl bg-muted/50" />
          </div>
        ))}
      </div>
    </div>
  );
}

// PLAN_FILTER_OPTIONS 与后端 cleanPlanAllow 的白名单保持一致(pro 与 prolite 相互独立)。
const PLAN_FILTER_OPTIONS = [
  "free",
  "plus",
  "pro",
  "prolite",
  "team",
  "k12",
  "go",
] as const;

// PlanMultiSelect 让 API Key 选择只调度哪些账号套餐。空表示不限套餐。
function PlanMultiSelect({
  value,
  onChange,
  allLabel,
}: {
  value: string[];
  onChange: (value: string[]) => void;
  allLabel: string;
}) {
  return (
    <div className="rounded-lg border border-border bg-background p-2">
      <div className="flex flex-wrap gap-1.5">
        <button
          type="button"
          onClick={() => onChange([])}
          className={`rounded-md border px-2.5 py-1 text-xs font-semibold transition-colors ${
            value.length === 0
              ? "border-primary bg-primary text-primary-foreground"
              : "border-border bg-muted/30 text-muted-foreground hover:text-foreground"
          }`}
        >
          {allLabel}
        </button>
        {PLAN_FILTER_OPTIONS.map((plan) => {
          const active = value.includes(plan);
          return (
            <button
              key={plan}
              type="button"
              onClick={() =>
                onChange(
                  active
                    ? value.filter((p) => p !== plan)
                    : [...value, plan],
                )
              }
              className={`rounded-md border px-2.5 py-1 text-xs font-semibold uppercase transition-colors ${
                active
                  ? "border-primary bg-primary/10 text-primary"
                  : "border-border bg-muted/30 text-muted-foreground hover:text-foreground"
              }`}
            >
              {plan}
            </button>
          );
        })}
      </div>
    </div>
  );
}

function TokenLimitField({
  label,
  value,
  unit,
  unitOptions,
  onValueChange,
  onUnitChange,
}: {
  label: string;
  value: string;
  unit: TokenLimitUnit;
  unitOptions: SelectOption[];
  onValueChange: (next: string) => void;
  onUnitChange: (next: TokenLimitUnit) => void;
}) {
  return (
    <div className="space-y-1">
      <label className="text-[11px] font-medium text-muted-foreground">
        {label}
      </label>
      <div className="grid grid-cols-[minmax(0,1fr)_112px] gap-2">
        <Input
          type="number"
          min="0"
          step="0.01"
          inputMode="decimal"
          value={value}
          onChange={(e) => onValueChange(e.target.value)}
          placeholder="0"
          className="text-xs"
        />
        <Select
          value={unit}
          onValueChange={(next) => onUnitChange(next as TokenLimitUnit)}
          options={unitOptions}
          compact
        />
      </div>
    </div>
  );
}

function LimitNumberField({
  label,
  value,
  onChange,
  suffix,
  step,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
  suffix?: string;
  step?: string;
}) {
  return (
    <div className="space-y-1">
      <label className="text-[11px] font-medium text-muted-foreground">
        {label}
      </label>
      <div className="relative">
        <Input
          type="number"
          min="0"
          step={step || "1"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="0"
          className="pr-10 text-xs"
        />
        {suffix && (
          <span className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 text-[10px] text-muted-foreground">
            {suffix}
          </span>
        )}
      </div>
    </div>
  );
}

function FormField({
  label,
  icon,
  children,
  as = "label",
}: {
  label: string;
  icon?: ReactNode;
  children: ReactNode;
  as?: "label" | "div";
}) {
  const Component = as;
  return (
    <Component className="block min-w-0">
      <span className="mb-1.5 flex items-center gap-1.5 text-xs font-semibold text-muted-foreground">
        {icon}
        {label}
      </span>
      {children}
    </Component>
  );
}


