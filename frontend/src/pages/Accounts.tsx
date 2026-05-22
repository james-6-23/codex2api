import type { ChangeEvent, DragEvent, ReactNode } from "react";
import { useCallback, useEffect, useRef, useState, useMemo } from "react";
import { api, getAdminKey } from "../api";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
import ToastNotice from "../components/ToastNotice";
import { useDataLoader } from "../hooks/useDataLoader";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { useToast } from "../hooks/useToast";
import type {
  AccountRow,
  AddAccountRequest,
  AddATAccountRequest,
  AddOpenAIResponsesAccountRequest,
  UpdateOpenAIResponsesAccountRequest,
  APIKeyRow,
  OpsOverviewResponse,
  AccountGroup,
  SystemSettings,
} from "../types";
import { getErrorMessage } from "../utils/error";
import { formatCompactEmail } from "../lib/utils";
import { formatRelativeTime, formatBeijingTime } from "../utils/time";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
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
import {
  Plus,
  RefreshCw,
  Trash2,
  Zap,
  FlaskConical,
  Ban,
  Timer,
  AlertTriangle,
  Upload,
  Download,
  ArrowDownToLine,
  KeyRound,
  ExternalLink,
  FileText,
  FileJson,
  BarChart3,
  Search,
  Fingerprint,
  FolderOpen,
  Lock,
  Unlock,
  RotateCcw,
  Pencil,
  Check,
  ChevronDown,
  Copy,
  Power,
  PowerOff,
  Hourglass,
  X,
  SlidersHorizontal,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import AccountUsageModal from "../components/AccountUsageModal";
import AccountQuotaDistributionChart from "../components/AccountQuotaDistributionChart";
import AccountRateLimitRecoveryChart from "../components/AccountRateLimitRecoveryChart";
import ChipInput from "../components/ChipInput";

const ACCOUNT_BATCH_CONCURRENCY = 6;
const ACCOUNT_REFRESH_BATCH_CONCURRENCY = 4;
const ACCOUNT_ANALYSIS_VISIBILITY_KEY = "codex2api:accounts:analysis-visible";
const ACCOUNT_VISIBLE_COLUMNS_KEY = "codex2api:accounts:visible-columns";
const ACCOUNT_TABLE_COLUMNS = [
  "sequence",
  "email",
  "tags",
  "groups",
  "plan",
  "status",
  "requests",
  "usage",
	"billed",
  "importTime",
  "updatedAt",
  "actions",
] as const;
const ACCOUNT_GROUP_COLORS = [
  "#2563eb",
  "#16a34a",
  "#d97706",
  "#dc2626",
  "#7c3aed",
  "#0891b2",
  "#64748b",
] as const;
type AccountTableColumn = (typeof ACCOUNT_TABLE_COLUMNS)[number];
type AccountGroupDraft = {
  id: number | null;
  name: string;
  description: string;
  color: string;
};

function getDefaultAccountVisibleColumns(): Record<
  AccountTableColumn,
  boolean
> {
  return Object.fromEntries(
    ACCOUNT_TABLE_COLUMNS.map((column) => [
      column,
      column !== "tags" && column !== "groups",
    ]),
  ) as Record<AccountTableColumn, boolean>;
}

function getInitialAccountVisibleColumns(): Record<
  AccountTableColumn,
  boolean
> {
  const fallback = getDefaultAccountVisibleColumns();
  try {
    const raw = window.localStorage.getItem(ACCOUNT_VISIBLE_COLUMNS_KEY);
    if (!raw) return fallback;
    const parsed = JSON.parse(raw) as Partial<
      Record<AccountTableColumn, boolean>
    >;
    return Object.fromEntries(
      ACCOUNT_TABLE_COLUMNS.map((column) => [
        column,
        column === "tags" || column === "groups"
          ? parsed[column] === true
          : parsed[column] !== false,
      ]),
    ) as Record<AccountTableColumn, boolean>;
  } catch {
    return fallback;
  }
}

function persistAccountVisibleColumns(
  columns: Record<AccountTableColumn, boolean>,
) {
  try {
    window.localStorage.setItem(
      ACCOUNT_VISIBLE_COLUMNS_KEY,
      JSON.stringify(columns),
    );
  } catch {
    // Keep the in-memory preference working when localStorage is unavailable.
  }
}

function getInitialAnalysisVisibility(): boolean {
  try {
    return (
      window.localStorage.getItem(ACCOUNT_ANALYSIS_VISIBILITY_KEY) !== "false"
    );
  } catch {
    return true;
  }
}

function persistAnalysisVisibility(visible: boolean) {
  try {
    window.localStorage.setItem(
      ACCOUNT_ANALYSIS_VISIBILITY_KEY,
      visible ? "true" : "false",
    );
  } catch {
    // Local storage can be unavailable in restricted browser modes; keep the in-memory toggle working.
  }
}

function parseModelTokens(value: string): string[] {
  const seen = new Set<string>();
  return value
    .split(/[\n,\t ]+/)
    .map((item) => item.trim())
    .filter((item) => {
      if (!item) return false;
      const key = item.toLowerCase();
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
}

function mergeModelLists(current: string[], incoming: string[]): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const item of [...current, ...incoming]) {
    const value = item.trim();
    if (!value) continue;
    const key = value.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(value);
  }
  return result;
}

function formatAccountName(account: AccountRow): string {
  if (account.openai_responses_api) {
    return account.name?.trim() || `ID ${account.id}`;
  }
  return account.email || account.name || `ID ${account.id}`;
}

async function runAccountBatch(
  ids: number[],
  action: (id: number) => Promise<unknown>,
  concurrency = ACCOUNT_BATCH_CONCURRENCY,
) {
  let success = 0;
  let fail = 0;
  let cursor = 0;
  const workerCount = Math.min(concurrency, ids.length);

  await Promise.all(
    Array.from({ length: workerCount }, async () => {
      while (cursor < ids.length) {
        const id = ids[cursor];
        cursor += 1;
        try {
          await action(id);
          success += 1;
        } catch {
          fail += 1;
        }
      }
    }),
  );

  return { success, fail };
}

export default function Accounts() {
  const { t } = useTranslation();
  const pageSizeOptions = [10, 20, 50, 100];
  const [showAdd, setShowAdd] = useState(false);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [statusFilter, setStatusFilter] = useState<
    | "all"
    | "normal"
    | "rate_limited"
    | "abnormal"
    | "banned"
    | "error"
    | "disabled"
    | "locked"
  >("all");
  const [searchQuery, setSearchQuery] = useState("");
  const [planFilter, setPlanFilter] = useState<
    "all" | "pro" | "prolite" | "plus" | "team" | "free"
  >("all");
  const [sortKey, setSortKey] = useState<
    "requests" | "usage" | "importTime" | null
  >(null);
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");
  const [addForm, setAddForm] = useState<AddAccountRequest>({
    refresh_token: "",
    proxy_url: "",
  });
  const [submitting, setSubmitting] = useState(false);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [refreshingIds, setRefreshingIds] = useState<Set<number>>(new Set());
  const [authJsonExportingIds, setAuthJsonExportingIds] = useState<Set<number>>(
    new Set(),
  );
  const [authJsonModal, setAuthJsonModal] = useState<{
    account: AccountRow;
    json: string;
  } | null>(null);
  const [batchLoading, setBatchLoading] = useState(false);
  const [batchRefreshing, setBatchRefreshing] = useState(false);
  const [batchTesting, setBatchTesting] = useState(false);
  const [lockingSubscriptionAccounts, setLockingSubscriptionAccounts] =
    useState(false);
  const [cleaningBanned, setCleaningBanned] = useState(false);
  const [cleaningRateLimited, setCleaningRateLimited] = useState(false);
  const [cleaningError, setCleaningError] = useState(false);
  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [usageAccount, setUsageAccount] = useState<AccountRow | null>(null);
  const [editingAccount, setEditingAccount] = useState<AccountRow | null>(null);
  const [editSubmitting, setEditSubmitting] = useState(false);
  const [editTab, setEditTab] = useState<"scheduler" | "account">("scheduler");
  const [scoreMode, setScoreMode] = useState<"default" | "custom">("default");
  const [scoreInput, setScoreInput] = useState("");
  const [concurrencyMode, setConcurrencyMode] = useState<"default" | "custom">(
    "default",
  );
  const [concurrencyInput, setConcurrencyInput] = useState("");
  const [allowedAPIKeySelection, setAllowedAPIKeySelection] = useState<
    number[]
  >([]);
  const [editProxyUrl, setEditProxyUrl] = useState("");
  const [editOpenAIForm, setEditOpenAIForm] =
    useState<UpdateOpenAIResponsesAccountRequest>({
      name: "",
      base_url: "https://api.openai.com",
      api_key: "",
      models: [],
      proxy_url: "",
    });
  const [openAIModelDraft, setOpenAIModelDraft] = useState("");
  const [editOpenAIModelDraft, setEditOpenAIModelDraft] = useState("");
  const [editOpenAIModelsLoading, setEditOpenAIModelsLoading] = useState(false);
  const [importing, setImporting] = useState(false);
  const [showImportPicker, setShowImportPicker] = useState(false);
  const [dragging, setDragging] = useState(false);
  const dragCounter = useRef(0);
  const [showExportPicker, setShowExportPicker] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [showMigrate, setShowMigrate] = useState(false);
  const [showAnalysisCharts, setShowAnalysisCharts] = useState(
    getInitialAnalysisVisibility,
  );
  const [migrateUrl, setMigrateUrl] = useState("");
  const [migrateKey, setMigrateKey] = useState("");
  const [migrating, setMigrating] = useState(false);
  const [importProgress, setImportProgress] = useState<{
    show: boolean;
    current: number;
    total: number;
    success: number;
    duplicate: number;
    failed: number;
    done: boolean;
  }>({
    show: false,
    current: 0,
    total: 0,
    success: 0,
    duplicate: 0,
    failed: 0,
    done: false,
  });
  const [addMethod, setAddMethod] = useState<"rt" | "at" | "openai" | "oauth">(
    "rt",
  );
  const [atForm, setAtForm] = useState<AddATAccountRequest>({
    access_token: "",
    proxy_url: "",
  });
  const [openAIForm, setOpenAIForm] =
    useState<AddOpenAIResponsesAccountRequest>({
      base_url: "https://api.openai.com",
      api_key: "",
      models: [],
      proxy_url: "",
    });
  const [openAIModelsLoading, setOpenAIModelsLoading] = useState(false);
  const [oauthStep, setOauthStep] = useState<"generate" | "exchange">(
    "generate",
  );
  const [oauthSession, setOauthSession] = useState<{
    session_id: string;
    auth_url: string;
  } | null>(null);
  const [oauthProxyUrl, setOauthProxyUrl] = useState("");
  const [oauthCallbackUrl, setOauthCallbackUrl] = useState("");
  const [oauthName, setOauthName] = useState("");
  const [oauthGenerating, setOauthGenerating] = useState(false);
  const [oauthCompleting, setOauthCompleting] = useState(false);
  const [editTags, setEditTags] = useState<string[]>([]);
  const [editGroupIds, setEditGroupIds] = useState<number[]>([]);
  const [tagFilter, setTagFilter] = useState<string>("");
  const [groupFilter, setGroupFilter] = useState<number | null>(null);
  const [allGroups, setAllGroups] = useState<AccountGroup[]>([]);
  const [showGroupManager, setShowGroupManager] = useState(false);
  const [groupDraft, setGroupDraft] = useState<AccountGroupDraft>({
    id: null,
    name: "",
    description: "",
    color: ACCOUNT_GROUP_COLORS[0],
  });
  const [groupSubmitting, setGroupSubmitting] = useState(false);
  const [showBatchMetaEditor, setShowBatchMetaEditor] = useState(false);
  const [batchTags, setBatchTags] = useState<string[]>([]);
  const [batchGroupIds, setBatchGroupIds] = useState<number[]>([]);
  const [batchMetaSubmitting, setBatchMetaSubmitting] = useState(false);
  const [visibleColumns, setVisibleColumns] = useState<
    Record<AccountTableColumn, boolean>
  >(getInitialAccountVisibleColumns);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const jsonInputRef = useRef<HTMLInputElement>(null);
  const atFileInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);
  const selectAllRef = useRef<HTMLInputElement>(null);
  const { toast, showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const loadAccounts = useCallback(async () => {
    const [
      accountsResponse,
      apiKeysResponse,
      opsOverview,
      groupsResponse,
      settings,
    ] =
      await Promise.all([
        api.getAccounts(),
        api.getAPIKeys(),
        api.getOpsOverview().catch((): OpsOverviewResponse | null => null),
        api.listAccountGroups().catch(() => ({ groups: [] })),
        api.getSettings().catch((): SystemSettings | null => null),
      ]);
    setAllGroups(groupsResponse.groups ?? []);
    return {
      accounts: accountsResponse.accounts ?? [],
      apiKeys: apiKeysResponse.keys ?? [],
      opsOverview,
      lazyMode: settings?.lazy_mode ?? false,
    };
  }, []);

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    accounts: AccountRow[];
    apiKeys: APIKeyRow[];
    opsOverview: OpsOverviewResponse | null;
    lazyMode: boolean;
  }>({
    initialData: {
      accounts: [],
      apiKeys: [],
      opsOverview: null,
      lazyMode: false,
    },
    load: loadAccounts,
  });
  const accounts = data.accounts;
  const apiKeys = data.apiKeys;
  const opsOverview = data.opsOverview;
  const lazyMode = data.lazyMode;
  const usageReloadAttemptsRef = useRef<Map<number, number>>(new Map());

  useEffect(() => {
    persistAnalysisVisibility(showAnalysisCharts);
  }, [showAnalysisCharts]);

  useEffect(() => {
    persistAccountVisibleColumns(visibleColumns);
  }, [visibleColumns]);

  useEffect(() => {
    if (groupFilter === null) return;
    if (!allGroups.some((group) => group.id === groupFilter)) {
      setGroupFilter(null);
    }
  }, [allGroups, groupFilter]);

  useEffect(() => {
    const needsUsageReload = (account: AccountRow) => {
      if (account.status !== "active" && account.status !== "ready") {
        return false;
      }

      const plan = normalizePlanType(account.plan_type);
      const has7d =
        account.usage_percent_7d !== null &&
        account.usage_percent_7d !== undefined;
      const has5h =
        account.usage_percent_5h !== null &&
        account.usage_percent_5h !== undefined;

      if (plan === "free") {
        return !has7d;
      }
      if (
        plan === "pro" ||
        plan === "team" ||
        plan === "plus" ||
        plan === "teamplus"
      ) {
        return !has5h || !has7d;
      }
      return !has7d;
    };

    const missingUsageIds = accounts
      .filter(needsUsageReload)
      .map((account) => account.id);
    const missingUsageIdSet = new Set(missingUsageIds);
    for (const id of Array.from(usageReloadAttemptsRef.current.keys())) {
      if (!missingUsageIdSet.has(id)) {
        usageReloadAttemptsRef.current.delete(id);
      }
    }

    const retryIds = missingUsageIds.filter(
      (id) => (usageReloadAttemptsRef.current.get(id) ?? 0) < 6,
    );
    if (retryIds.length === 0) {
      return;
    }

    for (const id of retryIds) {
      usageReloadAttemptsRef.current.set(
        id,
        (usageReloadAttemptsRef.current.get(id) ?? 0) + 1,
      );
    }

    const timer = window.setTimeout(() => {
      void reloadSilently();
    }, 2500);

    return () => window.clearTimeout(timer);
  }, [accounts, reloadSilently]);

  const accountSummary = useMemo(() => {
    const rateLimitedWindowStats = getRateLimitedWindowStats(accounts);
    // 互斥分类:每个账号只属于 异常 / 限流 / 正常 之一,三者相加等于总数。
    // 优先级:异常(封禁/错误/禁用) > 限流 > 正常。locked 不影响分类,视为正常状态的一种子标记。
    const bannedAccounts = accounts.filter(
      (account) => account.status === "unauthorized",
    ).length;
    const errorAccounts = accounts.filter(
      (account) => account.status === "error",
    ).length;
    const disabledAccounts = accounts.filter(
      (account) => account.enabled === false,
    ).length;
    const abnormalAccounts = accounts.filter(
      (account) =>
        account.status === "unauthorized" ||
        account.status === "error" ||
        account.enabled === false,
    ).length;
    const rateLimitedExclusive = accounts.filter(
      (account) =>
        account.status !== "unauthorized" &&
        account.status !== "error" &&
        account.enabled !== false &&
        isRateLimitedAccount(account),
    ).length;
    const normalAccounts = accounts.length - abnormalAccounts - rateLimitedExclusive;
    return {
      totalAccounts: accounts.length,
      normalAccounts,
      rateLimitedAccounts: rateLimitedExclusive,
      rateLimited5hAccounts: rateLimitedWindowStats.fiveHour,
      rateLimited7dAccounts: rateLimitedWindowStats.sevenDay,
      abnormalAccounts,
      bannedAccounts,
      errorAccounts,
      disabledAccounts,
      lockedAccounts: accounts.filter((account) => account.locked).length,
      subscriptionAccountsToLock: accounts.filter(
        (account) => isSubscriptionPlan(account.plan_type) && !account.locked,
      ),
      healthyAccounts: accounts.filter(
        (account) => account.health_tier === "healthy",
      ).length,
      warmAccounts: accounts.filter((account) => account.health_tier === "warm")
        .length,
      riskyAccounts: accounts.filter(
        (account) => account.health_tier === "risky",
      ).length,
    };
  }, [accounts]);
  const {
    totalAccounts,
    normalAccounts,
    rateLimitedAccounts,
    rateLimited5hAccounts,
    rateLimited7dAccounts,
    abnormalAccounts,
    bannedAccounts,
    errorAccounts,
    disabledAccounts,
    lockedAccounts,
    subscriptionAccountsToLock,
    healthyAccounts,
    warmAccounts,
    riskyAccounts,
  } = accountSummary;

  const allTags = useMemo(() => {
    const tags = new Set<string>();
    for (const account of accounts) {
      for (const tag of account.tags ?? []) {
        tags.add(tag);
      }
    }
    return Array.from(tags).sort();
  }, [accounts]);

  const filteredAccounts = useMemo(() => {
    const query = searchQuery.toLowerCase();
    return accounts.filter((account) => {
      switch (statusFilter) {
        case "normal":
          if (
            account.status === "unauthorized" ||
            account.status === "error" ||
            account.enabled === false ||
            isRateLimitedAccount(account)
          )
            return false;
          if (account.status !== "active" && account.status !== "ready")
            return false;
          break;
        case "rate_limited":
          if (
            account.status === "unauthorized" ||
            account.status === "error" ||
            account.enabled === false
          )
            return false;
          if (!isRateLimitedAccount(account)) return false;
          break;
        case "abnormal":
          if (
            account.status !== "unauthorized" &&
            account.status !== "error" &&
            account.enabled !== false
          )
            return false;
          break;
        case "banned":
          if (account.status !== "unauthorized") return false;
          break;
        case "error":
          if (account.status !== "error") return false;
          break;
        case "disabled":
          if (account.enabled !== false) return false;
          break;
        case "locked":
          if (!account.locked) return false;
          break;
      }
      if (planFilter !== "all") {
        const plan = (account.plan_type || "").toLowerCase().trim();
        if (plan !== planFilter) return false;
      }
      if (query) {
        const email = (account.email || "").toLowerCase();
        const name = (account.name || "").toLowerCase();
        if (!email.includes(query) && !name.includes(query)) return false;
      }
      if (tagFilter && !(account.tags ?? []).includes(tagFilter)) return false;
      if (
        groupFilter !== null &&
        !(account.group_ids ?? []).includes(groupFilter)
      )
        return false;
      return true;
    });
  }, [accounts, groupFilter, planFilter, searchQuery, statusFilter, tagFilter]);

  const sortedAccounts = useMemo(() => {
    if (!sortKey) return filteredAccounts;
    return [...filteredAccounts].sort((a, b) => {
      let diff = 0;
      if (sortKey === "requests") {
        diff =
          (a.success_requests ?? 0) +
          (a.error_requests ?? 0) -
          ((b.success_requests ?? 0) + (b.error_requests ?? 0));
      } else if (sortKey === "usage") {
        diff = (a.usage_percent_7d ?? -1) - (b.usage_percent_7d ?? -1);
      } else if (sortKey === "importTime") {
        diff =
          new Date(a.created_at || 0).getTime() -
          new Date(b.created_at || 0).getTime();
      }
      return sortDir === "asc" ? diff : -diff;
    });
  }, [filteredAccounts, sortDir, sortKey]);

  const totalPages = Math.max(1, Math.ceil(sortedAccounts.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pagedAccounts = useMemo(
    () =>
      sortedAccounts.slice(
        (currentPage - 1) * pageSize,
        currentPage * pageSize,
      ),
    [currentPage, pageSize, sortedAccounts],
  );
  const pagedAccountIds = useMemo(
    () => pagedAccounts.map((account) => account.id),
    [pagedAccounts],
  );
  const pageSelectedCount = useMemo(
    () =>
      pagedAccountIds.reduce(
        (count, id) => count + (selected.has(id) ? 1 : 0),
        0,
      ),
    [pagedAccountIds, selected],
  );
  const allPageSelected =
    pagedAccountIds.length > 0 && pageSelectedCount === pagedAccountIds.length;
  const somePageSelected = pageSelectedCount > 0 && !allPageSelected;

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);

  useEffect(() => {
    if (!accounts.some((account) => account.status === "refreshing")) {
      return;
    }

    const timer = window.setTimeout(() => {
      void reloadSilently();
    }, 2000);

    return () => window.clearTimeout(timer);
  }, [accounts, reloadSilently]);

  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = somePageSelected;
    }
  }, [somePageSelected]);

  const toggleSelect = useCallback((id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleSelectAll = useCallback(() => {
    if (allPageSelected) {
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of pagedAccountIds) next.delete(id);
        return next;
      });
    } else {
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of pagedAccountIds) next.add(id);
        return next;
      });
    }
  }, [allPageSelected, pagedAccountIds]);

  const handleAdd = async () => {
    if (!addForm.refresh_token.trim()) return;
    setSubmitting(true);
    try {
      await api.addAccount(addForm);
      showToast(t("accounts.addSuccess"));
      setShowAdd(false);
      setAddForm({ refresh_token: "", proxy_url: "" });
      void reload();
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const handleAddAT = async () => {
    if (!atForm.access_token.trim()) return;
    setSubmitting(true);
    try {
      await api.addATAccount(atForm);
      showToast(t("accounts.addSuccess"));
      setShowAdd(false);
      setAtForm({ access_token: "", proxy_url: "" });
      void reload();
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const addOpenAIModelValues = useCallback((raw: string) => {
    const nextModels = parseModelTokens(raw);
    if (nextModels.length === 0) return;
    setOpenAIForm((form) => ({
      ...form,
      models: mergeModelLists(form.models, nextModels),
    }));
    setOpenAIModelDraft("");
  }, []);

  const removeOpenAIModel = useCallback((model: string) => {
    setOpenAIForm((form) => ({
      ...form,
      models: form.models.filter((item) => item !== model),
    }));
  }, []);

  const addEditOpenAIModelValues = useCallback((raw: string) => {
    const nextModels = parseModelTokens(raw);
    if (nextModels.length === 0) return;
    setEditOpenAIForm((form) => ({
      ...form,
      models: mergeModelLists(form.models, nextModels),
    }));
    setEditOpenAIModelDraft("");
  }, []);

  const removeEditOpenAIModel = useCallback((model: string) => {
    setEditOpenAIForm((form) => ({
      ...form,
      models: form.models.filter((item) => item !== model),
    }));
  }, []);

  const handleFetchOpenAIModels = async () => {
    if (!openAIForm.api_key.trim()) return;
    setOpenAIModelsLoading(true);
    try {
      const result = await api.fetchOpenAIResponsesModels({
        base_url: openAIForm.base_url,
        api_key: openAIForm.api_key,
        proxy_url: openAIForm.proxy_url,
      });
      const models = result.models ?? [];
      setOpenAIForm((form) => ({
        ...form,
        base_url: result.base_url || form.base_url,
        models,
      }));
      showToast(
        t("accounts.openaiModelsFetchSuccess", { count: models.length }),
      );
    } catch (error) {
      showToast(
        t("accounts.openaiModelsFetchFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setOpenAIModelsLoading(false);
    }
  };

  const handleAddOpenAIResponses = async () => {
    const models = openAIForm.models;
    if (!openAIForm.api_key.trim() || models.length === 0) return;
    setSubmitting(true);
    try {
      await api.addOpenAIResponsesAccount({ ...openAIForm, models });
      showToast(t("accounts.addSuccess"));
      setShowAdd(false);
      setOpenAIForm({
        base_url: "https://api.openai.com",
        api_key: "",
        models: [],
        proxy_url: "",
      });
      setOpenAIModelDraft("");
      void reload();
    } catch (error) {
      showToast(
        t("accounts.addFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const handleFetchEditOpenAIModels = async () => {
    if (!editingAccount?.openai_responses_api) return;
    setEditOpenAIModelsLoading(true);
    try {
      const result = await api.fetchOpenAIResponsesModels({
        account_id: editingAccount.id,
        base_url: editOpenAIForm.base_url,
        api_key: editOpenAIForm.api_key ?? "",
        proxy_url: editOpenAIForm.proxy_url,
      });
      const models = result.models ?? [];
      setEditOpenAIForm((form) => ({
        ...form,
        base_url: result.base_url || form.base_url,
        models,
      }));
      showToast(
        t("accounts.openaiModelsFetchSuccess", { count: models.length }),
      );
    } catch (error) {
      showToast(
        t("accounts.openaiModelsFetchFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setEditOpenAIModelsLoading(false);
    }
  };

  const handleSaveOpenAIAccountSettings = async () => {
    if (!editingAccount?.openai_responses_api) return;
    if (!editOpenAIForm.base_url.trim() || editOpenAIForm.models.length === 0) {
      showToast(t("accounts.openaiAccountInvalid"), "error");
      return;
    }
    setEditSubmitting(true);
    try {
      await api.updateOpenAIResponsesAccount(editingAccount.id, {
        ...editOpenAIForm,
        api_key: editOpenAIForm.api_key?.trim() || undefined,
      });
      showToast(t("accounts.openaiAccountSaveSuccess"));
      await reload();
      closeSchedulerEditor(true);
    } catch (error) {
      showToast(
        t("accounts.openaiAccountSaveFailed", {
          error: getErrorMessage(error),
        }),
        "error",
      );
    } finally {
      setEditSubmitting(false);
    }
  };

  const handleOAuthGenerate = async () => {
    setOauthGenerating(true);
    try {
      const result = await api.generateOAuthURL({ proxy_url: oauthProxyUrl });
      setOauthSession(result);
      setOauthStep("exchange");
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setOauthGenerating(false);
    }
  };

  const handleOAuthComplete = async () => {
    if (!oauthSession) return;
    let code = "";
    let state = "";
    const raw = oauthCallbackUrl.trim();
    try {
      const url = new URL(raw);
      code = url.searchParams.get("code") ?? "";
      state = url.searchParams.get("state") ?? "";
    } catch {
      const qs = raw.includes("?") ? raw.split("?")[1] : raw;
      const params = new URLSearchParams(qs);
      code = params.get("code") ?? "";
      state = params.get("state") ?? "";
    }
    if (!code || !state) {
      showToast(t("accounts.oauthParseError"), "error");
      return;
    }
    setOauthCompleting(true);
    try {
      const result = await api.exchangeOAuthCode({
        session_id: oauthSession.session_id,
        code,
        state,
        name: oauthName.trim() || undefined,
        proxy_url: oauthProxyUrl.trim() || undefined,
      });
      showToast(
        result.email
          ? t("accounts.oauthSuccess", { email: result.email })
          : t("accounts.oauthSuccessNoEmail"),
      );
      setShowAdd(false);
      setAddMethod("rt");
      setOauthStep("generate");
      setOauthSession(null);
      setOauthCallbackUrl("");
      setOauthName("");
      void reload();
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setOauthCompleting(false);
    }
  };

  const readImportSSE = async (res: Response) => {
    setImportProgress({
      show: true,
      current: 0,
      total: 0,
      success: 0,
      duplicate: 0,
      failed: 0,
      done: false,
    });
    const reader = res.body?.getReader();
    if (!reader) return;
    const decoder = new TextDecoder();
    let buffer = "";
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.startsWith("data: ")) continue;
        try {
          const event = JSON.parse(line.slice(6)) as {
            type: string;
            current: number;
            total: number;
            success: number;
            duplicate: number;
            failed: number;
          };
          setImportProgress((p) => ({
            ...p,
            current: event.current,
            total: event.total,
            success: event.success,
            duplicate: event.duplicate,
            failed: event.failed,
            done: event.type === "complete",
          }));
          if (event.type === "complete") void reload();
        } catch {
          /* 忽略解析异常 */
        }
      }
    }
  };

  const importFiles = async (
    files: File[],
    format: "txt" | "json" | "at_txt",
  ) => {
    setImporting(true);
    try {
      const formData = new FormData();
      if (format !== "txt") formData.append("format", format);
      for (const f of files) formData.append("file", f);
      const res = await fetch("/api/admin/accounts/import", {
        method: "POST",
        body: formData,
        headers: getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {},
      });
      if (res.headers.get("content-type")?.includes("text/event-stream")) {
        await readImportSSE(res);
      } else {
        const data = await res.json();
        if (!res.ok) {
          showToast(
            data.error
              ? t("accounts.importFailedWithReason", { error: data.error })
              : t("accounts.importFailed"),
            "error",
          );
        } else {
          showToast(t("accounts.importCompleted"));
          void reload();
        }
      }
    } catch (error) {
      showToast(
        t("accounts.importFailedWithReason", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setImporting(false);
    }
  };

  const handleDragEnter = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounter.current++;
    if (dragCounter.current === 1) setDragging(true);
  };

  const handleDragOver = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
  };

  const handleDragLeave = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounter.current--;
    if (dragCounter.current === 0) setDragging(false);
  };

  const readAllEntriesFromDirectory = (
    dirEntry: FileSystemDirectoryEntry,
  ): Promise<File[]> => {
    return new Promise((resolve) => {
      const files: File[] = [];
      const readEntries = (reader: FileSystemDirectoryReader) => {
        reader.readEntries(async (entries) => {
          if (entries.length === 0) {
            resolve(files);
            return;
          }
          for (const entry of entries) {
            if (entry.isFile) {
              const file = await new Promise<File>((res) =>
                (entry as FileSystemFileEntry).file(res),
              );
              files.push(file);
            } else if (entry.isDirectory) {
              const subFiles = await readAllEntriesFromDirectory(
                entry as FileSystemDirectoryEntry,
              );
              files.push(...subFiles);
            }
          }
          readEntries(reader);
        });
      };
      readEntries(dirEntry.createReader());
    });
  };

  const handleDrop = async (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    dragCounter.current = 0;
    setDragging(false);
    if (importing) return;

    // 检测是否拖入了文件夹
    const items = e.dataTransfer.items;
    const hasDirectories =
      items &&
      Array.from(items).some((item) => item.webkitGetAsEntry?.()?.isDirectory);

    if (hasDirectories) {
      const allFiles: File[] = [];
      for (const item of Array.from(items)) {
        const entry = item.webkitGetAsEntry?.();
        if (!entry) continue;
        if (entry.isDirectory) {
          const dirFiles = await readAllEntriesFromDirectory(
            entry as FileSystemDirectoryEntry,
          );
          allFiles.push(...dirFiles);
        } else if (entry.isFile) {
          const file = await new Promise<File>((res) =>
            (entry as FileSystemFileEntry).file(res),
          );
          allFiles.push(file);
        }
      }

      const validFiles = allFiles.filter((f) => {
        const ext = f.name.split(".").pop()?.toLowerCase();
        return (ext === "txt" || ext === "json") && f.size > 0;
      });

      if (validFiles.length === 0) {
        showToast(t("accounts.folderNoValidFiles"), "error");
        return;
      }

      const txtFiles = validFiles.filter(
        (f) => f.name.split(".").pop()?.toLowerCase() === "txt",
      );
      const jsonFiles = validFiles.filter(
        (f) => f.name.split(".").pop()?.toLowerCase() === "json",
      );

      if (jsonFiles.length > 0) {
        await importFiles(jsonFiles, "json");
      }
      if (txtFiles.length > 0) {
        await importFiles(txtFiles, "txt");
      }
      return;
    }

    // 原有的文件拖放逻辑
    const files = Array.from(e.dataTransfer.files).filter((f) => f.size > 0);
    if (files.length === 0) return;

    const txtFiles: File[] = [];
    const jsonFiles: File[] = [];
    for (const f of files) {
      const ext = f.name.split(".").pop()?.toLowerCase();
      if (ext === "txt") txtFiles.push(f);
      else if (ext === "json") jsonFiles.push(f);
      else {
        showToast(t("accounts.unsupportedFileType", { name: f.name }), "error");
        return;
      }
    }

    if (jsonFiles.length > 0) {
      await importFiles(jsonFiles, "json");
    }
    if (txtFiles.length > 0) {
      await importFiles(txtFiles, "txt");
    }
  };

  const handleFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    if (files.length === 0) return;
    if (files.some((file) => !file.name.toLowerCase().endsWith(".txt"))) {
      showToast(t("accounts.selectTxtFile"), "error");
      return;
    }
    setShowImportPicker(false);
    await importFiles(files, "txt");
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const handleJsonImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files;
    if (!files || files.length === 0) return;
    setShowImportPicker(false);
    await importFiles(Array.from(files), "json");
    if (jsonInputRef.current) jsonInputRef.current.value = "";
  };

  const handleAtFileImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    if (files.length === 0) return;
    if (files.some((file) => !file.name.toLowerCase().endsWith(".txt"))) {
      showToast(t("accounts.selectTxtFile"), "error");
      return;
    }
    setShowImportPicker(false);
    await importFiles(files, "at_txt");
    if (atFileInputRef.current) atFileInputRef.current.value = "";
  };

  const handleFolderImport = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = event.target.files;
    if (!files || files.length === 0) return;
    setShowImportPicker(false);

    const validFiles = Array.from(files).filter((f) => {
      const ext = f.name.split(".").pop()?.toLowerCase();
      return (ext === "txt" || ext === "json") && f.size > 0;
    });

    if (validFiles.length === 0) {
      showToast(t("accounts.folderNoValidFiles"), "error");
      if (folderInputRef.current) folderInputRef.current.value = "";
      return;
    }

    const txtFiles = validFiles.filter(
      (f) => f.name.split(".").pop()?.toLowerCase() === "txt",
    );
    const jsonFiles = validFiles.filter(
      (f) => f.name.split(".").pop()?.toLowerCase() === "json",
    );

    if (jsonFiles.length > 0) {
      await importFiles(jsonFiles, "json");
    }
    if (txtFiles.length > 0) {
      await importFiles(txtFiles, "txt");
    }

    if (folderInputRef.current) folderInputRef.current.value = "";
  };

  const handleExport = async (
    format: "json" | "txt",
    scope: "healthy" | "selected",
  ) => {
    setExporting(true);
    setShowExportPicker(false);
    try {
      const params: { filter: "healthy" | "all"; ids?: number[] } = {
        filter: scope === "healthy" ? "healthy" : "all",
      };
      if (scope === "selected") {
        params.ids = Array.from(selected);
        params.filter = "all";
      }
      const data = await api.exportAccounts(params);
      if (data.length === 0) {
        showToast(t("accounts.exportNoAccounts"), "error");
        return;
      }
      const ts = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
      if (format === "json") {
        const blob = new Blob([JSON.stringify(data, null, 2)], {
          type: "application/json",
        });
        downloadBlob(blob, `cpa-${ts}-${data.length}.json`);
      } else {
        const text = data.map((e) => e.refresh_token).join("\n");
        const blob = new Blob([text], { type: "text/plain" });
        downloadBlob(blob, `rt-${ts}-${data.length}.txt`);
      }
      showToast(t("accounts.exportSuccess", { count: data.length }));
    } catch (error) {
      showToast(
        `${t("accounts.exportFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setExporting(false);
    }
  };

  const handleGenerateAuthJSON = async (account: AccountRow) => {
    setAuthJsonExportingIds((prev) => new Set(prev).add(account.id));
    try {
      const blob = await api.downloadAccountAuthJSON(account.id);
      const json = formatJSONText(await blob.text());
      setAuthJsonModal({ account, json });
      showToast(t("accounts.authJsonGenerated"));
    } catch (error) {
      showToast(
        t("accounts.authJsonFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setAuthJsonExportingIds((prev) => {
        const next = new Set(prev);
        next.delete(account.id);
        return next;
      });
    }
  };

  const handleCopyAuthJSON = async () => {
    if (!authJsonModal) return;
    try {
      await copyTextToClipboard(authJsonModal.json);
      showToast(t("accounts.authJsonCopied"));
    } catch (error) {
      showToast(
        t("accounts.authJsonCopyFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleExportAuthJSON = () => {
    if (!authJsonModal) return;
    const blob = new Blob([`${authJsonModal.json}\n`], {
      type: "application/json",
    });
    downloadBlob(blob, "auth.json");
    showToast(t("accounts.authJsonExported"));
  };

  const handleMigrate = async () => {
    setMigrating(true);
    setShowMigrate(false);
    try {
      const res = await fetch("/api/admin/accounts/migrate", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...(getAdminKey() ? { "X-Admin-Key": getAdminKey() } : {}),
        },
        body: JSON.stringify({
          url: migrateUrl.trim(),
          admin_key: migrateKey.trim(),
        }),
      });
      if (res.headers.get("content-type")?.includes("text/event-stream")) {
        await readImportSSE(res);
      } else {
        const data = await res.json();
        if (!res.ok) {
          showToast(
            data.error
              ? `${t("accounts.migrateFailed")}: ${data.error}`
              : t("accounts.migrateFailed"),
            "error",
          );
        } else {
          showToast(
            t("accounts.migrateSuccess", {
              imported: data.imported ?? 0,
              duplicate: data.duplicate ?? 0,
              failed: data.failed ?? 0,
            }),
          );
          void reload();
        }
      }
    } catch (error) {
      showToast(
        `${t("accounts.migrateFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setMigrating(false);
      setMigrateUrl("");
      setMigrateKey("");
    }
  };

  const handleDelete = async (account: AccountRow) => {
    const confirmed = await confirm({
      title: t("accounts.deleteTitle"),
      description: t("accounts.deleteDesc", {
        account: account.email || `ID ${account.id}`,
      }),
      confirmText: t("accounts.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    try {
      await api.deleteAccount(account.id);
      showToast(t("accounts.deleted"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.deleteFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleRefresh = async (account: AccountRow) => {
    setRefreshingIds((prev) => new Set(prev).add(account.id));
    try {
      const result = await api.refreshAccount(account.id);
      showToast(result.message || t("accounts.refreshRequested"));
      void reloadSilently();
    } catch (error) {
      showToast(
        t("accounts.refreshFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setRefreshingIds((prev) => {
        const next = new Set(prev);
        next.delete(account.id);
        return next;
      });
    }
  };

  const handleToggleLock = async (account: AccountRow) => {
    const newLocked = !account.locked;
    try {
      await api.toggleAccountLock(account.id, newLocked);
      showToast(
        newLocked ? t("accounts.lockSuccess") : t("accounts.unlockSuccess"),
      );
      void reload();
    } catch (error) {
      showToast(
        t("accounts.lockFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleLockSubscriptionAccounts = async () => {
    const candidates = subscriptionAccountsToLock;
    if (candidates.length === 0) {
      showToast(t("accounts.noSubscriptionAccountsToLock"));
      return;
    }

    setBatchLoading(true);
    setLockingSubscriptionAccounts(true);
    try {
      const { success, fail } = await runAccountBatch(
        candidates.map((account) => account.id),
        (id) => api.toggleAccountLock(id, true),
      );
      showToast(t("accounts.lockSubscriptionAccountsDone", { success, fail }));
      void reload();
    } finally {
      setBatchLoading(false);
      setLockingSubscriptionAccounts(false);
    }
  };

  const handleToggleEnabled = async (account: AccountRow) => {
    const nextEnabled = account.enabled === false;
    try {
      await api.toggleAccountEnabled(account.id, nextEnabled);
      showToast(
        nextEnabled
          ? t("accounts.enableSuccess")
          : t("accounts.disableSuccess"),
      );
      void reload();
    } catch (error) {
      showToast(
        t("accounts.enableFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleBatchDelete = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    const confirmed = await confirm({
      title: t("accounts.batchDeleteTitle"),
      description: t("accounts.batchDeleteDesc", { count: ids.length }),
      confirmText: t("accounts.deleteConfirm"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setBatchLoading(true);
    try {
      const { success, fail } = await runAccountBatch(ids, api.deleteAccount);
      showToast(t("accounts.batchDeleteDone", { success, fail }));
      setSelected(new Set());
      void reload();
    } finally {
      setBatchLoading(false);
    }
  };

  const handleBatchRefresh = async (ids?: number[]) => {
    const targetIds = ids ?? Array.from(selected);
    if (targetIds.length === 0) return;
    setBatchLoading(true);
    setBatchRefreshing(true);
    try {
      const { success, fail } = await runAccountBatch(
        targetIds,
        api.refreshAccount,
        ACCOUNT_REFRESH_BATCH_CONCURRENCY,
      );
      showToast(t("accounts.batchRefreshDone", { success, fail }));
      void reload();
    } finally {
      setBatchLoading(false);
      setBatchRefreshing(false);
    }
  };

  const handleBatchLock = async (locked: boolean) => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchLoading(true);
    try {
      const { success, fail } = await runAccountBatch(ids, (id) =>
        api.toggleAccountLock(id, locked),
      );
      showToast(
        t(locked ? "accounts.batchLockDone" : "accounts.batchUnlockDone", {
          success,
          fail,
        }),
      );
      setSelected(new Set());
      void reload();
    } finally {
      setBatchLoading(false);
    }
  };

  const handleBatchEnabled = async (enabled: boolean) => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchLoading(true);
    try {
      const { success, fail } = await runAccountBatch(ids, (id) =>
        api.toggleAccountEnabled(id, enabled),
      );
      showToast(
        t(enabled ? "accounts.batchEnableDone" : "accounts.batchDisableDone", {
          success,
          fail,
        }),
      );
      setSelected(new Set());
      void reload();
    } finally {
      setBatchLoading(false);
    }
  };

  const handleResetStatus = async (account: AccountRow) => {
    try {
      await api.resetAccountStatus(account.id);
      showToast(t("accounts.resetStatusSuccess"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.resetStatusFailed", { error: getErrorMessage(error) }),
        "error",
      );
    }
  };

  const handleBatchResetStatus = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchLoading(true);
    try {
      const result = await api.batchResetStatus(ids);
      showToast(
        t("accounts.batchResetStatusDone", {
          success: result.success,
          fail: result.failed,
        }),
      );
      setSelected(new Set());
      void reload();
    } catch (error) {
      showToast(
        t("accounts.resetStatusFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchLoading(false);
    }
  };

  const openBatchMetaEditor = () => {
    const selectedAccounts = accounts.filter((account) =>
      selected.has(account.id),
    );
    const tagSet = new Set<string>();
    const groupSet = new Set<number>();
    for (const account of selectedAccounts) {
      for (const tag of account.tags ?? []) tagSet.add(tag);
      for (const id of account.group_ids ?? []) groupSet.add(id);
    }
    setBatchTags(Array.from(tagSet).sort());
    setBatchGroupIds(Array.from(groupSet).sort((a, b) => a - b));
    setShowBatchMetaEditor(true);
  };

  const handleBatchSaveMeta = async () => {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBatchMetaSubmitting(true);
    try {
      const { success, fail } = await runAccountBatch(ids, (id) =>
        api.updateAccountScheduler(id, {
          tags: batchTags,
          group_ids: batchGroupIds,
        }),
      );
      showToast(t("accounts.batchMetaDone", { success, fail }));
      setShowBatchMetaEditor(false);
      await Promise.all([reload(), reloadGroups()]);
    } catch (error) {
      showToast(
        t("accounts.batchMetaFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchMetaSubmitting(false);
    }
  };

  const handleBatchTest = async (ids?: number[]) => {
    if (ids && ids.length === 0) return;
    setBatchTesting(true);
    try {
      const result = await api.batchTestAccounts(ids);
      showToast(
        t("accounts.batchTestDone", {
          success: result.success,
          banned: result.banned,
          rateLimited: result.rate_limited,
          failed: result.failed,
        }),
      );
      void reload();
    } catch (error) {
      showToast(
        t("accounts.batchTestFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setBatchTesting(false);
    }
  };

  const handleCleanBanned = async () => {
    const confirmed = await confirm({
      title: t("accounts.cleanBannedTitle"),
      description: t("accounts.cleanBannedDesc"),
      confirmText: t("accounts.cleanConfirm"),
      tone: "warning",
    });
    if (!confirmed) return;
    setCleaningBanned(true);
    try {
      await api.cleanBanned();
      showToast(t("accounts.cleanBannedSuccess"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.cleanBannedFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningBanned(false);
    }
  };

  const handleCleanRateLimited = async () => {
    const confirmed = await confirm({
      title: t("accounts.cleanRateLimitedTitle"),
      description: t("accounts.cleanRateLimitedDesc"),
      confirmText: t("accounts.cleanConfirm"),
      tone: "warning",
    });
    if (!confirmed) return;
    setCleaningRateLimited(true);
    try {
      await api.cleanRateLimited();
      showToast(t("accounts.cleanRateLimitedSuccess"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.cleanRateLimitedFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningRateLimited(false);
    }
  };

  const handleCleanError = async () => {
    const confirmed = await confirm({
      title: t("accounts.cleanErrorTitle"),
      description: t("accounts.cleanErrorDesc"),
      confirmText: t("accounts.cleanConfirm"),
      tone: "warning",
    });
    if (!confirmed) return;
    setCleaningError(true);
    try {
      await api.cleanError();
      showToast(t("accounts.cleanErrorSuccess"));
      void reload();
    } catch (error) {
      showToast(
        t("accounts.cleanErrorFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setCleaningError(false);
    }
  };

  const openSchedulerEditor = (account: AccountRow) => {
    setEditingAccount(account);
    setEditTab("scheduler");
    setScoreMode(
      account.score_bias_override === null ||
        account.score_bias_override === undefined
        ? "default"
        : "custom",
    );
    setScoreInput(
      account.score_bias_override === null ||
        account.score_bias_override === undefined
        ? ""
        : String(account.score_bias_override),
    );
    setConcurrencyMode(
      account.base_concurrency_override === null ||
        account.base_concurrency_override === undefined
        ? "default"
        : "custom",
    );
    setConcurrencyInput(
      account.base_concurrency_override === null ||
        account.base_concurrency_override === undefined
        ? ""
        : String(account.base_concurrency_override),
    );
    setAllowedAPIKeySelection(
      filterExistingAPIKeyIDs(account.allowed_api_key_ids ?? [], apiKeys),
    );
    setEditProxyUrl(account.proxy_url ?? "");
    setEditTags(account.tags ?? []);
    setEditGroupIds(account.group_ids ?? []);
    setEditOpenAIForm({
      name: account.name ?? "",
      base_url: account.base_url || "https://api.openai.com",
      api_key: "",
      models: account.models ?? [],
      proxy_url: account.proxy_url ?? "",
    });
    setEditOpenAIModelDraft("");
  };

  const closeSchedulerEditor = (force = false) => {
    if (editSubmitting && !force) return;
    setEditingAccount(null);
    setEditTab("scheduler");
    setScoreMode("default");
    setScoreInput("");
    setConcurrencyMode("default");
    setConcurrencyInput("");
    setAllowedAPIKeySelection([]);
    setEditProxyUrl("");
    setEditTags([]);
    setEditGroupIds([]);
    setEditOpenAIForm({
      name: "",
      base_url: "https://api.openai.com",
      api_key: "",
      models: [],
      proxy_url: "",
    });
    setEditOpenAIModelDraft("");
  };

  const parsedScoreBias =
    scoreMode === "custom" ? parseIntegerInput(scoreInput) : null;
  const parsedBaseConcurrency =
    concurrencyMode === "custom" ? parseIntegerInput(concurrencyInput) : null;
  const scoreInputInvalid =
    scoreMode === "custom" &&
    (parsedScoreBias === null ||
      parsedScoreBias < -200 ||
      parsedScoreBias > 200);
  const concurrencyInputInvalid =
    concurrencyMode === "custom" &&
    (parsedBaseConcurrency === null ||
      parsedBaseConcurrency < 1 ||
      parsedBaseConcurrency > 50);
  const openAIAccountInputInvalid = Boolean(
    editingAccount?.openai_responses_api &&
    editTab === "account" &&
    (!editOpenAIForm.base_url.trim() || editOpenAIForm.models.length === 0),
  );

  const editPreview = useMemo(() => {
    if (!editingAccount) return null;

    const rawScore = Math.round(editingAccount.scheduler_score ?? 0);
    const appliedBias =
      scoreMode === "custom"
        ? (parsedScoreBias ?? getEffectiveScoreBias(editingAccount))
        : getDefaultScoreBias(editingAccount.plan_type);
    const baseConcurrency =
      concurrencyMode === "custom"
        ? (parsedBaseConcurrency ?? getEffectiveBaseConcurrency(editingAccount))
        : getEffectiveBaseConcurrency(editingAccount);

    return {
      rawScore,
      dispatchScore: computePreviewDispatchScore(
        editingAccount,
        rawScore,
        appliedBias,
      ),
      healthTier: editingAccount.health_tier,
      dynamicConcurrency: computePreviewDynamicConcurrency(
        editingAccount,
        baseConcurrency,
      ),
      appliedBias,
      baseConcurrency,
    };
  }, [
    editingAccount,
    scoreMode,
    parsedScoreBias,
    concurrencyMode,
    parsedBaseConcurrency,
  ]);

  const handleSaveScheduler = async () => {
    if (!editingAccount) return;
    if (scoreInputInvalid || concurrencyInputInvalid) {
      showToast(t("accounts.schedulerInvalidInput"), "error");
      return;
    }

    setEditSubmitting(true);
    try {
      const payload = {
        score_bias_override: scoreMode === "custom" ? parsedScoreBias : null,
        base_concurrency_override:
          concurrencyMode === "custom" ? parsedBaseConcurrency : null,
        allowed_api_key_ids: allowedAPIKeySelection,
        proxy_url: editProxyUrl.trim() || null,
        tags: editTags,
        group_ids: editGroupIds,
      };
      await api.updateAccountScheduler(editingAccount.id, payload);
      showToast(t("accounts.schedulerSaveSuccess"));
      await Promise.all([reload(), reloadGroups()]);
      closeSchedulerEditor(true);
    } catch (error) {
      showToast(
        t("accounts.schedulerSaveFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditSubmitting(false);
    }
  };

  const handleSaveAccountEditor = async () => {
    if (editingAccount?.openai_responses_api && editTab === "account") {
      await handleSaveOpenAIAccountSettings();
      return;
    }
    await handleSaveScheduler();
  };

  const reloadGroups = async () => {
    const res = await api.listAccountGroups();
    setAllGroups(res.groups ?? []);
  };

  const resetGroupDraft = () => {
    setGroupDraft({
      id: null,
      name: "",
      description: "",
      color: ACCOUNT_GROUP_COLORS[0],
    });
  };

  const startEditGroup = (group: AccountGroup) => {
    setGroupDraft({
      id: group.id,
      name: group.name,
      description: group.description ?? "",
      color: group.color || ACCOUNT_GROUP_COLORS[0],
    });
  };

  const handleSaveGroup = async () => {
    const name = groupDraft.name.trim();
    if (!name) {
      showToast(t("accounts.groupNameRequired"), "error");
      return;
    }
    setGroupSubmitting(true);
    try {
      const payload = {
        name,
        description: groupDraft.description.trim(),
        color: groupDraft.color.trim() || ACCOUNT_GROUP_COLORS[0],
      };
      if (groupDraft.id === null) {
        await api.createAccountGroup(payload);
        showToast(t("accounts.groupCreated"));
      } else {
        await api.updateAccountGroup(groupDraft.id, payload);
        showToast(t("accounts.groupUpdated"));
      }
      await reloadGroups();
      resetGroupDraft();
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setGroupSubmitting(false);
    }
  };

  const handleDeleteGroup = async (group: AccountGroup) => {
    const force = group.member_count > 0;
    const confirmed = await confirm({
      title: t("accounts.groupDeleteTitle"),
      description: force
        ? t("accounts.groupDeleteWithMembers")
        : t("accounts.groupDeleteEmpty"),
      confirmText: force ? t("accounts.groupDeleteForce") : t("common.delete"),
      tone: "destructive",
      confirmVariant: "destructive",
    });
    if (!confirmed) return;
    setGroupSubmitting(true);
    try {
      await api.deleteAccountGroup(group.id, force);
      showToast(t("accounts.groupDeleted"));
      setEditGroupIds((current) => current.filter((id) => id !== group.id));
      setBatchGroupIds((current) => current.filter((id) => id !== group.id));
      if (groupFilter === group.id) setGroupFilter(null);
      if (groupDraft.id === group.id) resetGroupDraft();
      await Promise.all([reload(), reloadGroups()]);
    } catch (error) {
      showToast(getErrorMessage(error), "error");
    } finally {
      setGroupSubmitting(false);
    }
  };

  return (
    <div
      className="relative"
      onDragEnter={handleDragEnter}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={(e) => void handleDrop(e)}
    >
      {dragging && (
        <div className="pointer-events-none absolute inset-0 z-50 flex items-center justify-center rounded-lg border-2 border-dashed border-primary bg-primary/5 backdrop-blur-sm">
          <div className="flex flex-col items-center gap-2 text-primary">
            <Upload className="size-10" />
            <span className="text-lg font-semibold">
              {t("accounts.dropToImport")}
            </span>
            <span className="text-sm text-muted-foreground">
              {t("accounts.dropHint")}
            </span>
          </div>
        </div>
      )}
      <StateShell
        variant="page"
        loading={loading}
        error={error}
        onRetry={() => void reload()}
        loadingTitle={t("accounts.loadingTitle")}
        loadingDescription={t("accounts.loadingDesc")}
        errorTitle={t("accounts.errorTitle")}
      >
        <>
          <PageHeader
            title={t("accounts.title")}
            description={t("accounts.description")}
            onRefresh={() => void reload()}
            actions={
              <div className="flex flex-wrap items-center justify-end gap-1.5">
                <Button
                  variant="outline"
                  aria-pressed={showAnalysisCharts}
                  onClick={() => setShowAnalysisCharts((visible) => !visible)}
                  className="max-sm:w-full"
                >
                  <BarChart3 className="size-3.5" />
                  {showAnalysisCharts
                    ? t("accounts.hideAnalysisCharts")
                    : t("accounts.showAnalysisCharts")}
                </Button>
                <HeaderActionMenu
                  label={t("accounts.maintenanceActions")}
                  icon={<Zap className="size-3.5" />}
                  items={[
                    {
                      key: "refresh-tokens",
                      label: t("accounts.refreshTokens"),
                      icon: (
                        <RefreshCw
                          className={`size-3.5 ${batchRefreshing ? "animate-spin" : ""}`}
                        />
                      ),
                      disabled:
                        batchLoading || batchTesting || accounts.length === 0,
                      onSelect: () =>
                        void handleBatchRefresh(
                          accounts.map((account) => account.id),
                        ),
                    },
                    {
                      key: "test-connection",
                      label: batchTesting
                        ? t("accounts.batchTesting")
                        : t("accounts.testConnection"),
                      icon: <FlaskConical className="size-3.5" />,
                      disabled:
                        batchLoading || batchTesting || accounts.length === 0,
                      onSelect: () => void handleBatchTest(),
                    },
                    {
                      key: "lock-subscription",
                      label: lockingSubscriptionAccounts
                        ? t("accounts.lockingSubscriptionAccounts")
                        : t("accounts.lockSubscriptionAccounts"),
                      icon: <Lock className="size-3.5" />,
                      disabled:
                        batchLoading ||
                        batchTesting ||
                        lockingSubscriptionAccounts ||
                        accounts.length === 0,
                      title: t("accounts.lockSubscriptionAccountsHint", {
                        count: subscriptionAccountsToLock.length,
                      }),
                      onSelect: () => void handleLockSubscriptionAccounts(),
                    },
                  ]}
                />
                <HeaderActionMenu
                  label={t("accounts.cleanupActions")}
                  icon={<Trash2 className="size-3.5" />}
                  items={[
                    {
                      key: "clean-banned",
                      label: cleaningBanned
                        ? t("accounts.cleaning")
                        : t("accounts.cleanBanned"),
                      icon: <Ban className="size-3.5" />,
                      disabled: cleaningBanned,
                      onSelect: () => void handleCleanBanned(),
                    },
                    {
                      key: "clean-rate-limited",
                      label: cleaningRateLimited
                        ? t("accounts.cleaning")
                        : t("accounts.cleanRateLimited"),
                      icon: <Timer className="size-3.5" />,
                      disabled: cleaningRateLimited,
                      onSelect: () => void handleCleanRateLimited(),
                    },
                    {
                      key: "clean-error",
                      label: cleaningError
                        ? t("accounts.cleaning")
                        : t("accounts.cleanError"),
                      icon: <AlertTriangle className="size-3.5" />,
                      disabled: cleaningError,
                      onSelect: () => void handleCleanError(),
                    },
                  ]}
                />
                <HeaderActionMenu
                  label={t("accounts.dataActions")}
                  icon={<FolderOpen className="size-3.5" />}
                  items={[
                    {
                      key: "import",
                      label: importing
                        ? t("accounts.importing")
                        : t("accounts.importFile"),
                      icon: <Upload className="size-3.5" />,
                      disabled: importing,
                      onSelect: () => setShowImportPicker(true),
                    },
                    {
                      key: "export",
                      label: exporting
                        ? t("accounts.exporting")
                        : t("accounts.export"),
                      icon: <Download className="size-3.5" />,
                      disabled: exporting,
                      onSelect: () => setShowExportPicker(true),
                    },
                    {
                      key: "migrate",
                      label: migrating
                        ? t("accounts.migrating")
                        : t("accounts.migrateImport"),
                      icon: <ArrowDownToLine className="size-3.5" />,
                      disabled: migrating,
                      onSelect: () => setShowMigrate(true),
                    },
                  ]}
                />
                <Button onClick={() => setShowAdd(true)}>
                  <Plus className="size-3.5" />
                  {t("accounts.addAccount")}
                </Button>
                <input
                  ref={fileInputRef}
                  type="file"
                  accept=".txt"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleFileImport(e)}
                />
                <input
                  ref={jsonInputRef}
                  type="file"
                  accept=".json"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleJsonImport(e)}
                />
                <input
                  ref={atFileInputRef}
                  type="file"
                  accept=".txt"
                  multiple
                  className="hidden"
                  onChange={(e) => void handleAtFileImport(e)}
                />
                <input
                  ref={folderInputRef}
                  type="file"
                  className="hidden"
                  onChange={(e) => void handleFolderImport(e)}
                  {...({
                    webkitdirectory: "",
                    directory: "",
                  } as React.InputHTMLAttributes<HTMLInputElement>)}
                />
              </div>
            }
          />

          <div className="mb-4 grid grid-cols-2 gap-3 xl:grid-cols-4">
            <CompactStat
              label={t("accounts.totalAccounts")}
              chipLabel={t("accounts.filterAll")}
              value={totalAccounts}
              tone="neutral"
            />
            <CompactStat
              label={t("accounts.normalAccounts")}
              chipLabel={t("accounts.filterNormal")}
              value={normalAccounts}
              tone="success"
            />
            <CompactStat
              label={t("accounts.rateLimited")}
              chipLabel={t("accounts.filterRateLimited")}
              value={rateLimitedAccounts}
              tone="warning"
              details={[
                { label: "5h", value: rateLimited5hAccounts },
                { label: "7d", value: rateLimited7dAccounts },
              ]}
            />
            <CompactStat
              label={t("accounts.abnormalAccounts")}
              chipLabel={t("accounts.filterAbnormal")}
              value={abnormalAccounts}
              tone="danger"
              details={[
                { label: t("accounts.abnormalBannedShort"), value: bannedAccounts },
                { label: t("accounts.abnormalErrorShort"), value: errorAccounts },
                { label: t("accounts.abnormalDisabledShort"), value: disabledAccounts },
              ]}
            />
          </div>

          {showAnalysisCharts ? (
            <div className="mb-4 grid items-stretch gap-4 xl:grid-cols-2">
              <AccountQuotaDistributionChart
                accounts={accounts}
                compact
                className="min-w-0"
              />
              <AccountRateLimitRecoveryChart
                accounts={accounts}
                currentRpm={opsOverview?.traffic?.rpm}
                rpmLimit={opsOverview?.traffic?.rpm_limit}
                avgDurationMs={opsOverview?.traffic?.avg_duration_ms}
                compact
                className="min-w-0"
              />
            </div>
          ) : null}

          <div className="mb-3 grid gap-3 xl:grid-cols-[minmax(0,1fr)_max-content]">
            <div className="toolbar-surface flex items-center gap-1.5 overflow-visible max-lg:overflow-x-auto xl:flex-nowrap">
              <span className="shrink-0 whitespace-nowrap font-semibold text-foreground">
                {t("accounts.filter")}
              </span>
              {(
                [
                  ["all", t("accounts.filterAll")],
                  ["normal", t("accounts.filterNormal")],
                  ["rate_limited", t("accounts.filterRateLimited")],
                  ["abnormal", t("accounts.filterAbnormal")],
                  ["banned", t("accounts.filterBanned")],
                  ["error", t("accounts.filterError")],
                  ["disabled", t("accounts.filterDisabled")],
                  ["locked", t("accounts.filterLocked")],
                ] as const
              ).map(([key, label]) => (
                <button
                  key={key}
                  onClick={() => {
                    setStatusFilter(key);
                    setPage(1);
                  }}
                  className={`shrink-0 whitespace-nowrap rounded-md px-2.5 py-1 font-semibold transition-colors ${
                    statusFilter === key
                      ? "bg-primary text-primary-foreground"
                      : "bg-muted/50 text-muted-foreground hover:bg-muted"
                  }`}
                >
                  {label}{" "}
                  {key === "all"
                    ? totalAccounts
                    : key === "normal"
                      ? normalAccounts
                      : key === "rate_limited"
                        ? rateLimitedAccounts
                        : key === "abnormal"
                          ? abnormalAccounts
                          : key === "banned"
                            ? bannedAccounts
                            : key === "error"
                              ? errorAccounts
                              : key === "disabled"
                                ? disabledAccounts
                                : lockedAccounts}
                </button>
              ))}
            </div>

            <div className="toolbar-surface flex items-center gap-1.5 overflow-visible max-lg:overflow-x-auto xl:flex-nowrap">
              <span className="shrink-0 whitespace-nowrap font-semibold text-foreground">
                {t("accounts.schedulerView")}
              </span>
              <SchedulerChip
                label={t("accounts.healthy")}
                value={healthyAccounts}
                tone="success"
              />
              <SchedulerChip
                label={t("accounts.warm")}
                value={warmAccounts}
                tone="warning"
              />
              <SchedulerChip
                label={t("accounts.risky")}
                value={riskyAccounts}
                tone="danger"
              />
              <SchedulerChip
                label={t("status.unauthorized")}
                value={bannedAccounts}
                tone="neutral"
              />
            </div>
          </div>

          <div className="mb-4 flex items-center gap-2 overflow-visible max-lg:flex-wrap max-lg:overflow-x-auto">
            <div className="relative w-64 shrink-0 max-sm:w-full">
              <Search className="absolute left-3 top-1/2 -translate-y-1/2 size-4 text-muted-foreground pointer-events-none" />
              <Input
                className="pl-9 h-8 rounded-lg text-[13px]"
                placeholder={t("accounts.searchPlaceholder")}
                value={searchQuery}
                onChange={(e: ChangeEvent<HTMLInputElement>) => {
                  setSearchQuery(e.target.value);
                  setPage(1);
                }}
              />
            </div>
            <div className="flex shrink-0 items-center gap-1 rounded-lg border border-border bg-muted/30 p-0.5">
              {(["all", "pro", "prolite", "plus", "team", "free"] as const).map(
                (key) => (
                  <button
                    key={key}
                    onClick={() => {
                      setPlanFilter(key);
                      setPage(1);
                    }}
                    className={`whitespace-nowrap rounded-md px-2.5 py-1 text-[12px] font-medium transition-colors ${
                      planFilter === key
                        ? "bg-background shadow-sm text-foreground"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {key === "all"
                      ? t("accounts.filterAll")
                      : key === "prolite"
                        ? "ProLite"
                        : key.charAt(0).toUpperCase() + key.slice(1)}
                  </button>
                ),
              )}
            </div>
            <Select
              className="w-36 shrink-0"
              compact
              value={tagFilter || "all"}
              onValueChange={(value) => {
                setTagFilter(value === "all" ? "" : value);
                setPage(1);
              }}
              options={[
                { value: "all", label: t("accounts.tagsFilter") },
                ...allTags.map((tag) => ({ value: tag, label: tag })),
              ]}
            />
            <Select
              className="w-36 shrink-0"
              compact
              value={groupFilter === null ? "all" : String(groupFilter)}
              onValueChange={(value) => {
                setGroupFilter(value === "all" ? null : Number(value));
                setPage(1);
              }}
              options={[
                { value: "all", label: t("accounts.groupsFilter") },
                ...allGroups.map((group) => ({
                  value: String(group.id),
                  label: group.name,
                })),
              ]}
            />
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="shrink-0"
              onClick={() => setShowGroupManager(true)}
            >
              <FolderOpen className="size-3.5" />
              {t("accounts.groupManage")}
            </Button>
            <div className="ml-auto shrink-0">
              <ColumnSettingsMenu
                columns={visibleColumns}
                onToggle={(column) =>
                  setVisibleColumns((current) => ({
                    ...current,
                    [column]: !current[column],
                  }))
                }
                onReset={() =>
                  setVisibleColumns(getDefaultAccountVisibleColumns())
                }
                resetTitle={t("accounts.columnReset")}
                labels={{
                  sequence: t("accounts.sequence"),
                  email: t("accounts.email"),
                  plan: t("accounts.plan"),
                  tags: t("accounts.tagsLabel"),
                  groups: t("accounts.groupsLabel"),
                  status: t("accounts.status"),
                  requests: t("accounts.requests"),
                  usage: t("accounts.usage"),
                  billed: t("accounts.billed"),
                  importTime: t("accounts.importTime"),
                  updatedAt: t("accounts.updatedAt"),
                  actions: t("accounts.actions"),
                }}
                title={t("accounts.columnSettings")}
              />
            </div>
          </div>

          {selected.size > 0 && (
            <div className="sticky top-2 z-20 mb-4 flex items-center justify-between gap-3 rounded-lg border border-primary/20 bg-card/95 px-3 py-2.5 text-sm font-semibold text-primary shadow-lg backdrop-blur-sm max-lg:flex-col max-lg:items-stretch">
              <span>{t("common.selected", { count: selected.size })}</span>
              <div className="flex flex-wrap items-center justify-end gap-1.5 max-lg:justify-start">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchRefresh()}
                >
                  <RefreshCw
                    className={`size-3 mr-1 ${batchRefreshing ? "animate-spin" : ""}`}
                  />
                  {t("accounts.batchRefresh")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchTest(Array.from(selected))}
                >
                  <FlaskConical className="size-3 mr-1" />
                  {batchTesting
                    ? t("accounts.batchTesting")
                    : t("accounts.batchTest")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchEnabled(true)}
                >
                  <Power className="size-3 mr-1" />
                  {t("accounts.enable")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchEnabled(false)}
                >
                  <PowerOff className="size-3 mr-1" />
                  {t("accounts.disable")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchLock(true)}
                >
                  <Lock className="size-3 mr-1" />
                  {t("accounts.lock")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchLock(false)}
                >
                  <Unlock className="size-3 mr-1" />
                  {t("accounts.unlock")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={openBatchMetaEditor}
                >
                  <FolderOpen className="size-3 mr-1" />
                  {t("accounts.batchMetaEdit")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchResetStatus()}
                >
                  <RotateCcw className="size-3 mr-1" />
                  {t("accounts.batchResetStatus")}
                </Button>
                <Button
                  variant="destructive"
                  size="sm"
                  disabled={batchLoading || batchTesting}
                  onClick={() => void handleBatchDelete()}
                >
                  {t("accounts.batchDelete")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setSelected(new Set())}
                >
                  {t("accounts.cancelSelection")}
                </Button>
              </div>
            </div>
          )}

          <Card>
            <CardContent className="p-4">
              <StateShell
                variant="section"
                isEmpty={accounts.length === 0}
                emptyTitle={t("accounts.noData")}
                emptyDescription={t("accounts.noDataDesc")}
                action={
                  <Button onClick={() => setShowAdd(true)}>
                    {t("accounts.addAccount")}
                  </Button>
                }
              >
                <div className="data-table-shell">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead className="w-10">
                          <input
                            ref={selectAllRef}
                            type="checkbox"
                            className="size-4 cursor-pointer accent-primary"
                            checked={allPageSelected}
                            onChange={toggleSelectAll}
                          />
                        </TableHead>
                        {visibleColumns.sequence && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.sequence")}
                          </TableHead>
                        )}
                        {visibleColumns.email && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.email")}
                          </TableHead>
                        )}
                        {visibleColumns.tags && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.tagsLabel")}
                          </TableHead>
                        )}
                        {visibleColumns.groups && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.groupsLabel")}
                          </TableHead>
                        )}
                        {visibleColumns.plan && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.plan")}
                          </TableHead>
                        )}
                        {visibleColumns.status && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.status")}
                          </TableHead>
                        )}
                        {visibleColumns.requests && (
                          <TableHead
                            className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                            onClick={() => {
                              if (sortKey === "requests") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("requests");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.requests")}{" "}
                            {sortKey === "requests"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.usage && (
                          <TableHead
                            className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                            onClick={() => {
                              if (sortKey === "usage") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("usage");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.usage")}{" "}
                            {sortKey === "usage"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.billed && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.billed")}
                          </TableHead>
                        )}
                        {visibleColumns.importTime && (
                          <TableHead
                            className="text-[13px] font-semibold cursor-pointer select-none hover:text-primary transition-colors"
                            onClick={() => {
                              if (sortKey === "importTime") {
                                setSortDir((d) =>
                                  d === "asc" ? "desc" : "asc",
                                );
                              } else {
                                setSortKey("importTime");
                                setSortDir("desc");
                              }
                              setPage(1);
                            }}
                          >
                            {t("accounts.importTime")}{" "}
                            {sortKey === "importTime"
                              ? sortDir === "desc"
                                ? "↓"
                                : "↑"
                              : ""}
                          </TableHead>
                        )}
                        {visibleColumns.updatedAt && (
                          <TableHead className="text-[13px] font-semibold">
                            {t("accounts.updatedAt")}
                          </TableHead>
                        )}
                        {visibleColumns.actions && (
                          <TableHead className="text-[13px] font-semibold text-right">
                            {t("accounts.actions")}
                          </TableHead>
                        )}
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {pagedAccounts.map((account, index) => {
                        const isSelected = selected.has(account.id);
                        return (
                          <TableRow
                            key={account.id}
                            className={isSelected ? "bg-primary/5" : ""}
                          >
                            <TableCell>
                              <input
                                type="checkbox"
                                className="size-4 cursor-pointer accent-primary"
                                checked={isSelected}
                                onChange={() => toggleSelect(account.id)}
                              />
                            </TableCell>
                            {visibleColumns.sequence && (
                              <TableCell
                                className="text-[14px] font-mono text-muted-foreground"
                                title={`ID ${account.id}`}
                              >
                                {(currentPage - 1) * pageSize + index + 1}
                              </TableCell>
                            )}
                            {visibleColumns.email && (
                              <TableCell className="text-[14px] text-muted-foreground">
                                <span>
                                  {account.openai_responses_api
                                    ? formatAccountName(account)
                                    : formatCompactEmail(account.email)}
                                </span>
                                {account.at_only && (
                                  <span className="ml-1.5 inline-flex items-center rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 ring-1 ring-inset ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20">
                                    AT
                                  </span>
                                )}
                                {account.openai_responses_api && (
                                  <span className="ml-1.5 inline-flex items-center rounded-md bg-emerald-50 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 ring-1 ring-inset ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20">
                                    Responses API
                                  </span>
                                )}
                                {account.enabled === false && (
                                  <span className="ml-1.5 inline-flex items-center rounded-md bg-zinc-100 px-1.5 py-0.5 text-[10px] font-medium text-zinc-700 ring-1 ring-inset ring-zinc-500/20 dark:bg-zinc-900 dark:text-zinc-300 dark:ring-zinc-400/20">
                                    <PowerOff className="size-2.5 mr-0.5" />
                                    {t("accounts.disabled")}
                                  </span>
                                )}
                                {account.locked && (
                                  <span className="ml-1.5 inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
                                    <Lock className="size-2.5 mr-0.5" />
                                    {t("accounts.lock")}
                                  </span>
                                )}
                              </TableCell>
                            )}
                            {visibleColumns.tags && (
                              <TableCell className="min-w-[120px]">
                                <ChipList
                                  items={account.tags ?? []}
                                  tone="purple"
                                />
                              </TableCell>
                            )}
                            {visibleColumns.groups && (
                              <TableCell className="min-w-[140px]">
                                <GroupChipList
                                  groups={resolveAccountGroups(
                                    account.group_ids ?? [],
                                    allGroups,
                                  )}
                                />
                              </TableCell>
                            )}
                            {visibleColumns.plan && (
                              <TableCell>
                                <PlanBadge planType={account.plan_type} />
                              </TableCell>
                            )}
                            {visibleColumns.status && (
                              <TableCell>
                                <div className="space-y-1.5">
                                  <div className="flex min-h-6 items-center gap-2 whitespace-nowrap">
                                    <StatusBadge
                                      status={account.status}
                                      detail={
                                        getAccountRateLimitWindow(account) ??
                                        undefined
                                      }
                                    />
                                    <AccountStatusCountdown account={account} />
                                  </div>
                                  {account.status === "error" &&
                                    account.error_message && (
                                      <div
                                        className="max-w-[180px] truncate text-[11px] leading-tight text-red-500"
                                        title={account.error_message}
                                      >
                                        {account.error_message}
                                      </div>
                                    )}
                                  {(account.model_cooldowns?.length ?? 0) >
                                    0 && (
                                    <div className="text-[11px] leading-tight text-amber-600">
                                      model{" "}
                                      {account.model_cooldowns?.[0]?.model}
                                      {(account.model_cooldowns?.length ?? 0) >
                                      1
                                        ? ` +${(account.model_cooldowns?.length ?? 1) - 1}`
                                        : ""}
                                    </div>
                                  )}
                                  <div className="text-[11px] text-muted-foreground">
                                    {t("accounts.healthSummary", {
                                      health: formatHealthTier(
                                        account.health_tier,
                                        t,
                                      ),
                                      score: Math.round(
                                        getDispatchScore(account),
                                      ),
                                      concurrency:
                                        account.dynamic_concurrency_limit ??
                                        "-",
                                    })}
                                  </div>
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.requests && (
                              <TableCell>
                                <div className="space-y-0.5 text-[13px]">
                                  <div className="flex items-center gap-2">
                                    <span className="text-emerald-600 font-medium">
                                      {account.success_requests ?? 0}
                                    </span>
                                    <span className="text-muted-foreground">
                                      /
                                    </span>
                                    <span className="text-red-500 font-medium">
                                      {account.error_requests ?? 0}
                                    </span>
                                  </div>
                                  {((account.retry_error_requests ?? 0) > 0 ||
                                    (account.rate_limit_attempts ?? 0) > 0) && (
                                    <div className="text-[11px] text-muted-foreground">
                                      retry {account.retry_error_requests ?? 0}{" "}
                                      · 429 {account.rate_limit_attempts ?? 0}
                                    </div>
                                  )}
                                </div>
                              </TableCell>
                            )}
                            {visibleColumns.usage && (
                              <TableCell>
                                <UsageCell account={account} />
                              </TableCell>
                            )}
                            {visibleColumns.billed && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                <BilledCell account={account} />
                              </TableCell>
                            )}
                            {visibleColumns.importTime && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                {formatBeijingTime(account.created_at)}
                              </TableCell>
                            )}
                            {visibleColumns.updatedAt && (
                              <TableCell className="text-[13px] text-muted-foreground whitespace-nowrap">
                                {lazyMode ? (
                                  <div className="space-y-0.5 leading-tight">
                                    <div title={t("accounts.recordUpdatedAt")}>
                                      <span className="mr-1 text-[11px] text-muted-foreground/70">
                                        {t("accounts.recordUpdatedAtShort")}
                                      </span>
                                      {formatRelativeTime(account.updated_at)}
                                    </div>
                                    <div title={t("accounts.usageUpdatedAt")}>
                                      <span className="mr-1 text-[11px] text-muted-foreground/70">
                                        {t("accounts.usageUpdatedAtShort")}
                                      </span>
                                      {account.codex_usage_updated_at
                                        ? formatRelativeTime(
                                            account.codex_usage_updated_at,
                                          )
                                        : t("accounts.noUsageUpdatedAt")}
                                    </div>
                                  </div>
                                ) : (
                                  formatRelativeTime(account.updated_at)
                                )}
                              </TableCell>
                            )}
                            {visibleColumns.actions && (
                              <TableCell className="text-right">
                                <div className="flex items-center gap-1 justify-end">
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => openSchedulerEditor(account)}
                                    title={t("accounts.editScheduler")}
                                  >
                                    <Pencil className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => setUsageAccount(account)}
                                    title={t("accounts.usageDetail")}
                                  >
                                    <BarChart3 className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => setTestingAccount(account)}
                                    title={t("accounts.testConnection")}
                                  >
                                    <Zap className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    disabled={
                                      refreshingIds.has(account.id) ||
                                      account.at_only ||
                                      account.openai_responses_api
                                    }
                                    onClick={() => void handleRefresh(account)}
                                    title={
                                      account.at_only ||
                                      account.openai_responses_api
                                        ? t("accounts.atRefreshDisabled")
                                        : t("accounts.refreshAccessToken")
                                    }
                                  >
                                    <RefreshCw
                                      className={`size-3.5 ${refreshingIds.has(account.id) ? "animate-spin" : ""}`}
                                    />
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    disabled={
                                      authJsonExportingIds.has(account.id) ||
                                      account.at_only ||
                                      account.openai_responses_api
                                    }
                                    onClick={() =>
                                      void handleGenerateAuthJSON(account)
                                    }
                                    title={
                                      account.at_only ||
                                      account.openai_responses_api
                                        ? t("accounts.authJsonDisabled")
                                        : t("accounts.generateAuthJson")
                                    }
                                  >
                                    <FileJson className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant={
                                      account.enabled === false
                                        ? "default"
                                        : "outline"
                                    }
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() =>
                                      void handleToggleEnabled(account)
                                    }
                                    title={
                                      account.enabled === false
                                        ? t("accounts.enableHint")
                                        : t("accounts.disableHint")
                                    }
                                  >
                                    {account.enabled === false ? (
                                      <Power className="size-3.5" />
                                    ) : (
                                      <PowerOff className="size-3.5" />
                                    )}
                                  </Button>
                                  <Button
                                    variant={
                                      account.locked ? "default" : "outline"
                                    }
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() =>
                                      void handleToggleLock(account)
                                    }
                                    title={
                                      account.locked
                                        ? t("accounts.unlockHint")
                                        : t("accounts.lockHint")
                                    }
                                  >
                                    {account.locked ? (
                                      <Lock className="size-3.5" />
                                    ) : (
                                      <Unlock className="size-3.5" />
                                    )}
                                  </Button>
                                  <Button
                                    variant="outline"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() =>
                                      void handleResetStatus(account)
                                    }
                                    title={t("accounts.resetStatusHint")}
                                  >
                                    <RotateCcw className="size-3.5" />
                                  </Button>
                                  <Button
                                    variant="destructive"
                                    size="icon"
                                    className="h-7 w-8 px-0"
                                    onClick={() => void handleDelete(account)}
                                    title={t("accounts.deleteAccount")}
                                  >
                                    <Trash2 className="size-3.5" />
                                  </Button>
                                </div>
                              </TableCell>
                            )}
                          </TableRow>
                        );
                      })}
                    </TableBody>
                  </Table>
                </div>
                <Pagination
                  page={currentPage}
                  totalPages={totalPages}
                  onPageChange={setPage}
                  totalItems={sortedAccounts.length}
                  pageSize={pageSize}
                  pageSizeOptions={pageSizeOptions}
                  onPageSizeChange={(nextPageSize) => {
                    setPageSize(nextPageSize);
                    setPage(1);
                  }}
                />
              </StateShell>
            </CardContent>
          </Card>

          <Modal
            show={showAdd}
            title={t("accounts.addTitle")}
            contentClassName="sm:max-w-[640px]"
            onClose={() => {
              setShowAdd(false);
              setAddMethod("rt");
              setOauthStep("generate");
              setOauthSession(null);
              setOauthCallbackUrl("");
              setOauthName("");
              setOpenAIForm({
                base_url: "https://api.openai.com",
                api_key: "",
                models: [],
                proxy_url: "",
              });
              setOpenAIModelDraft("");
            }}
            footer={
              <>
                <Button
                  variant="outline"
                  onClick={() => {
                    setShowAdd(false);
                    setAddMethod("rt");
                    setOauthStep("generate");
                    setOauthSession(null);
                    setOauthCallbackUrl("");
                    setOauthName("");
                    setOpenAIForm({
                      base_url: "https://api.openai.com",
                      api_key: "",
                      models: [],
                      proxy_url: "",
                    });
                    setOpenAIModelDraft("");
                  }}
                >
                  {t("common.cancel")}
                </Button>
                {addMethod === "rt" ? (
                  <Button
                    onClick={() => void handleAdd()}
                    disabled={submitting || !addForm.refresh_token.trim()}
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "at" ? (
                  <Button
                    onClick={() => void handleAddAT()}
                    disabled={submitting || !atForm.access_token.trim()}
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : addMethod === "openai" ? (
                  <Button
                    onClick={() => void handleAddOpenAIResponses()}
                    disabled={
                      submitting ||
                      !openAIForm.api_key.trim() ||
                      openAIForm.models.length === 0
                    }
                  >
                    {submitting ? t("accounts.adding") : t("accounts.submit")}
                  </Button>
                ) : oauthStep === "generate" ? (
                  <Button
                    onClick={() => void handleOAuthGenerate()}
                    disabled={oauthGenerating}
                  >
                    {oauthGenerating
                      ? t("accounts.oauthGenerating")
                      : t("accounts.oauthGenerateBtn")}
                  </Button>
                ) : (
                  <Button
                    onClick={() => void handleOAuthComplete()}
                    disabled={oauthCompleting || !oauthCallbackUrl.trim()}
                  >
                    {oauthCompleting
                      ? t("accounts.oauthCompleting")
                      : t("accounts.oauthCompleteBtn")}
                  </Button>
                )}
              </>
            }
          >
            {/* Tab switcher */}
            <div className="flex gap-1 p-1 mb-5 rounded-xl bg-muted/50 border border-border">
              <button
                onClick={() => setAddMethod("rt")}
                className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                  addMethod === "rt"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <RefreshCw className="size-3.5" />
                {t("accounts.addMethodRT")}
              </button>
              <button
                onClick={() => setAddMethod("at")}
                className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                  addMethod === "at"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <Fingerprint className="size-3.5" />
                {t("accounts.addMethodAT")}
              </button>
              <button
                onClick={() => setAddMethod("openai")}
                className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                  addMethod === "openai"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <KeyRound className="size-3.5" />
                {t("accounts.addMethodOpenAI")}
              </button>
              <button
                onClick={() => {
                  setAddMethod("oauth");
                  setOauthStep("generate");
                  setOauthSession(null);
                  setOauthCallbackUrl("");
                }}
                className={`flex-1 flex items-center justify-center gap-1.5 rounded-lg py-2 text-sm font-semibold transition-all ${
                  addMethod === "oauth"
                    ? "bg-background shadow-sm text-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
              >
                <KeyRound className="size-3.5" />
                {t("accounts.addMethodOAuth")}
              </button>
            </div>

            {addMethod === "rt" ? (
              <div className="space-y-4">
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.refreshTokenLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.refreshTokenPlaceholder")}
                    value={addForm.refresh_token}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setAddForm((form) => ({
                        ...form,
                        refresh_token: event.target.value,
                      }))
                    }
                    rows={6}
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.proxyUrl")}
                  </label>
                  <Input
                    placeholder={t("accounts.proxyUrlPlaceholder")}
                    value={addForm.proxy_url}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setAddForm((form) => ({
                        ...form,
                        proxy_url: event.target.value,
                      }))
                    }
                  />
                </div>
              </div>
            ) : addMethod === "at" ? (
              <div className="space-y-4">
                <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-950/50 dark:text-amber-300">
                  {t("accounts.atWarning")}
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.accessTokenLabel")} *
                  </label>
                  <textarea
                    className="w-full min-h-[160px] p-3 border border-input rounded-xl bg-background text-sm resize-y focus:outline-none focus:ring-2 focus:ring-ring"
                    placeholder={t("accounts.accessTokenPlaceholder")}
                    value={atForm.access_token}
                    onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
                      setAtForm((form) => ({
                        ...form,
                        access_token: event.target.value,
                      }))
                    }
                    rows={6}
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.proxyUrl")}
                  </label>
                  <Input
                    placeholder={t("accounts.proxyUrlPlaceholder")}
                    value={atForm.proxy_url}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setAtForm((form) => ({
                        ...form,
                        proxy_url: event.target.value,
                      }))
                    }
                  />
                </div>
              </div>
            ) : addMethod === "openai" ? (
              <div className="space-y-4">
                <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                  <p className="font-semibold text-foreground mb-1">
                    {t("accounts.openaiResponsesTitle")}
                  </p>
                  <p>{t("accounts.openaiResponsesDesc")}</p>
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.openaiNameLabel")}
                  </label>
                  <Input
                    placeholder={t("accounts.openaiNamePlaceholder")}
                    value={openAIForm.name ?? ""}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        name: event.target.value,
                      }))
                    }
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.openaiBaseUrl")} *
                  </label>
                  <Input
                    placeholder="https://api.openai.com"
                    value={openAIForm.base_url}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        base_url: event.target.value,
                      }))
                    }
                  />
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.openaiApiKey")} *
                  </label>
                  <Input
                    type="password"
                    placeholder="sk-proj-..."
                    value={openAIForm.api_key}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        api_key: event.target.value,
                      }))
                    }
                  />
                </div>
                <div>
                  <div className="mb-2 flex items-center justify-between gap-2">
                    <label className="text-sm font-semibold text-muted-foreground">
                      {t("accounts.openaiModels")} *
                    </label>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => void handleFetchOpenAIModels()}
                      disabled={
                        openAIModelsLoading || !openAIForm.api_key.trim()
                      }
                    >
                      <RefreshCw
                        className={`size-3.5 ${openAIModelsLoading ? "animate-spin" : ""}`}
                      />
                      {openAIModelsLoading
                        ? t("accounts.openaiModelsFetching")
                        : t("accounts.openaiModelsFetch")}
                    </Button>
                  </div>
                  <div className="mb-3 flex gap-2">
                    <Input
                      placeholder={t("accounts.openaiModelsPlaceholder")}
                      value={openAIModelDraft}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setOpenAIModelDraft(event.target.value)
                      }
                      onKeyDown={(event) => {
                        if (event.key === "Enter") {
                          event.preventDefault();
                          addOpenAIModelValues(openAIModelDraft);
                        }
                      }}
                      onPaste={(event) => {
                        const pasted = event.clipboardData.getData("text");
                        if (parseModelTokens(pasted).length > 1) {
                          event.preventDefault();
                          addOpenAIModelValues(pasted);
                        }
                      }}
                    />
                    <Button
                      type="button"
                      variant="outline"
                      onClick={() => addOpenAIModelValues(openAIModelDraft)}
                      disabled={!openAIModelDraft.trim()}
                    >
                      <Plus className="size-3.5" />
                      {t("accounts.openaiModelsAdd")}
                    </Button>
                  </div>
                  <ModelChipGrid
                    models={openAIForm.models}
                    onRemove={removeOpenAIModel}
                    emptyLabel={t("accounts.openaiModelsEmpty")}
                  />
                  <p className="mt-1.5 text-xs text-muted-foreground">
                    {t("accounts.openaiModelsHint", {
                      count: openAIForm.models.length,
                    })}
                  </p>
                </div>
                <div>
                  <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                    {t("accounts.proxyUrl")}
                  </label>
                  <Input
                    placeholder={t("accounts.proxyUrlPlaceholder")}
                    value={openAIForm.proxy_url}
                    onChange={(event: ChangeEvent<HTMLInputElement>) =>
                      setOpenAIForm((form) => ({
                        ...form,
                        proxy_url: event.target.value,
                      }))
                    }
                  />
                </div>
              </div>
            ) : (
              <div className="space-y-5">
                {oauthStep === "generate" ? (
                  <>
                    <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                      <p className="font-semibold text-foreground mb-1">
                        {t("accounts.oauthStep1Title")}
                      </p>
                      <p>{t("accounts.oauthStep1Desc")}</p>
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.oauthNameLabel")}
                      </label>
                      <Input
                        placeholder={t("accounts.oauthNamePlaceholder")}
                        value={oauthName}
                        onChange={(e: ChangeEvent<HTMLInputElement>) =>
                          setOauthName(e.target.value)
                        }
                      />
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.oauthProxyUrl")}
                      </label>
                      <Input
                        placeholder={t("accounts.oauthProxyUrlPlaceholder")}
                        value={oauthProxyUrl}
                        onChange={(e: ChangeEvent<HTMLInputElement>) =>
                          setOauthProxyUrl(e.target.value)
                        }
                      />
                    </div>
                  </>
                ) : (
                  <>
                    <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                      <p className="font-semibold text-foreground mb-1">
                        {t("accounts.oauthStep2Title")}
                      </p>
                      <p>{t("accounts.oauthStep2Desc")}</p>
                    </div>
                    {oauthSession && (
                      <div className="rounded-xl border border-primary/30 bg-primary/5 px-4 py-3">
                        <p className="text-xs font-semibold text-muted-foreground mb-2">
                          {t("accounts.oauthOpenLink")}
                        </p>
                        <a
                          href={oauthSession.auth_url}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="inline-flex items-center gap-1.5 text-sm font-semibold text-primary hover:underline break-all"
                        >
                          <ExternalLink className="size-3.5 shrink-0" />
                          {t("accounts.oauthOpenLink")}
                        </a>
                      </div>
                    )}
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.oauthCallbackUrlLabel")}
                      </label>
                      <Input
                        placeholder={t("accounts.oauthCallbackUrlPlaceholder")}
                        value={oauthCallbackUrl}
                        onChange={(e: ChangeEvent<HTMLInputElement>) =>
                          setOauthCallbackUrl(e.target.value)
                        }
                      />
                      <p className="mt-1.5 text-xs text-muted-foreground">
                        {t("accounts.oauthCallbackUrlHint")}
                      </p>
                    </div>
                    <button
                      onClick={() => {
                        setOauthStep("generate");
                        setOauthSession(null);
                        setOauthCallbackUrl("");
                      }}
                      className="text-xs text-muted-foreground hover:text-foreground underline underline-offset-2"
                    >
                      {t("accounts.oauthRestart")}
                    </button>
                  </>
                )}
              </div>
            )}
          </Modal>

          <Modal
            show={showImportPicker}
            title={t("accounts.importTitle")}
            contentClassName="sm:max-w-[640px]"
            onClose={() => setShowImportPicker(false)}
          >
            <div className="grid grid-cols-2 gap-3">
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  fileInputRef.current?.click();
                }}
              >
                <FileText className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importTxt")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importTxtDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  jsonInputRef.current?.click();
                }}
              >
                <FileJson className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importJson")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importJsonDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  atFileInputRef.current?.click();
                }}
              >
                <Fingerprint className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importAtTxt")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importAtTxtDesc")}
                  </div>
                </div>
              </button>
              <button
                className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                onClick={() => {
                  setShowImportPicker(false);
                  folderInputRef.current?.click();
                }}
              >
                <FolderOpen className="size-5 shrink-0 text-muted-foreground" />
                <div>
                  <div className="text-sm font-medium">
                    {t("accounts.importFolder")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importFolderDesc")}
                  </div>
                </div>
              </button>
            </div>
          </Modal>

          <Modal
            show={showExportPicker}
            title={t("accounts.exportTitle")}
            contentClassName="sm:max-w-[580px]"
            onClose={() => setShowExportPicker(false)}
          >
            <div className="space-y-4">
              {/* 健康账号导出 */}
              <div>
                <div className="text-xs font-semibold text-muted-foreground mb-2">
                  {t("accounts.exportScopeHealthy")}
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                    onClick={() => void handleExport("json", "healthy")}
                  >
                    <FileJson className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">CPA JSON</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportHealthyJsonDesc")}
                      </div>
                    </div>
                  </button>
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors"
                    onClick={() => void handleExport("txt", "healthy")}
                  >
                    <FileText className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">TXT</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportHealthyTxtDesc")}
                      </div>
                    </div>
                  </button>
                </div>
              </div>
              {/* 已选账号导出 */}
              <div>
                <div className="text-xs font-semibold text-muted-foreground mb-2">
                  {t("accounts.exportScopeSelected", { count: selected.size })}
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
                    disabled={selected.size === 0}
                    onClick={() => void handleExport("json", "selected")}
                  >
                    <FileJson className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">CPA JSON</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportSelectedJsonDesc")}
                      </div>
                    </div>
                  </button>
                  <button
                    className="flex items-center gap-3 rounded-xl border border-border px-4 py-3 text-left hover:bg-muted/50 transition-colors disabled:opacity-40 disabled:pointer-events-none"
                    disabled={selected.size === 0}
                    onClick={() => void handleExport("txt", "selected")}
                  >
                    <FileText className="size-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium">TXT</div>
                      <div className="text-[11px] text-muted-foreground">
                        {t("accounts.exportSelectedTxtDesc")}
                      </div>
                    </div>
                  </button>
                </div>
              </div>
            </div>
          </Modal>

          <Modal
            show={Boolean(authJsonModal)}
            title={t("accounts.authJsonModalTitle")}
            contentClassName="sm:max-w-[720px]"
            onClose={() => setAuthJsonModal(null)}
          >
            {authJsonModal && (
              <div className="space-y-4">
                <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/30 px-4 py-3">
                  <FileJson className="mt-0.5 size-5 shrink-0 text-primary" />
                  <div className="min-w-0 space-y-1">
                    <div className="text-sm font-semibold text-foreground">
                      {authJsonModal.account.email ||
                        `ID ${authJsonModal.account.id}`}
                    </div>
                    <p className="text-xs leading-relaxed text-muted-foreground">
                      {t("accounts.authJsonModalDesc")}
                    </p>
                  </div>
                </div>

                <div>
                  <div className="mb-2 text-xs font-semibold text-muted-foreground">
                    {t("accounts.authJsonPreview")}
                  </div>
                  <textarea
                    readOnly
                    value={authJsonModal.json}
                    className="min-h-[260px] w-full resize-y rounded-lg border border-border bg-muted/30 p-3 text-[12px] leading-relaxed text-muted-foreground outline-none focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                    style={{ fontFamily: "var(--font-geist-mono)" }}
                  />
                </div>

                <div className="flex flex-wrap justify-end gap-2">
                  <Button
                    variant="outline"
                    onClick={() => setAuthJsonModal(null)}
                  >
                    {t("common.close")}
                  </Button>
                  <Button
                    variant="outline"
                    onClick={() => void handleCopyAuthJSON()}
                  >
                    <Copy className="size-4" />
                    {t("accounts.copyAuthJson")}
                  </Button>
                  <Button onClick={handleExportAuthJSON}>
                    <Download className="size-4" />
                    {t("accounts.exportAuthJson")}
                  </Button>
                </div>
              </div>
            )}
          </Modal>

          <Modal
            show={showMigrate}
            title={t("accounts.migrateTitle")}
            contentClassName="sm:max-w-[520px]"
            onClose={() => {
              setShowMigrate(false);
              setMigrateUrl("");
              setMigrateKey("");
            }}
          >
            <div className="space-y-4">
              <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                <p>{t("accounts.migrateDesc")}</p>
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                  {t("accounts.migrateUrlLabel")}
                </label>
                <Input
                  placeholder={t("accounts.migrateUrlPlaceholder")}
                  value={migrateUrl}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setMigrateUrl(e.target.value)
                  }
                />
              </div>
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                  {t("accounts.migrateKeyLabel")}
                </label>
                <Input
                  type="password"
                  placeholder={t("accounts.migrateKeyPlaceholder")}
                  value={migrateKey}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setMigrateKey(e.target.value)
                  }
                />
              </div>
              <div className="flex justify-end gap-2 pt-2">
                <Button
                  variant="outline"
                  onClick={() => {
                    setShowMigrate(false);
                    setMigrateUrl("");
                    setMigrateKey("");
                  }}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  onClick={() => void handleMigrate()}
                  disabled={
                    migrating || !migrateUrl.trim() || !migrateKey.trim()
                  }
                >
                  {migrating
                    ? t("accounts.migrating")
                    : t("accounts.migrateConfirm")}
                </Button>
              </div>
            </div>
          </Modal>

          {testingAccount && (
            <TestConnectionModal
              account={testingAccount}
              onSettled={() => {
                void reloadSilently();
              }}
              onClose={() => setTestingAccount(null)}
            />
          )}

          {usageAccount && (
            <AccountUsageModal
              account={usageAccount}
              onClose={() => setUsageAccount(null)}
            />
          )}

          <Modal
            show={Boolean(editingAccount)}
            title={t("accounts.schedulerEditTitle")}
            contentClassName="sm:max-w-[760px]"
            onClose={closeSchedulerEditor}
            footer={
              <>
                <Button
                  variant="outline"
                  onClick={() => closeSchedulerEditor()}
                  disabled={editSubmitting}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  onClick={() => void handleSaveAccountEditor()}
                  disabled={
                    editSubmitting ||
                    (editTab === "scheduler" &&
                      (scoreInputInvalid || concurrencyInputInvalid)) ||
                    openAIAccountInputInvalid
                  }
                >
                  {editSubmitting ? t("common.saving") : t("common.save")}
                </Button>
              </>
            }
          >
            {editingAccount && editPreview ? (
              <div className="space-y-5">
                <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                  <div className="font-semibold text-foreground">
                    {formatAccountName(editingAccount)}
                  </div>
                  <div className="mt-1">
                    {t("accounts.schedulerEditDesc", {
                      plan: editingAccount.plan_type || "-",
                    })}
                  </div>
                </div>

                {editingAccount.openai_responses_api && (
                  <div className="flex gap-1 rounded-xl border border-border bg-muted/50 p-1">
                    <button
                      type="button"
                      onClick={() => setEditTab("scheduler")}
                      className={`flex-1 rounded-lg px-3 py-2 text-sm font-semibold transition-all ${
                        editTab === "scheduler"
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {t("accounts.editTabScheduler")}
                    </button>
                    <button
                      type="button"
                      onClick={() => setEditTab("account")}
                      className={`flex-1 rounded-lg px-3 py-2 text-sm font-semibold transition-all ${
                        editTab === "account"
                          ? "bg-background text-foreground shadow-sm"
                          : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {t("accounts.editTabAccount")}
                    </button>
                  </div>
                )}

                {editTab === "account" &&
                editingAccount.openai_responses_api ? (
                  <div className="space-y-4">
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.openaiNameLabel")}
                      </label>
                      <Input
                        placeholder={t("accounts.openaiNamePlaceholder")}
                        value={editOpenAIForm.name ?? ""}
                        onChange={(event: ChangeEvent<HTMLInputElement>) =>
                          setEditOpenAIForm((form) => ({
                            ...form,
                            name: event.target.value,
                          }))
                        }
                      />
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.openaiBaseUrl")} *
                      </label>
                      <Input
                        placeholder="https://api.openai.com"
                        value={editOpenAIForm.base_url}
                        onChange={(event: ChangeEvent<HTMLInputElement>) =>
                          setEditOpenAIForm((form) => ({
                            ...form,
                            base_url: event.target.value,
                          }))
                        }
                      />
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.openaiApiKey")}
                      </label>
                      <Input
                        type="password"
                        placeholder={t("accounts.openaiApiKeyKeepPlaceholder")}
                        value={editOpenAIForm.api_key ?? ""}
                        onChange={(event: ChangeEvent<HTMLInputElement>) =>
                          setEditOpenAIForm((form) => ({
                            ...form,
                            api_key: event.target.value,
                          }))
                        }
                      />
                    </div>
                    <div>
                      <div className="mb-2 flex items-center justify-between gap-2">
                        <label className="text-sm font-semibold text-muted-foreground">
                          {t("accounts.openaiModels")} *
                        </label>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() => void handleFetchEditOpenAIModels()}
                          disabled={editOpenAIModelsLoading}
                        >
                          <RefreshCw
                            className={`size-3.5 ${editOpenAIModelsLoading ? "animate-spin" : ""}`}
                          />
                          {editOpenAIModelsLoading
                            ? t("accounts.openaiModelsFetching")
                            : t("accounts.openaiModelsFetch")}
                        </Button>
                      </div>
                      <div className="mb-3 flex gap-2">
                        <Input
                          placeholder={t("accounts.openaiModelsPlaceholder")}
                          value={editOpenAIModelDraft}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditOpenAIModelDraft(event.target.value)
                          }
                          onKeyDown={(event) => {
                            if (event.key === "Enter") {
                              event.preventDefault();
                              addEditOpenAIModelValues(editOpenAIModelDraft);
                            }
                          }}
                          onPaste={(event) => {
                            const pasted = event.clipboardData.getData("text");
                            if (parseModelTokens(pasted).length > 1) {
                              event.preventDefault();
                              addEditOpenAIModelValues(pasted);
                            }
                          }}
                        />
                        <Button
                          type="button"
                          variant="outline"
                          onClick={() =>
                            addEditOpenAIModelValues(editOpenAIModelDraft)
                          }
                          disabled={!editOpenAIModelDraft.trim()}
                        >
                          <Plus className="size-3.5" />
                          {t("accounts.openaiModelsAdd")}
                        </Button>
                      </div>
                      <ModelChipGrid
                        models={editOpenAIForm.models}
                        onRemove={removeEditOpenAIModel}
                        emptyLabel={t("accounts.openaiModelsEmpty")}
                      />
                      <p className="mt-1.5 text-xs text-muted-foreground">
                        {t("accounts.openaiModelsHint", {
                          count: editOpenAIForm.models.length,
                        })}
                      </p>
                    </div>
                    <div>
                      <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                        {t("accounts.proxyUrl")}
                      </label>
                      <Input
                        placeholder={t("accounts.proxyUrlPlaceholder")}
                        value={editOpenAIForm.proxy_url}
                        onChange={(event: ChangeEvent<HTMLInputElement>) =>
                          setEditOpenAIForm((form) => ({
                            ...form,
                            proxy_url: event.target.value,
                          }))
                        }
                      />
                    </div>
                  </div>
                ) : (
                  <>
                    <div className="grid gap-4 md:grid-cols-2">
                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerScoreLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerScoreHint")}
                        </div>
                        <div className="mt-3 flex gap-2">
                          <TogglePill
                            active={scoreMode === "default"}
                            onClick={() => setScoreMode("default")}
                            label={t("accounts.schedulerScoreAuto")}
                          />
                          <TogglePill
                            active={scoreMode === "custom"}
                            onClick={() => setScoreMode("custom")}
                            label={t("accounts.schedulerCustom")}
                          />
                        </div>
                        {scoreMode === "default" ? (
                          <div className="mt-3 rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                            {t("accounts.schedulerScoreAutoValue", {
                              value: formatSignedNumber(
                                getDefaultScoreBias(editingAccount.plan_type),
                              ),
                            })}
                          </div>
                        ) : (
                          <div className="mt-3 space-y-2">
                            <Input
                              inputMode="numeric"
                              value={scoreInput}
                              onChange={(
                                event: ChangeEvent<HTMLInputElement>,
                              ) => setScoreInput(event.target.value)}
                              placeholder={t(
                                "accounts.schedulerScorePlaceholder",
                              )}
                            />
                            <div
                              className={`text-xs ${scoreInputInvalid ? "text-red-500" : "text-muted-foreground"}`}
                            >
                              {scoreInputInvalid
                                ? t("accounts.schedulerScoreRange")
                                : t("accounts.schedulerCustomValuePreview", {
                                    value: formatSignedNumber(
                                      parsedScoreBias ??
                                        getEffectiveScoreBias(editingAccount),
                                    ),
                                  })}
                            </div>
                          </div>
                        )}
                      </div>

                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.schedulerConcurrencyLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.schedulerConcurrencyHint")}
                        </div>
                        <div className="mt-3 flex gap-2">
                          <TogglePill
                            active={concurrencyMode === "default"}
                            onClick={() => setConcurrencyMode("default")}
                            label={t("accounts.schedulerConcurrencyAuto")}
                          />
                          <TogglePill
                            active={concurrencyMode === "custom"}
                            onClick={() => setConcurrencyMode("custom")}
                            label={t("accounts.schedulerCustom")}
                          />
                        </div>
                        {concurrencyMode === "default" ? (
                          <div className="mt-3 rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
                            {t("accounts.schedulerConcurrencyAutoValue", {
                              value:
                                getEffectiveBaseConcurrency(editingAccount),
                            })}
                          </div>
                        ) : (
                          <div className="mt-3 space-y-2">
                            <Input
                              inputMode="numeric"
                              value={concurrencyInput}
                              onChange={(
                                event: ChangeEvent<HTMLInputElement>,
                              ) => setConcurrencyInput(event.target.value)}
                              placeholder={t(
                                "accounts.schedulerConcurrencyPlaceholder",
                              )}
                            />
                            <div
                              className={`text-xs ${concurrencyInputInvalid ? "text-red-500" : "text-muted-foreground"}`}
                            >
                              {concurrencyInputInvalid
                                ? t("accounts.schedulerConcurrencyRange")
                                : t("accounts.schedulerCustomValuePreview", {
                                    value:
                                      parsedBaseConcurrency ??
                                      getEffectiveBaseConcurrency(
                                        editingAccount,
                                      ),
                                  })}
                            </div>
                          </div>
                        )}
                      </div>
                    </div>

                    <div className="rounded-xl border border-border p-4">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.allowedAPIKeysLabel")}
                      </div>
                      <div className="mt-1 text-xs text-muted-foreground">
                        {t("accounts.allowedAPIKeysHint")}
                      </div>
                      <div className="mt-3">
                        <APIKeyMultiSelect
                          options={apiKeys}
                          value={allowedAPIKeySelection}
                          disabled={apiKeys.length === 0}
                          onChange={setAllowedAPIKeySelection}
                          allLabel={t("accounts.allowedAPIKeysAll")}
                          selectedLabel={t("accounts.allowedAPIKeysSelected", {
                            count: allowedAPIKeySelection.length,
                          })}
                          placeholder={t("accounts.allowedAPIKeysPlaceholder")}
                          emptyLabel={t("accounts.allowedAPIKeysNoOptions")}
                          emptyHint={t("accounts.allowedAPIKeysNoOptionsHint")}
                        />
                      </div>
                    </div>

                    <div className="rounded-xl border border-border p-4">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.proxyUrl")}
                      </div>
                      <div className="mt-3">
                        <Input
                          placeholder={t("accounts.proxyUrlPlaceholder")}
                          value={editProxyUrl}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditProxyUrl(event.target.value)
                          }
                        />
                      </div>
                    </div>

                    <div className="grid gap-4 md:grid-cols-2">
                      <div className="rounded-xl border border-border p-4">
                        <div className="text-sm font-semibold text-foreground">
                          {t("accounts.tagsLabel")}
                        </div>
                        <div className="mt-1 text-xs text-muted-foreground">
                          {t("accounts.tagsHint")}
                        </div>
                        <ChipInput
                          className="mt-3"
                          value={editTags}
                          onChange={setEditTags}
                          placeholder={t("accounts.tagsPlaceholder")}
                          maxVisible={3}
                        />
                      </div>

                      <div className="rounded-xl border border-border p-4">
                        <div className="flex items-start justify-between gap-3">
                          <div>
                            <div className="text-sm font-semibold text-foreground">
                              {t("accounts.groupsLabel")}
                            </div>
                            <div className="mt-1 text-xs text-muted-foreground">
                              {t("accounts.groupsHint")}
                            </div>
                          </div>
                          <Button
                            type="button"
                            variant="outline"
                            size="xs"
                            onClick={() => setShowGroupManager(true)}
                          >
                            <FolderOpen className="size-3" />
                            {t("accounts.groupManage")}
                          </Button>
                        </div>
                        <div className="mt-3 flex flex-wrap gap-2">
                          {allGroups.length === 0 ? (
                            <span className="text-sm text-muted-foreground">
                              {t("accounts.groupsNone")}
                            </span>
                          ) : (
                            allGroups.map((group) => {
                              const active = editGroupIds.includes(group.id);
                              const color = normalizeGroupColor(group.color);
                              return (
                                <button
                                  key={group.id}
                                  type="button"
                                  onClick={() =>
                                    setEditGroupIds((current) =>
                                      active
                                        ? current.filter(
                                            (id) => id !== group.id,
                                          )
                                        : [...current, group.id],
                                    )
                                  }
                                  className="inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-semibold transition-colors"
                                  style={{
                                    borderColor: active ? color : `${color}55`,
                                    backgroundColor: active
                                      ? color
                                      : `${color}14`,
                                    color: active ? "#ffffff" : color,
                                  }}
                                >
                                  <span className="size-1.5 rounded-full bg-current" />
                                  {group.name}
                                </button>
                              );
                            })
                          )}
                        </div>
                      </div>
                    </div>

                    <div className="rounded-xl border border-border bg-white/60 px-4 py-4 dark:bg-white/5">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.schedulerPreviewTitle")}
                      </div>
                      <div className="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
                        <PreviewItem
                          label={t("accounts.schedulerPreviewRawScore")}
                          value={String(editPreview.rawScore)}
                        />
                        <PreviewItem
                          label={t("accounts.schedulerPreviewDispatchScore")}
                          value={String(editPreview.dispatchScore)}
                        />
                        <PreviewItem
                          label={t("accounts.schedulerPreviewHealthTier")}
                          value={formatHealthTier(editPreview.healthTier, t)}
                        />
                        <PreviewItem
                          label={t(
                            "accounts.schedulerPreviewDynamicConcurrency",
                          )}
                          value={String(editPreview.dynamicConcurrency)}
                        />
                      </div>
                    </div>
                  </>
                )}
              </div>
            ) : null}
          </Modal>

          <Modal
            show={showBatchMetaEditor}
            title={t("accounts.batchMetaTitle")}
            contentClassName="sm:max-w-[560px]"
            onClose={() => {
              if (batchMetaSubmitting) return;
              setShowBatchMetaEditor(false);
            }}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setShowBatchMetaEditor(false)}
                  disabled={batchMetaSubmitting}
                >
                  {t("common.cancel")}
                </Button>
                <Button
                  type="button"
                  onClick={() => void handleBatchSaveMeta()}
                  disabled={batchMetaSubmitting}
                >
                  {batchMetaSubmitting ? t("common.saving") : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="space-y-4">
              <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
                {t("accounts.batchMetaDesc", { count: selected.size })}
              </div>
              <div className="rounded-xl border border-border p-4">
                <div className="text-sm font-semibold text-foreground">
                  {t("accounts.tagsLabel")}
                </div>
                <ChipInput
                  className="mt-3"
                  value={batchTags}
                  onChange={setBatchTags}
                  placeholder={t("accounts.tagsPlaceholder")}
                  maxVisible={6}
                />
              </div>
              <div className="rounded-xl border border-border p-4">
                <div className="text-sm font-semibold text-foreground">
                  {t("accounts.groupsLabel")}
                </div>
                <div className="mt-3 flex flex-wrap gap-2">
                  {allGroups.length === 0 ? (
                    <span className="text-sm text-muted-foreground">
                      {t("accounts.groupsNone")}
                    </span>
                  ) : (
                    allGroups.map((group) => {
                      const active = batchGroupIds.includes(group.id);
                      const color = normalizeGroupColor(group.color);
                      return (
                        <button
                          key={group.id}
                          type="button"
                          onClick={() =>
                            setBatchGroupIds((current) =>
                              active
                                ? current.filter((id) => id !== group.id)
                                : [...current, group.id],
                            )
                          }
                          className="inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-semibold transition-colors"
                          style={{
                            borderColor: active ? color : `${color}55`,
                            backgroundColor: active ? color : `${color}14`,
                            color: active ? "#ffffff" : color,
                          }}
                        >
                          <span className="size-1.5 rounded-full bg-current" />
                          {group.name}
                        </button>
                      );
                    })
                  )}
                </div>
              </div>
            </div>
          </Modal>

          <Modal
            show={showGroupManager}
            title={t("accounts.groupManageTitle")}
            contentClassName="sm:max-w-[820px]"
            bodyClassName="space-y-3"
            onClose={() => {
              if (groupSubmitting) return;
              setShowGroupManager(false);
              resetGroupDraft();
            }}
            footer={
              <>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => {
                    setShowGroupManager(false);
                    resetGroupDraft();
                  }}
                  disabled={groupSubmitting}
                >
                  {t("common.close")}
                </Button>
                <Button
                  type="button"
                  onClick={() => void handleSaveGroup()}
                  disabled={groupSubmitting || !groupDraft.name.trim()}
                >
                  {groupSubmitting
                    ? t("common.saving")
                    : groupDraft.id === null
                      ? t("accounts.groupCreate")
                      : t("common.save")}
                </Button>
              </>
            }
          >
            <div className="flex items-start justify-between gap-3 rounded-lg border border-border bg-muted/20 px-3 py-2.5">
              <div className="min-w-0">
                <div className="text-sm font-semibold text-foreground">
                  {t("accounts.groupManageTitle")}
                </div>
                <div className="mt-0.5 text-xs text-muted-foreground">
                  {t("accounts.groupEmptyDesc")}
                </div>
              </div>
              <Badge variant="secondary" className="shrink-0">
                {t("accounts.groupMembers")}{" "}
                {allGroups.reduce((sum, group) => sum + group.member_count, 0)}
              </Badge>
            </div>

            <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(300px,0.55fr)]">
              <div className="min-h-[260px] overflow-hidden rounded-lg border border-border bg-background">
                <div className="flex h-10 items-center justify-between border-b border-border px-3">
                  <div className="text-xs font-semibold text-muted-foreground">
                    {t("accounts.groupsLabel")}
                  </div>
                  <Badge variant="outline">{allGroups.length}</Badge>
                </div>
                {allGroups.length === 0 ? (
                  <div className="flex min-h-[218px] flex-col items-center justify-center px-4 text-center">
                    <FolderOpen className="mb-3 size-8 text-muted-foreground" />
                    <div className="text-sm font-semibold text-foreground">
                      {t("accounts.groupEmpty")}
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {t("accounts.groupEmptyDesc")}
                    </div>
                  </div>
                ) : (
                  <div className="max-h-[360px] overflow-y-auto p-2">
                    {allGroups.map((group) => {
                      const active = groupDraft.id === group.id;
                      const color = normalizeGroupColor(group.color);
                      return (
                        <div
                          key={group.id}
                          className={`flex items-center gap-3 rounded-md border px-3 py-2 transition-colors ${
                            active
                              ? "border-primary/40 bg-primary/5 shadow-sm"
                              : "border-transparent bg-transparent hover:border-border hover:bg-muted/30"
                          }`}
                        >
                          <span
                            className="size-3 shrink-0 rounded-full"
                            style={{ backgroundColor: color }}
                          />
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                              <span className="truncate text-sm font-semibold text-foreground">
                                {group.name}
                              </span>
                              <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[11px] font-semibold text-muted-foreground">
                                {t("accounts.groupMembers")}{" "}
                                {group.member_count}
                              </span>
                            </div>
                            <div className="mt-0.5 truncate text-xs text-muted-foreground">
                              {group.description ||
                                t("accounts.groupNoDescription")}
                            </div>
                          </div>
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            onClick={() => startEditGroup(group)}
                            title={t("accounts.groupEdit")}
                          >
                            <Pencil className="size-3.5" />
                          </Button>
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            onClick={() => void handleDeleteGroup(group)}
                            disabled={groupSubmitting}
                            title={t("common.delete")}
                          >
                            <Trash2 className="size-3.5 text-red-500" />
                          </Button>
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-border bg-background p-3">
                <div className="flex h-8 items-center justify-between gap-3">
                  <div className="text-sm font-semibold text-foreground">
                    {groupDraft.id === null
                      ? t("accounts.groupCreateTitle")
                      : t("accounts.groupEditTitle")}
                  </div>
                  {groupDraft.id !== null ? (
                    <Button
                      type="button"
                      variant="ghost"
                      size="xs"
                      onClick={resetGroupDraft}
                    >
                      <Plus className="size-3" />
                      {t("accounts.groupCreate")}
                    </Button>
                  ) : null}
                </div>
                <div className="mt-4 space-y-3">
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupName")}
                    </span>
                    <Input
                      value={groupDraft.name}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          name: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupNamePlaceholder")}
                      maxLength={80}
                    />
                  </label>
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupDescription")}
                    </span>
                    <Input
                      value={groupDraft.description}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          description: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupDescriptionPlaceholder")}
                      maxLength={240}
                    />
                  </label>
                  <div className="space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("accounts.groupColor")}
                    </span>
                    <div className="flex flex-wrap gap-2">
                      {ACCOUNT_GROUP_COLORS.map((color) => (
                        <button
                          key={color}
                          type="button"
                          className={`size-8 rounded-lg border transition-transform hover:scale-105 ${
                            normalizeGroupColor(groupDraft.color) === color
                              ? "border-foreground ring-2 ring-ring/30"
                              : "border-border"
                          }`}
                          style={{ backgroundColor: color }}
                          onClick={() =>
                            setGroupDraft((draft) => ({ ...draft, color }))
                          }
                          aria-label={color}
                        />
                      ))}
                    </div>
                    <Input
                      className="h-8 font-mono text-xs"
                      value={groupDraft.color}
                      onChange={(event: ChangeEvent<HTMLInputElement>) =>
                        setGroupDraft((draft) => ({
                          ...draft,
                          color: event.target.value,
                        }))
                      }
                      placeholder={t("accounts.groupColorPlaceholder")}
                      maxLength={20}
                    />
                  </div>
                </div>
              </div>
            </div>
          </Modal>

          <Modal
            show={importProgress.show}
            title={
              importProgress.done
                ? t("accounts.importDone")
                : t("accounts.importingProgress")
            }
            contentClassName="sm:max-w-[420px]"
            onClose={() => setImportProgress((p) => ({ ...p, show: false }))}
          >
            <div className="space-y-4">
              <div className="w-full h-3 bg-muted rounded-full overflow-hidden">
                <div
                  className="h-full bg-primary rounded-full transition-all duration-300 ease-out"
                  style={{
                    width:
                      importProgress.total > 0
                        ? `${Math.round((importProgress.current / importProgress.total) * 100)}%`
                        : "0%",
                  }}
                />
              </div>
              <div className="text-center text-sm text-muted-foreground">
                {importProgress.total > 0
                  ? `${importProgress.current} / ${importProgress.total}  (${Math.round((importProgress.current / importProgress.total) * 100)}%)`
                  : t("accounts.importPreparing")}
              </div>
              <div className="grid grid-cols-3 gap-3 text-center">
                <div className="rounded-xl bg-emerald-500/10 px-3 py-2">
                  <div className="text-lg font-bold text-emerald-600">
                    {importProgress.success}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importSuccess")}
                  </div>
                </div>
                <div className="rounded-xl bg-amber-500/10 px-3 py-2">
                  <div className="text-lg font-bold text-amber-600">
                    {importProgress.duplicate}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importDuplicate")}
                  </div>
                </div>
                <div className="rounded-xl bg-red-500/10 px-3 py-2">
                  <div className="text-lg font-bold text-red-600">
                    {importProgress.failed}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("accounts.importFailedCount")}
                  </div>
                </div>
              </div>
              {importProgress.done && (
                <p className="text-xs text-center text-muted-foreground">
                  {t("accounts.importDoneHint")}
                </p>
              )}
            </div>
          </Modal>

          {confirmDialog}

          <ToastNotice toast={toast} />
        </>
      </StateShell>
    </div>
  );
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

function formatJSONText(text: string) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

async function copyTextToClipboard(text: string) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.top = "-1000px";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  document.body.removeChild(textarea);
  if (!copied) {
    throw new Error("copy failed");
  }
}

function filterExistingAPIKeyIDs(
  selected: number[],
  apiKeys: APIKeyRow[],
): number[] {
  if (!selected.length || !apiKeys.length) {
    return [];
  }
  const existing = new Set(apiKeys.map((item) => item.id));
  return [...new Set(selected.filter((id) => existing.has(id)))].sort(
    (a, b) => a - b,
  );
}

function formatAPIKeyOptionLabel(apiKey: APIKeyRow): string {
  const name = apiKey.name?.trim() || `API Key #${apiKey.id}`;
  return `${name} · ${apiKey.key}`;
}

function APIKeyMultiSelect({
  options,
  value,
  disabled,
  onChange,
  allLabel,
  selectedLabel,
  placeholder,
  emptyLabel,
  emptyHint,
}: {
  options: APIKeyRow[];
  value: number[];
  disabled: boolean;
  onChange: (value: number[]) => void;
  allLabel: string;
  selectedLabel: string;
  placeholder: string;
  emptyLabel: string;
  emptyHint: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;

    const handlePointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };

    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
      }
    };

    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("keydown", handleEscape);
    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [open]);

  const summary = value.length === 0 ? allLabel : selectedLabel;

  const toggleOption = (id: number) => {
    if (disabled) return;
    if (value.includes(id)) {
      onChange(value.filter((item) => item !== id));
      return;
    }
    onChange([...value, id].sort((a, b) => a - b));
  };

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        disabled={disabled}
        className={`flex w-full items-center justify-between gap-3 rounded-md border border-input bg-background px-3.5 py-3 text-left shadow-xs transition-[border-color,box-shadow] ${
          disabled
            ? "cursor-not-allowed opacity-70"
            : "hover:border-primary/30 hover:bg-accent/40"
        } ${open ? "border-primary/35 ring-[3px] ring-primary/10" : ""}`}
        onClick={() => {
          if (!disabled) {
            setOpen((current) => !current);
          }
        }}
      >
        <div className="min-w-0">
          <div className="truncate text-[15px] text-foreground">{summary}</div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            {disabled ? emptyHint : placeholder}
          </div>
        </div>
        <ChevronDown
          className={`size-4 shrink-0 text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
        />
      </button>

      {open ? (
        <div className="absolute left-0 right-0 top-[calc(100%+0.5rem)] z-50 overflow-hidden rounded-lg border border-border bg-popover shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm">
          {options.length === 0 ? (
            <div className="px-4 py-3 text-sm text-muted-foreground">
              {emptyLabel}
            </div>
          ) : (
            <div className="max-h-72 space-y-1 overflow-auto p-2">
              {options.map((option) => {
                const checked = value.includes(option.id);
                return (
                  <button
                    key={option.id}
                    type="button"
                    className={`flex w-full items-center gap-3 rounded-md px-3 py-2.5 text-left transition-colors ${
                      checked
                        ? "bg-primary/10 text-primary"
                        : "text-foreground hover:bg-accent/70"
                    }`}
                    onClick={() => toggleOption(option.id)}
                  >
                    <span
                      className={`flex size-4 items-center justify-center rounded border ${checked ? "border-primary bg-primary text-primary-foreground" : "border-border bg-background text-transparent"}`}
                    >
                      <Check className="size-3" />
                    </span>
                    <span className="min-w-0 flex-1 truncate text-sm">
                      {formatAPIKeyOptionLabel(option)}
                    </span>
                  </button>
                );
              })}
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

function ModelChipGrid({
  models,
  onRemove,
  emptyLabel,
}: {
  models: string[];
  onRemove: (model: string) => void;
  emptyLabel: string;
}) {
  if (models.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-3 text-sm text-muted-foreground">
        {emptyLabel}
      </div>
    );
  }
  return (
    <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
      {models.map((model) => (
        <div
          key={model}
          className="flex min-h-10 items-center justify-between gap-2 rounded-md border border-border bg-background px-3 py-2 text-sm"
          title={model}
        >
          <span className="min-w-0 truncate font-mono text-[12px] text-foreground">
            {model}
          </span>
          <button
            type="button"
            className="inline-flex size-6 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            onClick={() => onRemove(model)}
            aria-label={`Remove ${model}`}
          >
            <X className="size-3.5" />
          </button>
        </div>
      ))}
    </div>
  );
}

function TogglePill({
  active,
  onClick,
  label,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-full px-3 py-1.5 text-xs font-semibold transition-colors ${
        active
          ? "bg-primary text-primary-foreground"
          : "bg-muted/50 text-muted-foreground hover:bg-muted"
      }`}
    >
      {label}
    </button>
  );
}

function PreviewItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border bg-muted/20 px-3 py-3">
      <div className="text-[11px] font-semibold text-muted-foreground">
        {label}
      </div>
      <div className="mt-1 text-base font-semibold text-foreground">
        {value}
      </div>
    </div>
  );
}

function parseIntegerInput(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed || !/^-?\d+$/.test(trimmed)) {
    return null;
  }

  return Number.parseInt(trimmed, 10);
}

function getDispatchScore(account: AccountRow): number {
  return account.dispatch_score ?? account.scheduler_score ?? 0;
}

// OpenAI reports the $100 Pro tier as "prolite" — functionally a Pro plan with
// a smaller usage cap. Keep behavioral comparisons (usage windows, plan filter,
// scheduler bias) aligned with the Go side by folding it into "pro".
function normalizePlanType(planType?: string): string {
  const raw = (planType || "").toLowerCase().trim();
  if (raw === "prolite" || raw === "pro_lite" || raw === "pro-lite")
    return "pro";
  return raw;
}

function isFutureTime(value?: string): boolean {
  if (!value) return false;
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) && timestamp > Date.now();
}

function isUsageWindowExhausted(value?: number | null): boolean {
  return typeof value === "number" && Number.isFinite(value) && value >= 100;
}

function isActiveUsageWindowExhausted(
  value?: number | null,
  resetAt?: string,
): boolean {
  return isUsageWindowExhausted(value) && (!resetAt || isFutureTime(resetAt));
}

function isPremiumUsagePlan(planType?: string): boolean {
  return ["plus", "pro", "team", "teamplus"].includes(
    normalizePlanType(planType),
  );
}

type RateLimitWindow = "5h" | "7d";

function isRateLimitedAccount(account: AccountRow): boolean {
  return getAccountRateLimitWindow(account) !== null;
}

function getAccountRateLimitWindow(
  account: AccountRow,
): RateLimitWindow | null {
  const status = (account.status || "").toLowerCase();
  const reason = (account.cooldown_reason || "").toLowerCase();
  const explicitlyRateLimited =
    status === "rate_limited" ||
    status === "usage_exhausted" ||
    status === "rate_limited_5h" ||
    status === "rate_limited_7d" ||
    reason === "rate_limited" ||
    reason === "rate_limited_5h" ||
    reason === "rate_limited_7d";
  const has7dLimit = isActiveUsageWindowExhausted(
    account.usage_percent_7d,
    account.reset_7d_at,
  );
  const has5hLimit =
    isPremiumUsagePlan(account.plan_type) &&
    isActiveUsageWindowExhausted(account.usage_percent_5h, account.reset_5h_at);

  // Prefer the longer 7d window when both windows are exhausted so each account
  // belongs to exactly one bucket and 5h + 7d stays equal to total limited.
  if (
    status === "usage_exhausted" ||
    status === "rate_limited_7d" ||
    reason === "rate_limited_7d" ||
    has7dLimit
  ) {
    return "7d";
  }

  if (
    status === "rate_limited_5h" ||
    reason === "rate_limited_5h" ||
    has5hLimit
  ) {
    return "5h";
  }

  return explicitlyRateLimited ? "5h" : null;
}

function getRateLimitedWindowStats(accounts: AccountRow[]): {
  total: number;
  fiveHour: number;
  sevenDay: number;
} {
  const stats = accounts.reduce(
    (stats, account) => {
      const window = getAccountRateLimitWindow(account);
      if (!window) {
        return stats;
      }
      if (window === "7d") {
        stats.sevenDay += 1;
      } else {
        stats.fiveHour += 1;
      }
      return stats;
    },
    { fiveHour: 0, sevenDay: 0 },
  );

  return {
    ...stats,
    total: stats.fiveHour + stats.sevenDay,
  };
}

function isSubscriptionPlan(planType?: string): boolean {
  const normalized = normalizePlanType(planType);
  if (!normalized || normalized === "free") return false;
  if (
    [
      "plus",
      "pro",
      "team",
      "teamplus",
      "enterprise",
      "business",
      "edu",
      "education",
    ].includes(normalized)
  ) {
    return true;
  }
  return (
    normalized.includes("plus") ||
    normalized.startsWith("pro") ||
    normalized.startsWith("team") ||
    normalized.includes("enterprise") ||
    normalized.includes("business")
  );
}

interface HeaderActionMenuItem {
  key: string;
  label: string;
  icon: ReactNode;
  disabled?: boolean;
  title?: string;
  onSelect: () => void;
}

function HeaderActionMenu({
  label,
  icon,
  items,
}: {
  label: string;
  icon: ReactNode;
  items: HeaderActionMenuItem[];
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;

    const handlePointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };

    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
      }
    };

    document.addEventListener("mousedown", handlePointerDown);
    document.addEventListener("keydown", handleEscape);

    return () => {
      document.removeEventListener("mousedown", handlePointerDown);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [open]);

  return (
    <div ref={rootRef} className="relative">
      <Button
        type="button"
        variant="outline"
        size="sm"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((current) => !current)}
      >
        {icon}
        {label}
        <ChevronDown
          className={`size-3.5 transition-transform ${open ? "rotate-180" : ""}`}
        />
      </Button>

      {open ? (
        <div className="absolute right-0 top-[calc(100%+0.5rem)] z-50 w-56 overflow-hidden rounded-lg border border-border bg-popover p-1.5 shadow-[0_18px_40px_hsl(222_30%_18%/0.12)] backdrop-blur-sm">
          <div role="menu" className="space-y-0.5">
            {items.map((item) => (
              <button
                key={item.key}
                type="button"
                role="menuitem"
                disabled={item.disabled}
                title={item.title}
                className="flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm text-foreground transition-colors hover:bg-accent/70 disabled:cursor-not-allowed disabled:opacity-50"
                onClick={() => {
                  if (item.disabled) return;
                  setOpen(false);
                  item.onSelect();
                }}
              >
                <span className="flex size-5 shrink-0 items-center justify-center text-muted-foreground">
                  {item.icon}
                </span>
                <span className="min-w-0 flex-1 truncate">{item.label}</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}

function formatPlanLabel(planType?: string): string {
  const raw = (planType || "").trim();
  if (!raw) return "-";
  const lower = raw.toLowerCase();
  if (lower === "prolite" || lower === "pro_lite" || lower === "pro-lite")
    return "ProLite";
  return raw;
}

function PlanBadge({ planType }: { planType?: string }) {
  const label = formatPlanLabel(planType);
  if (label === "-")
    return <span className="text-[12px] text-muted-foreground">-</span>;

  const style: Record<string, string> = {
    pro: "bg-violet-100 text-violet-700 ring-violet-500/30 dark:bg-violet-500/20 dark:text-violet-300 dark:ring-violet-400/30",
    prolite:
      "bg-purple-50 text-purple-600 ring-purple-400/25 dark:bg-purple-500/15 dark:text-purple-300 dark:ring-purple-400/25",
    plus: "bg-blue-100 text-blue-700 ring-blue-500/30 dark:bg-blue-500/20 dark:text-blue-300 dark:ring-blue-400/30",
    team: "bg-amber-100 text-amber-700 ring-amber-500/30 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-400/30",
    free: "bg-zinc-100 text-zinc-500 ring-zinc-400/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/15",
  };

  const normalized = normalizePlanType(planType);
  const key =
    normalized === "pro" && label === "ProLite" ? "prolite" : normalized;
  const cls =
    style[key] ||
    "bg-slate-100 text-slate-600 ring-slate-400/20 dark:bg-slate-500/15 dark:text-slate-300 dark:ring-slate-400/20";

  return (
    <span
      className={`inline-flex items-center rounded-md px-2.5 py-1 text-[13px] font-semibold ring-1 ring-inset ${cls}`}
    >
      {label}
    </span>
  );
}

function getDefaultScoreBias(planType?: string): number {
  switch (normalizePlanType(planType)) {
    case "pro":
    case "plus":
    case "team":
      return 50;
    default:
      return 0;
  }
}

function getEffectiveScoreBias(account: AccountRow): number {
  if (typeof account.score_bias_effective === "number") {
    return account.score_bias_effective;
  }
  if (typeof account.score_bias_override === "number") {
    return account.score_bias_override;
  }
  return getDefaultScoreBias(account.plan_type);
}

function getEffectiveBaseConcurrency(account: AccountRow): number {
  if (
    typeof account.base_concurrency_effective === "number" &&
    account.base_concurrency_effective > 0
  ) {
    return account.base_concurrency_effective;
  }
  if (
    typeof account.base_concurrency_override === "number" &&
    account.base_concurrency_override > 0
  ) {
    return account.base_concurrency_override;
  }
  if (
    typeof account.dynamic_concurrency_limit === "number" &&
    account.dynamic_concurrency_limit > 0
  ) {
    return account.dynamic_concurrency_limit;
  }
  return 1;
}

function computePreviewDispatchScore(
  account: AccountRow,
  rawScore: number,
  appliedBias: number,
): number {
  if (
    (account.health_tier === "healthy" || account.health_tier === "warm") &&
    (account.status === "active" || account.status === "ready")
  ) {
    return rawScore + appliedBias;
  }
  return rawScore;
}

function computePreviewDynamicConcurrency(
  account: AccountRow,
  baseConcurrency: number,
): number {
  switch (account.health_tier) {
    case "healthy":
      return baseConcurrency;
    case "warm":
      return Math.max(1, Math.floor(baseConcurrency / 2));
    case "risky":
      return 1;
    case "banned":
      return 0;
    default:
      return account.dynamic_concurrency_limit ?? baseConcurrency;
  }
}

function formatSignedNumber(value: number): string {
  if (value > 0) return `+${value}`;
  return String(value);
}

function CompactStat({
  label,
  chipLabel,
  value,
  tone,
  details,
}: {
  label: string;
  chipLabel?: string;
  value: number;
  tone: "neutral" | "success" | "warning" | "danger";
  details?: Array<{ label: string; value: number }>;
}) {
  const toneStyle = {
    neutral: {
      chip: "bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300",
      dot: "bg-slate-500",
    },
    success: {
      chip: "bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300",
      dot: "bg-emerald-500",
    },
    warning: {
      chip: "bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300",
      dot: "bg-amber-500",
    },
    danger: {
      chip: "bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300",
      dot: "bg-red-500",
    },
  }[tone];

  return (
    <div className="flex min-h-[88px] items-center justify-between gap-3 rounded-lg border border-border bg-card/85 px-3 py-2.5 shadow-sm">
      <div className="min-w-0">
        <div className="text-[12px] font-semibold text-muted-foreground">
          {label}
        </div>
        <div className="mt-1 text-[24px] font-bold leading-none text-foreground">
          {value}
        </div>
      </div>
      <div className="flex min-h-[58px] shrink-0 flex-col items-end gap-1.5">
        <div
          className={`inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-[12px] font-semibold ${toneStyle.chip}`}
        >
          <span className={`size-2 rounded-full ${toneStyle.dot}`} />
          {chipLabel ?? label}
        </div>
        {details && details.length > 0 && (
          <div className="flex flex-col items-end gap-0.5 text-[11px] font-semibold leading-4 text-muted-foreground">
            {details.map((item) => (
              <div
                key={item.label}
                className="grid grid-cols-[max-content_auto_max-content] items-center gap-x-0.5 whitespace-nowrap tabular-nums"
              >
                <span className="justify-self-start">{item.label}</span>
                <span className="justify-self-center">：</span>
                <span className="justify-self-end text-foreground">
                  {item.value}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function SchedulerChip({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
  tone: "neutral" | "success" | "warning" | "danger";
}) {
  const toneStyle = {
    neutral:
      "bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300",
    success:
      "bg-emerald-500/10 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300",
    warning:
      "bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300",
    danger: "bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300",
  }[tone];

  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 font-semibold ${toneStyle}`}
    >
      <span>{label}</span>
      <span>{value}</span>
    </span>
  );
}

function ChipList({
  items,
  tone,
}: {
  items: string[];
  tone: "purple" | "blue";
}) {
  if (items.length === 0) return null;
  const visible = items.slice(0, 3);
  const hidden = items.length - visible.length;
  const toneClass =
    tone === "purple"
      ? "bg-purple-500/10 text-purple-700 ring-purple-500/20 dark:text-purple-300"
      : "bg-blue-500/10 text-blue-700 ring-blue-500/20 dark:text-blue-300";

  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
      {visible.map((item) => (
        <span
          key={item}
          className={`inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-semibold ring-1 ring-inset ${toneClass}`}
        >
          {item}
        </span>
      ))}
      {hidden > 0 && (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">
          +{hidden}
        </span>
      )}
    </div>
  );
}

function normalizeGroupColor(color?: string): string {
  const value = (color || "").trim();
  return /^#[0-9a-fA-F]{6}$/.test(value) ? value : ACCOUNT_GROUP_COLORS[0];
}

function resolveAccountGroups(
  ids: number[],
  groups: AccountGroup[],
): AccountGroup[] {
  if (ids.length === 0 || groups.length === 0) return [];
  const byID = new Map(groups.map((group) => [group.id, group]));
  return ids.map((id) => byID.get(id)).filter(Boolean) as AccountGroup[];
}

function GroupChipList({ groups }: { groups: AccountGroup[] }) {
  if (groups.length === 0) return null;
  const visible = groups.slice(0, 3);
  const hidden = groups.length - visible.length;

  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
      {visible.map((group) => {
        const color = normalizeGroupColor(group.color);
        return (
          <span
            key={group.id}
            className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold"
            style={{
              backgroundColor: `${color}14`,
              color,
              boxShadow: `inset 0 0 0 1px ${color}33`,
            }}
            title={group.description || group.name}
          >
            <span className="size-1.5 rounded-full bg-current" />
            {group.name}
          </span>
        );
      })}
      {hidden > 0 && (
        <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">
          +{hidden}
        </span>
      )}
    </div>
  );
}

function ColumnSettingsMenu({
  columns,
  onToggle,
  onReset,
  resetTitle,
  labels,
  title,
}: {
  columns: Record<AccountTableColumn, boolean>;
  onToggle: (column: AccountTableColumn) => void;
  onReset: () => void;
  resetTitle: string;
  labels: Record<AccountTableColumn, string>;
  title: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const handler = (event: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open]);

  return (
    <div ref={rootRef} className="relative">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => setOpen((current) => !current)}
        title={title}
      >
        <SlidersHorizontal className="size-3.5" />
        {title}
      </Button>
      {open ? (
        <div className="absolute right-0 top-[calc(100%+0.5rem)] z-50 w-48 overflow-hidden rounded-lg border border-border bg-popover p-1.5 shadow-lg">
          <button
            type="button"
            className="mb-1 flex w-full items-center justify-center rounded-md px-2.5 py-1.5 text-xs font-semibold text-primary transition-colors hover:bg-accent/70"
            onClick={onReset}
          >
            {resetTitle}
          </button>
          {ACCOUNT_TABLE_COLUMNS.map((column) => (
            <button
              key={column}
              type="button"
              role="menuitemcheckbox"
              aria-checked={columns[column]}
              className="flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm text-foreground transition-colors hover:bg-accent/70"
              onClick={() => onToggle(column)}
            >
              <span
                className={`flex size-4 shrink-0 items-center justify-center rounded border ${columns[column] ? "border-primary bg-primary text-primary-foreground" : "border-border bg-background"}`}
              >
                {columns[column] ? <Check className="size-3" /> : null}
              </span>
              <span className="min-w-0 flex-1 truncate">{labels[column]}</span>
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}

function formatHealthTier(healthTier?: string, t?: any) {
  if (!t) return "Unknown";
  switch (healthTier) {
    case "healthy":
      return t("accounts.healthy");
    case "warm":
      return t("accounts.warm");
    case "risky":
      return t("accounts.risky");
    case "banned":
      return t("accounts.quarantine");
    default:
      return t("accounts.unknown");
  }
}

// ==================== 测试连接弹窗 ====================

interface TestEvent {
  type: "test_start" | "content" | "test_complete" | "error";
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
}

function formatTestErrorMessage(message: string) {
  const normalized = message.trim();
  const jsonStart = normalized.indexOf("{");

  if (jsonStart === -1) {
    return normalized;
  }

  const prefix = normalized
    .slice(0, jsonStart)
    .trim()
    .replace(/[：:]\s*$/, "");
  const jsonText = normalized.slice(jsonStart);

  try {
    const parsed = JSON.parse(jsonText);
    const prettyJson = JSON.stringify(parsed, null, 2);
    return prefix ? `${prefix}\n${prettyJson}` : prettyJson;
  } catch {
    return normalized;
  }
}

function formatTestOutput(text: string) {
  try {
    const parsed = JSON.parse(text);
    return JSON.stringify(parsed, null, 2);
  } catch {
    return text;
  }
}

const DEFAULT_TEST_MODEL = "gpt-5.4";

function isConnectionTestModel(model: string) {
  const value = model.trim().toLowerCase();
  return value !== "" && !value.includes("image");
}

function extractTextModels(
  modelsResp: Awaited<ReturnType<typeof api.getModels>>,
) {
  if (modelsResp.items && modelsResp.items.length > 0) {
    return modelsResp.items
      .filter(
        (item) =>
          item.enabled &&
          item.category !== "image" &&
          !item.id.includes("image"),
      )
      .map((item) => item.id);
  }
  return (modelsResp.models ?? []).filter(isConnectionTestModel);
}

function uniqueTestModels(
  models: string[],
  preferredModel?: string,
  includeDefault = true,
) {
  const seen = new Set<string>();
  const result: string[] = [];
  const candidates = [
    preferredModel ?? "",
    ...models,
    ...(includeDefault ? [DEFAULT_TEST_MODEL] : []),
  ];

  for (const model of candidates) {
    const value = model.trim();
    if (!isConnectionTestModel(value) || seen.has(value)) continue;
    seen.add(value);
    result.push(value);
  }
  return result;
}

function TestConnectionModal({
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

  const isOpenAIResponsesAccount = Boolean(account.openai_responses_api);

  const modelSelectOptions = useMemo(
    () =>
      uniqueTestModels(
        modelOptions,
        selectedModel,
        !isOpenAIResponsesAccount,
      ).map((item) => ({ label: item, value: item })),
    [isOpenAIResponsesAccount, modelOptions, selectedModel],
  );

  useEffect(() => {
    let active = true;

    const loadModels = async () => {
      try {
        const settings = await api.getSettings();
        if (!active) return;

        if (isOpenAIResponsesAccount) {
          const accountModels = (account.models ?? []).filter(
            isConnectionTestModel,
          );
          const preferredModel =
            accountModels.find(
              (item) =>
                item.toLowerCase() === settings.test_model.toLowerCase(),
            ) ?? accountModels[0];
          const nextModels = uniqueTestModels(
            accountModels,
            preferredModel,
            false,
          );
          setModelOptions(nextModels);
          setSelectedModel((current) => current || nextModels[0] || "");
          return;
        }

        const modelsResp = await api.getModels();
        if (!active) return;
        const upstreamModels = extractTextModels(modelsResp);
        const preferredModel = isConnectionTestModel(settings.test_model)
          ? settings.test_model
          : DEFAULT_TEST_MODEL;
        const nextModels = uniqueTestModels(upstreamModels, preferredModel);
        setModelOptions(nextModels);
        setSelectedModel(
          (current) => current || nextModels[0] || DEFAULT_TEST_MODEL,
        );
      } catch {
        if (!active) return;
        if (isOpenAIResponsesAccount) {
          const fallbackModels = uniqueTestModels(
            (account.models ?? []).filter(isConnectionTestModel),
            undefined,
            false,
          );
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0] || "");
        } else {
          const fallbackModels = uniqueTestModels([], DEFAULT_TEST_MODEL);
          setModelOptions(fallbackModels);
          setSelectedModel((current) => current || fallbackModels[0]);
        }
      } finally {
        if (active) {
          setModelOptionsReady(true);
        }
      }
    };

    void loadModels();

    return () => {
      active = false;
    };
  }, [account.models, isOpenAIResponsesAccount]);

  useEffect(() => {
    if (!modelOptionsReady || !selectedModel) return;

    // 重置状态（StrictMode 二次 mount 时清理上一次的残留）
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
        let receivedTerminalEvent = false;

        const processEventLines = (lines: string[]) => {
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
                  if (event.text) {
                    setOutput((prev) => [...prev, event.text!]);
                  }
                  break;
                case "test_complete":
                  receivedTerminalEvent = true;
                  setStatus(event.success ? "success" : "error");
                  markSettled();
                  break;
                case "error":
                  receivedTerminalEvent = true;
                  setStatus("error");
                  setErrorMsg(event.error || t("accounts.unknownError"));
                  markSettled();
                  break;
              }
            } catch {
              /* ignore non-JSON lines */
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
          processEventLines(lines);
        }

        if (buffer.trim()) {
          processEventLines([buffer]);
        }

        if (!receivedTerminalEvent) {
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

    // 延迟 50ms 启动，确保 StrictMode cleanup 有足够时间执行 abort
    const timer = window.setTimeout(() => {
      void run();
    }, 50);

    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [account.id, markSettled, modelOptionsReady, selectedModel, t]);

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const statusLabel = {
    connecting: `⏳ ${t("accounts.connecting")}`,
    streaming: `🔄 ${t("accounts.receivingResponse")}`,
    success: `✅ ${t("accounts.testSuccess")}`,
    error: `❌ ${t("accounts.testFailed")}`,
  }[status];

  const statusColor = {
    connecting: "text-muted-foreground",
    streaming: "text-blue-500",
    success: "text-emerald-500",
    error: "text-red-500",
  }[status];
  const formattedErrorMsg = errorMsg ? formatTestErrorMessage(errorMsg) : "";

  return (
    <Modal
      show={true}
      title={t("accounts.testConnectionTitle", {
        account: formatAccountName(account),
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
            className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}
          >
            {statusLabel}
          </span>
          <Select
            className="w-52 max-w-full"
            compact
            value={selectedModel}
            onValueChange={setSelectedModel}
            options={modelSelectOptions}
            placeholder={model || t("settings.testModel")}
            disabled={!modelOptionsReady || modelSelectOptions.length === 0}
          />
        </div>

        {(output.length > 0 ||
          status === "connecting" ||
          status === "streaming") && (
          <div
            className="min-h-[80px] max-h-[240px] overflow-auto rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed whitespace-pre-wrap break-all"
            style={{ fontFamily: "var(--font-geist-mono)" }}
          >
            {output.length === 0 && status === "connecting" && (
              <span className="text-muted-foreground animate-pulse">
                {t("accounts.sendingTestRequest")}
              </span>
            )}
            {output.join("")}
            <div ref={outputEndRef} />
          </div>
        )}

        {errorMsg && (
          <div className="max-h-[40vh] overflow-auto rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 text-sm font-semibold">
              {t("accounts.failureDetails")}
            </div>
            <pre
              className="text-[13px] leading-relaxed whitespace-pre-wrap break-all"
              style={{ fontFamily: "var(--font-geist-mono)" }}
            >
              {formattedErrorMsg}
            </pre>
          </div>
        )}

        {status === "success" && (
          <div className="flex items-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2.5 text-sm text-emerald-700 dark:border-emerald-900/50 dark:bg-emerald-950/30 dark:text-emerald-400">
            <RotateCcw className="size-4 shrink-0" />
            {t("accounts.testAutoReset")}
          </div>
        )}
      </div>
    </Modal>
  );
}

// 格式化重置时间为具体时间
function formatResetAt(resetAt: string | undefined): string | null {
  if (!resetAt) return null;
  const d = new Date(resetAt);
  if (d.getTime() <= Date.now()) return null;
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function formatCompactUsageNumber(value?: number): string {
  const n = Number(value || 0);
  if (n >= 1_000_000)
    return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`;
  return String(n);
}

function hasUsageWindowDetail(detail?: AccountRow["usage_5h_detail"]): boolean {
  return Boolean(
    detail && ((detail.requests ?? 0) > 0 || (detail.tokens ?? 0) > 0),
  );
}

// 用量进度条颜色
function usageBarColor(pct: number): string {
  if (pct >= 90) return "bg-red-500";
  if (pct >= 70) return "bg-amber-500";
  return "bg-emerald-500";
}

// 单行用量进度条
function UsageBar({
  label,
  pct,
  resetAt,
  detail,
}: {
  label: string;
  pct: number;
  resetAt?: string;
  detail?: AccountRow["usage_5h_detail"];
}) {
  const resetText = formatResetAt(resetAt);
  const { t } = useTranslation();
  const detailText = hasUsageWindowDetail(detail)
    ? `${formatCompactUsageNumber(detail?.requests)} ${t("accounts.usageReqUnit")} / ${formatCompactUsageNumber(detail?.tokens)} ${t("accounts.usageTokUnit")}`
    : "";
  return (
    <div>
      <div className="flex items-center gap-1.5">
        <span className="text-[11px] font-medium text-muted-foreground w-5 shrink-0">
          {label}
        </span>
        <div className="flex-1 h-1.5 rounded-full bg-muted overflow-hidden min-w-[72px]">
          <div
            className={`h-full rounded-full transition-all ${usageBarColor(pct)}`}
            style={{ width: `${Math.min(100, pct)}%` }}
          />
        </div>
        <span className="text-[12px] font-semibold w-[42px] text-right shrink-0">
          {pct.toFixed(1)}%
        </span>
      </div>
      {detailText && (
        <div className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[26px]">
          {detailText}
        </div>
      )}
      {resetText && (
        <div className="text-[11px] font-medium text-muted-foreground mt-0.5 pl-[26px]">
          ⏱ {resetText}
        </div>
      )}
    </div>
  );
}

function UsageWindowStat({
  label,
  detail,
}: {
  label: string;
  detail?: AccountRow["usage_5h_detail"];
}) {
  const { t } = useTranslation();
  if (!detail || !hasUsageWindowDetail(detail)) return null;

  const accountBilledText =
    typeof detail.account_billed === "number"
      ? detail.account_billed.toFixed(4)
      : "";
  const userBilledText =
    typeof detail.user_billed === "number" ? detail.user_billed.toFixed(4) : "";

  return (
    <div className="flex flex-col gap-0.5">
      <div className="flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
        <span className="w-5 shrink-0">{label}</span>
        <span>
          {formatCompactUsageNumber(detail?.requests)}{" "}
          {t("accounts.usageReqUnit")} /{" "}
          {formatCompactUsageNumber(detail?.tokens)}{" "}
          {t("accounts.usageTokUnit")}
        </span>
      </div>
      {(accountBilledText || userBilledText) && (
        <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground/80 pl-6">
          {accountBilledText && (
            <span>
              {t("accounts.accountBilledLabel")}: ${accountBilledText}
            </span>
          )}
          {userBilledText && (
            <span>
              {t("accounts.userBilledLabel")}: ${userBilledText}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

// 用量列组件
function UsageCell({ account }: { account: AccountRow }) {
  const plan = normalizePlanType(account.plan_type);
  const has7d =
    account.usage_percent_7d !== null && account.usage_percent_7d !== undefined;
  const has5h =
    account.usage_percent_5h !== null && account.usage_percent_5h !== undefined;
  const has7dDetail = hasUsageWindowDetail(account.usage_7d_detail);
  const has5hDetail = hasUsageWindowDetail(account.usage_5h_detail);

  if (plan === "free") {
    if (!has7d && !has7dDetail)
      return <span className="text-[12px] text-muted-foreground">-</span>;
    return (
      <div className="w-48">
        {has7d ? (
          <UsageBar
            label="7d"
            pct={account.usage_percent_7d!}
            resetAt={account.reset_7d_at}
            detail={account.usage_7d_detail}
          />
        ) : (
          <UsageWindowStat label="7d" detail={account.usage_7d_detail} />
        )}
      </div>
    );
  }

  if (
    plan === "pro" ||
    plan === "team" ||
    plan === "plus" ||
    plan === "teamplus"
  ) {
    if (!has5h && !has7d && !has5hDetail && !has7dDetail)
      return <span className="text-[12px] text-muted-foreground">-</span>;
    return (
      <div className="w-52 space-y-1.5">
        {has5h ? (
          <UsageBar
            label="5h"
            pct={account.usage_percent_5h!}
            resetAt={account.reset_5h_at}
            detail={account.usage_5h_detail}
          />
        ) : (
          <UsageWindowStat label="5h" detail={account.usage_5h_detail} />
        )}
        {has7d ? (
          <UsageBar
            label="7d"
            pct={account.usage_percent_7d!}
            resetAt={account.reset_7d_at}
            detail={account.usage_7d_detail}
          />
        ) : (
          <UsageWindowStat label="7d" detail={account.usage_7d_detail} />
        )}
      </div>
    );
  }

  if (has7d || has7dDetail) {
    return (
      <div className="w-48">
        {has7d ? (
          <UsageBar
            label="7d"
            pct={account.usage_percent_7d!}
            resetAt={account.reset_7d_at}
            detail={account.usage_7d_detail}
          />
        ) : (
          <UsageWindowStat label="7d" detail={account.usage_7d_detail} />
        )}
      </div>
    );
  }
  return <span className="text-[13px] text-muted-foreground">-</span>;
}

function BilledCell({ account }: { account: AccountRow }) {
  const h5 = typeof account.billed_5h === "number" ? account.billed_5h.toFixed(2) : null;
  const d7 = typeof account.billed_7d === "number" ? account.billed_7d.toFixed(2) : null;
  if (h5 === null && d7 === null) return <span className="text-[12px] text-muted-foreground">-</span>;
  return (
    <span className="text-[12px] text-muted-foreground">
      {h5 !== null ? `5h: $${h5}` : "5h: -"}
      {" / "}
      {d7 !== null ? `7d: $${d7}` : "7d: -"}
    </span>
  );
}

function getAccountStatusCountdownUntil(
  account: AccountRow,
): string | undefined {
  const status = account.status;
  if (
    account.cooldown_until &&
    (status === "rate_limited" || status === "error" || status === "cooldown")
  ) {
    return account.cooldown_until;
  }
  if (status === "usage_exhausted") {
    return account.reset_7d_at;
  }
  return undefined;
}

function AccountStatusCountdown({ account }: { account: AccountRow }) {
  const until = getAccountStatusCountdownUntil(account);
  if (!until) return null;
  return <CooldownTimer until={until} />;
}

// 冷却倒计时组件
function CooldownTimer({ until }: { until: string }) {
  const [remaining, setRemaining] = useState("");

  useEffect(() => {
    const target = new Date(until).getTime();

    const update = () => {
      const diff = Math.max(0, target - Date.now());
      if (diff <= 0) {
        setRemaining("");
        return;
      }
      const h = Math.floor(diff / 3600000);
      const m = Math.floor((diff % 3600000) / 60000);
      const s = Math.floor((diff % 60000) / 1000);
      if (h > 0) {
        setRemaining(
          `${h}h ${String(m).padStart(2, "0")}m ${String(s).padStart(2, "0")}s`,
        );
      } else if (m > 0) {
        setRemaining(`${m}m ${String(s).padStart(2, "0")}s`);
      } else {
        setRemaining(`${s}s`);
      }
    };

    update();
    const id = setInterval(update, 1000);
    return () => clearInterval(id);
  }, [until]);

  if (!remaining) return null;
  return (
    <span className="inline-flex h-6 min-w-[112px] shrink-0 items-center justify-center gap-1.5 rounded-full bg-amber-50 px-2 text-[11px] font-mono leading-none tabular-nums text-amber-700 ring-1 ring-inset ring-amber-200/70 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20">
      <Hourglass className="size-3 shrink-0" aria-hidden="true" />
      {remaining}
    </span>
  );
}
