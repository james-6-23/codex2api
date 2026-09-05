import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ChangeEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  CircleGauge,
  Download,
  ExternalLink,
  FileJson,
  FlaskConical,
  FolderOpen,
  KeyRound,
  Link2,
  Loader2,
  Pencil,
  Plus,
  Power,
  PowerOff,
  RotateCw,
  Search,
  ShieldCheck,
  ShieldQuestion,
  ShieldX,
  TriangleAlert,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import { api } from "../api";
import type { ProxyRow } from "../api";
import { ProxyField } from "../components/ProxyField";
import type {
  AccountGroup,
  AccountListSummary,
  AccountRow,
  AntigravityAuthKind,
  AntigravityAccountState,
  AntigravityImportItem,
  AntigravityImportResponse,
  AntigravityModelQuota,
  AntigravityOAuthStartResponse,
  AntigravityOAuthStatusResponse,
  AntigravityPermissionsSnapshot,
  AntigravityQuotaSnapshot,
  UpdateAntigravityAccountRequest,
} from "../types";
import AccountGroupFilterSelect, {
  EMPTY_ACCOUNT_GROUP_FILTER,
  isAccountGroupFilterEmpty,
  pruneAccountGroupFilter,
  type AccountGroupFilterValue,
} from "../components/AccountGroupFilterSelect";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import AccountProxyBadge from "../components/AccountProxyBadge";
import AccountProxyQuickEditor from "../components/AccountProxyQuickEditor";
import {
  buildProxyBindingContext,
  type ProxyBindingContext,
} from "../lib/accountProxyBinding";
import ChannelLogo from "../components/ChannelLogo";
import { CompactStat } from "../components/CompactStat";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import Pagination from "../components/Pagination";
import StateShell from "../components/StateShell";
import StatusBadge from "../components/StatusBadge";
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
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import {
  DEFAULT_PAGE_SIZE_OPTIONS,
  usePersistedPageSize,
} from "../hooks/usePersistedPageSize";
import { useToast } from "../hooks/useToast";
import { cn } from "@/lib/utils";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";

type StatusFilter = "all" | "active" | "disabled" | "error";
type ImportMode = "single" | "files";
type BusyAction = "refresh" | "quota" | "toggle" | "delete";
type OAuthModalStatus = "idle" | "starting" | "waiting" | "processing" | "completed" | "failed" | "cancelled";

const ANTIGRAVITY_DEFAULT_MODELS = [
  "gemini-3-pro-preview",
  "gemini-2.5-pro",
  "gemini-2.5-flash",
];

function parseModelList(value: string): string[] {
  return Array.from(
    new Set(
      value
        .split(/[\n,]/)
        .map((model) => model.trim())
        .filter(Boolean),
    ),
  );
}

function editDraftFromAccount(account: AccountRow): EditDraft {
  const authKind: AntigravityAuthKind =
    account.antigravity_auth_kind === "api_key" ? "api_key" : "oauth";
  return {
    name: account.name ?? "",
    authKind,
    authJson: "",
    apiKey: "",
    models: (account.models?.length
      ? account.models
      : authKind === "api_key"
        ? ANTIGRAVITY_DEFAULT_MODELS
        : []
    ).join("\n"),
    modelMapping: account.model_mapping ?? "",
    proxyUrl: account.proxy_url ?? "",
    groupIds: account.group_ids ?? [],
  };
}

function importItemSourceLabel(item: AntigravityImportItem): string {
  const suffix = item.sub_index && item.sub_index > 0 ? `.${item.sub_index}` : "";
  return `#${item.index}${suffix}`;
}

function importItemDisplayLabel(item: AntigravityImportItem): string {
  const source = importItemSourceLabel(item);
  return item.email ? `${item.email} (${source})` : source;
}

interface CredentialFile {
  name: string;
  content: string;
}

interface ImportDraft {
  name: string;
  authKind: AntigravityAuthKind;
  authJson: string;
  apiKey: string;
  models: string;
  modelMapping: string;
  proxyUrl: string;
  groupIds: number[];
  // importFileProxies 只控制"文件内代理是否注册进代理表"。该渠道的导入一直会采用
  // 文件里的 proxy_url，关掉它只是让那些代理停在账号绑定上、不进代理池。
  importFileProxies: boolean;
}

interface EditDraft {
  name: string;
  authKind: AntigravityAuthKind;
  authJson: string;
  apiKey: string;
  models: string;
  modelMapping: string;
  proxyUrl: string;
  groupIds: number[];
}

interface OAuthDraft {
  name: string;
  proxyUrl: string;
  groupIds: number[];
}

const EMPTY_IMPORT_DRAFT: ImportDraft = {
  name: "",
  authKind: "oauth",
  authJson: "",
  apiKey: "",
  models: ANTIGRAVITY_DEFAULT_MODELS.join("\n"),
  modelMapping: "",
  proxyUrl: "",
  groupIds: [],
  importFileProxies: false,
};

const EMPTY_OAUTH_DRAFT: OAuthDraft = {
  name: "",
  proxyUrl: "",
  groupIds: [],
};

function accountLabel(account: AccountRow): string {
  return account.name || account.email || `#${account.id}`;
}

function identityProjectID(account: AccountRow): string {
  return account.project_id || account.antigravity_project_id || "";
}

function identityAvatarURL(account: AccountRow): string {
  return account.avatar_url || account.antigravity_avatar_url || "";
}

function identityVerified(account: AccountRow): boolean {
  return account.verified_email ?? account.antigravity_verified_email ?? false;
}

function toRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

function firstString(record: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const value = record[key];
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function normalizeFraction(value: unknown): number | null {
  if (typeof value !== "number" || !Number.isFinite(value)) return null;
  const fraction = value > 1 && value <= 100 ? value / 100 : value;
  return Math.min(1, Math.max(0, fraction));
}

function quotaRemaining(info: AntigravityModelQuota): number | null {
  const record = toRecord(info);
  return normalizeFraction(
    record.remaining_fraction ?? record.remainingFraction ?? record.percentage,
  );
}

function quotaResetTime(info: AntigravityModelQuota): string {
  const record = toRecord(info);
  return firstString(record, ["reset_time", "resetTime"]);
}

function quotaDisplayName(model: string, info: AntigravityModelQuota): string {
  const record = toRecord(info);
  return firstString(record, ["display_name", "displayName"]) || model;
}

function modelQuotaEntries(
  quota?: AntigravityQuotaSnapshot,
): Array<[string, AntigravityModelQuota]> {
  if (!quota?.models) return [];
  if (Array.isArray(quota.models)) {
    return quota.models.map((info, index) => {
      const record = toRecord(info);
      const model =
        firstString(record, ["model", "model_id", "modelId", "name", "id"]) ||
        `model-${index + 1}`;
      return [model, info];
    });
  }
  return Object.entries(quota.models);
}

function sortedModelQuotaEntries(
  quota?: AntigravityQuotaSnapshot,
): Array<[string, AntigravityModelQuota]> {
  return modelQuotaEntries(quota).sort(([leftName, left], [rightName, right]) => {
    if (Boolean(left.recommended) !== Boolean(right.recommended)) {
      return left.recommended ? -1 : 1;
    }
    return quotaDisplayName(leftName, left).localeCompare(
      quotaDisplayName(rightName, right),
    );
  });
}

function quotaUpdatedAt(quota?: AntigravityQuotaSnapshot): string {
  const record = toRecord(quota);
  return firstString(record, ["updated_at", "updatedAt"]);
}

function quotaGroups(
  quota?: AntigravityQuotaSnapshot,
): NonNullable<AntigravityQuotaSnapshot["quota_groups"]> {
  const record = toRecord(quota);
  const groups = record.quota_groups ?? record.quotaGroups ?? record.groups;
  return Array.isArray(groups)
    ? (groups as NonNullable<AntigravityQuotaSnapshot["quota_groups"]>)
    : [];
}

function subscriptionTier(account: AccountRow): string {
  const quota = toRecord(account.antigravity_quota);
  return (
    firstString(quota, ["subscription_tier", "subscriptionTier"]) ||
    account.plan_type ||
    ""
  );
}

function permissionAllowed(
  permissions?: AntigravityPermissionsSnapshot,
): boolean | null {
  if (!permissions) return null;
  const record = toRecord(permissions);
  const value = record.allowed ?? record.allow_access ?? record.allowAccess;
  return typeof value === "boolean" ? value : null;
}

function tierLabel(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  const record = toRecord(value);
  const label = firstString(record, [
    "display_name",
    "displayName",
    "name",
    "label",
    "tier",
    "id",
    "key",
    "reason_code",
  ]);
  if (label) return label;
  if (!value) return "";
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function resolveGroups(account: AccountRow, groups: AccountGroup[]): AccountGroup[] {
  const ids = account.group_ids ?? [];
  if (ids.length === 0) return [];
  const byID = new Map(groups.map((group) => [group.id, group]));
  return ids.map((id) => byID.get(id)).filter(Boolean) as AccountGroup[];
}

function quotaTone(fraction: number | null): string {
  if (fraction == null) return "bg-muted-foreground";
  if (fraction <= 0.1) return "bg-red-500";
  if (fraction <= 0.35) return "bg-amber-500";
  return "bg-emerald-500";
}

function formatPercent(fraction: number | null): string {
  if (fraction == null) return "-";
  return `${Math.round(fraction * 100)}%`;
}

function AccountAvatar({ account, size = 36 }: { account: AccountRow; size?: number }) {
  const [failed, setFailed] = useState(false);
  const avatar = identityAvatarURL(account);
  if (avatar && !failed) {
    return (
      <img
        src={avatar}
        alt=""
        width={size}
        height={size}
        referrerPolicy="no-referrer"
        className="shrink-0 rounded-full border border-border bg-background object-cover"
        style={{ width: size, height: size }}
        onError={() => setFailed(true)}
      />
    );
  }
  return (
    <span
      className="inline-flex shrink-0 items-center justify-center rounded-full border border-border bg-background"
      style={{ width: size, height: size }}
    >
      <ChannelLogo channel="antigravity" size={Math.round(size * 0.58)} />
    </span>
  );
}

const warningURLPattern = /https?:\/\/[^\s;]+/;

function firstWarningURL(text?: string): string | null {
  if (!text) return null;
  const match = text.match(warningURLPattern);
  return match ? match[0] : null;
}

// 权限列里的操作入口:同步 warning 会带出 Google 的一次性操作链接
// (TOS 申诉表单 / 账号验证),直接渲染成可点击入口,免得去详情页
// 复制长 URL。
function SyncWarningAction({ warning }: { warning?: string }) {
  const { t } = useTranslation();
  const url = firstWarningURL(warning);
  if (!url) return null;
  const label = /appeal/i.test(warning ?? "")
    ? t("antigravity.submitAppeal")
    : t("antigravity.verifyAccount");
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      className="mt-1 inline-flex max-w-[180px] items-center gap-1 text-[11px] font-medium text-primary hover:underline"
    >
      <ExternalLink className="size-3 shrink-0" aria-hidden />
      <span className="truncate">{label}</span>
    </a>
  );
}

// 详情页 warning 横幅:URL 转成可点击链接,展示文本截短避免长链接刷屏。
function LinkifiedWarning({ text }: { text: string }) {
  const parts = text.split(/(https?:\/\/[^\s;]+)/g);
  return (
    <span className="break-words">
      {parts.map((part, index) => {
        if (!/^https?:\/\//.test(part)) {
          return part;
        }
        const display = part.length > 64 ? part.slice(0, 61) + "…" : part;
        return (
          <a
            key={index}
            href={part}
            target="_blank"
            rel="noopener noreferrer"
            className="font-medium underline underline-offset-2"
          >
            {display}
          </a>
        );
      })}
    </span>
  );
}

function PermissionBadge({ permissions }: { permissions?: AntigravityPermissionsSnapshot }) {
  const { t } = useTranslation();
  const allowed = permissionAllowed(permissions);
  if (allowed == null) {
    return (
      <Badge variant="outline" className="gap-1 text-muted-foreground">
        <ShieldQuestion className="size-3" />
        {t("antigravity.permissionUnknown")}
      </Badge>
    );
  }
  if (allowed) {
    return (
      <Badge className="gap-1 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">
        <ShieldCheck className="size-3" />
        {t("antigravity.permissionAllowed")}
      </Badge>
    );
  }
  return (
    <Badge variant="destructive" className="gap-1">
      <ShieldX className="size-3" />
      {t("antigravity.permissionBlocked")}
    </Badge>
  );
}

function GroupChips({ account, groups }: { account: AccountRow; groups: AccountGroup[] }) {
  const resolved = resolveGroups(account, groups);
  if (resolved.length === 0) return null;
  return (
    <div className="mt-1 flex flex-wrap gap-1">
      {resolved.slice(0, 3).map((group) => (
        <span
          key={group.id}
          className="inline-flex max-w-28 items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground"
          title={group.description || group.name}
        >
          <span
            className="size-1.5 shrink-0 rounded-full"
            style={{ backgroundColor: group.color || "#2563eb" }}
          />
          <span className="truncate">{group.name}</span>
        </span>
      ))}
      {resolved.length > 3 ? (
        <span className="rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
          +{resolved.length - 3}
        </span>
      ) : null}
    </div>
  );
}

function AccountMetadataFields({
  proxyUrl,
  onProxyUrlChange,
  proxies = [],
  groupIds,
  onGroupIdsChange,
  groups,
  onCreateGroup,
}: {
  proxyUrl: string;
  onProxyUrlChange: (value: string) => void;
  proxies?: ProxyRow[];
  groupIds: number[];
  onGroupIdsChange: (value: number[]) => void;
  groups: AccountGroup[];
  onCreateGroup: (name: string) => Promise<number | null>;
}) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      {/* 代理字段与其他三个渠道同构:手填 + 测试 + 从代理池选择(含关联提示)。 */}
      <ProxyField
        className="space-y-1.5"
        value={proxyUrl}
        onChange={onProxyUrlChange}
        proxies={proxies}
        label={t("antigravity.proxyUrl")}
        placeholder={t("antigravity.proxyUrlPlaceholder")}
      />
      <div className="space-y-1.5">
        <span className="text-xs font-semibold text-muted-foreground">
          {t("accounts.groupsLabel")}
        </span>
        <AccountGroupMultiSelect
          groups={groups}
          value={groupIds}
          onChange={onGroupIdsChange}
          placeholder={t("accounts.groupsPlaceholder")}
          emptyLabel={t("accounts.groupsNone")}
          emptyHint={t("accounts.groupsSelectHint")}
          selectedLabel={t("accounts.groupsSelected", { count: groupIds.length })}
          onCreateGroup={onCreateGroup}
          createLabel={t("accounts.groupCreate")}
          createPlaceholder={t("accounts.groupNamePlaceholder")}
          creatingLabel={t("accounts.groupCreating")}
          createEmptyHint={t("accounts.groupCreateInlineEmptyHint")}
        />
      </div>
    </div>
  );
}

function CompactQuota({ account, onOpen }: { account: AccountRow; onOpen: () => void }) {
  const { t } = useTranslation();
  const entries = modelQuotaEntries(account.antigravity_quota);
  const remaining = entries
    .map(([, info]) => quotaRemaining(info))
    .filter((value): value is number => value != null);
  if (entries.length === 0) {
    return (
      <button
        type="button"
        onClick={onOpen}
        className="text-left text-xs text-muted-foreground hover:text-foreground"
      >
        {t("antigravity.quotaNotSynced")}
      </button>
    );
  }
  const lowest = remaining.length > 0 ? Math.min(...remaining) : null;
  return (
    <button
      type="button"
      onClick={onOpen}
      className="group min-w-[150px] text-left"
      title={t("antigravity.viewDetails")}
    >
      <div className="flex items-center justify-between gap-2 text-xs">
        <span className="font-semibold text-foreground">
          {t("antigravity.modelsCount", { count: entries.length })}
        </span>
        <span className="font-mono font-semibold tabular-nums text-muted-foreground group-hover:text-foreground">
          {formatPercent(lowest)}
        </span>
      </div>
      <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
        <div
          className={cn("h-full rounded-full", quotaTone(lowest))}
          style={{ width: `${Math.max(2, (lowest ?? 0) * 100)}%` }}
        />
      </div>
      <div className="mt-1 text-[10px] text-muted-foreground">
        {t("antigravity.lowestRemaining")}
      </div>
    </button>
  );
}

function QuotaDetail({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const models = sortedModelQuotaEntries(account.antigravity_quota);
  const groups = quotaGroups(account.antigravity_quota);
  const permissions = account.antigravity_permissions;
  const allowed = permissionAllowed(permissions);
  const permissionRecord = toRecord(permissions);
  const currentTier = tierLabel(
    permissionRecord.current_tier ?? permissionRecord.currentTier,
  );
  const paidTier = tierLabel(permissionRecord.paid_tier ?? permissionRecord.paidTier);
  const allowedTiersRaw = permissionRecord.allowed_tiers ?? permissionRecord.allowedTiers;
  const ineligibleTiersRaw =
    permissionRecord.ineligible_tiers ?? permissionRecord.ineligibleTiers;
  const allowedTiers = Array.isArray(allowedTiersRaw)
    ? allowedTiersRaw.map(tierLabel).filter(Boolean)
    : [];
  const ineligibleTiers = Array.isArray(ineligibleTiersRaw)
    ? ineligibleTiersRaw.map(tierLabel).filter(Boolean)
    : [];
  const reason = firstString(permissionRecord, ["reason"]);
  const updatedAt = quotaUpdatedAt(account.antigravity_quota);

  return (
    <div className="space-y-6">
      {account.antigravity_sync_warning ? (
        <div className="flex items-start gap-2 rounded-md border border-amber-300/70 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />
          <LinkifiedWarning text={account.antigravity_sync_warning} />
        </div>
      ) : null}
      <section className="grid gap-3 sm:grid-cols-2">
        <div>
          <div className="text-[11px] font-semibold uppercase text-muted-foreground">
            {t("antigravity.projectId")}
          </div>
          <div className="mt-1 break-all font-mono text-sm text-foreground">
            {identityProjectID(account) || "-"}
          </div>
        </div>
        <div>
          <div className="text-[11px] font-semibold uppercase text-muted-foreground">
            {t("antigravity.subscriptionTier")}
          </div>
          <div className="mt-1 text-sm font-semibold text-foreground">
            {subscriptionTier(account) || currentTier || "-"}
          </div>
        </div>
      </section>

      <section className="border-t border-border pt-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 className="text-sm font-semibold text-foreground">
              {t("antigravity.permissionsTitle")}
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {allowed === false && reason
                ? reason
                : t("antigravity.permissionsDescription")}
            </p>
          </div>
          <PermissionBadge permissions={permissions} />
        </div>
        {(currentTier || paidTier) && (
          <div className="mt-3 flex flex-wrap gap-2 text-xs">
            {currentTier ? (
              <Badge variant="outline">
                {t("antigravity.currentTier")}: {currentTier}
              </Badge>
            ) : null}
            {paidTier ? (
              <Badge variant="outline">
                {t("antigravity.paidTier")}: {paidTier}
              </Badge>
            ) : null}
          </div>
        )}
        {allowedTiers.length > 0 ? (
          <div className="mt-3">
            <div className="text-[11px] font-semibold uppercase text-muted-foreground">
              {t("antigravity.allowedTiers")}
            </div>
            <div className="mt-1.5 flex flex-wrap gap-1.5">
              {allowedTiers.map((tier, index) => (
                <Badge key={`${tier}-${index}`} variant="outline">
                  {tier}
                </Badge>
              ))}
            </div>
          </div>
        ) : null}
        {ineligibleTiers.length > 0 ? (
          <div className="mt-3">
            <div className="text-[11px] font-semibold uppercase text-muted-foreground">
              {t("antigravity.ineligibleTiers")}
            </div>
            <div className="mt-1.5 flex flex-wrap gap-1.5">
              {ineligibleTiers.map((tier, index) => (
                <Badge key={`${tier}-${index}`} variant="outline" className="text-muted-foreground">
                  {tier}
                </Badge>
              ))}
            </div>
          </div>
        ) : null}
      </section>

      <section className="border-t border-border pt-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h3 className="text-sm font-semibold text-foreground">
              {t("antigravity.modelQuotaTitle")}
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {updatedAt
                ? t("antigravity.updatedAt", {
                    time: formatRelativeTime(updatedAt, { variant: "compact" }),
                  })
                : t("antigravity.quotaNotSynced")}
            </p>
          </div>
          <Badge variant="secondary">
            {t("antigravity.modelsCount", { count: models.length })}
          </Badge>
        </div>
        {models.length === 0 ? (
          <div className="mt-4 rounded-lg border border-dashed border-border p-5 text-center text-sm text-muted-foreground">
            {t("antigravity.quotaNotSynced")}
          </div>
        ) : (
          <div className="mt-3 divide-y divide-border rounded-lg border border-border">
            {models.map(([model, info]) => {
              const fraction = quotaRemaining(info);
              const resetAt = quotaResetTime(info);
              return (
                <div
                  key={model}
                  className="grid gap-2 px-3 py-3 sm:grid-cols-[minmax(0,1fr)_180px] sm:items-center"
                >
                  <div className="min-w-0">
                    <div className="flex min-w-0 items-center gap-2">
                      <span className="truncate text-sm font-semibold text-foreground">
                        {quotaDisplayName(model, info)}
                      </span>
                      {info.recommended ? (
                        <Badge variant="secondary" className="text-[10px]">
                          {t("antigravity.recommended")}
                        </Badge>
                      ) : null}
                    </div>
                    <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
                      {info.supports_images ? <span>{t("antigravity.images")}</span> : null}
                      {info.supports_thinking ? <span>{t("antigravity.thinking")}</span> : null}
                      {info.max_output_tokens ? (
                        <span>
                          {t("antigravity.maxOutput", {
                            count: info.max_output_tokens,
                          })}
                        </span>
                      ) : null}
                    </div>
                  </div>
                  <div>
                    <div className="flex items-center justify-between text-xs">
                      <span className="text-muted-foreground">
                        {resetAt
                          ? t("antigravity.resetAt", {
                              time: formatBeijingTime(resetAt),
                            })
                          : t("antigravity.resetUnknown")}
                      </span>
                      <span className="font-mono font-semibold tabular-nums text-foreground">
                        {formatPercent(fraction)}
                      </span>
                    </div>
                    <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
                      <div
                        className={cn("h-full rounded-full", quotaTone(fraction))}
                        style={{ width: `${Math.max(2, (fraction ?? 0) * 100)}%` }}
                      />
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>

      {groups.length > 0 ? (
        <section className="border-t border-border pt-4">
          <h3 className="text-sm font-semibold text-foreground">
            {t("antigravity.groupQuotaTitle")}
          </h3>
          <div className="mt-3 space-y-3">
            {groups.map((group, groupIndex) => (
              <div key={`${group.display_name}-${groupIndex}`} className="rounded-lg border border-border p-3">
                <div className="text-sm font-semibold text-foreground">
                  {group.display_name || t("antigravity.unnamedQuotaGroup")}
                </div>
                {group.description ? (
                  <div className="mt-0.5 text-xs text-muted-foreground">
                    {group.description}
                  </div>
                ) : null}
                <div className="mt-3 space-y-2">
                  {(group.buckets ?? []).map((bucket) => {
                    const fraction = normalizeFraction(bucket.remaining_fraction);
                    return (
                      <div
                        key={bucket.bucket_id}
                        className="grid gap-1.5 sm:grid-cols-[minmax(0,1fr)_180px] sm:items-center"
                      >
                        <div className="min-w-0 text-xs">
                          <span className="font-medium text-foreground">
                            {bucket.display_name || bucket.bucket_id}
                          </span>
                          <span className="ml-2 text-muted-foreground">{bucket.window}</span>
                        </div>
                        <div className="flex items-center gap-2">
                          <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
                            <div
                              className={cn("h-full rounded-full", quotaTone(fraction))}
                              style={{ width: `${Math.max(2, (fraction ?? 0) * 100)}%` }}
                            />
                          </div>
                          <span className="w-10 text-right font-mono text-xs font-semibold tabular-nums">
                            {formatPercent(fraction)}
                          </span>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            ))}
          </div>
        </section>
      ) : null}
    </div>
  );
}

function AntigravityManagementState({
  state,
  loading,
  error,
  action,
  onSync,
  onProbe,
}: {
  state: AntigravityAccountState | null;
  loading: boolean;
  error: string | null;
  action: "sync" | "probe" | null;
  onSync: () => void;
  onProbe: () => void;
}) {
  const { t } = useTranslation();
  const latest = state?.capabilities?.[0];
  return (
    <section className="space-y-3 rounded-xl border border-border bg-card p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold">{t("antigravity.stateTitle")}</h3>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            {t("antigravity.stateDescription")}
          </p>
        </div>
        <div className="flex items-center gap-1.5">
          <Button variant="outline" size="xs" disabled={loading || action !== null} onClick={onSync}>
            <RotateCw className={cn("size-3", action === "sync" && "animate-spin")} />
            {action === "sync" ? t("antigravity.stateSyncing") : t("antigravity.stateSync")}
          </Button>
          <Button variant="outline" size="xs" disabled={loading || action !== null} onClick={onProbe}>
            <FlaskConical className={cn("size-3", action === "probe" && "animate-pulse")} />
            {action === "probe" ? t("antigravity.capabilityProbing") : t("antigravity.capabilityProbe")}
          </Button>
        </div>
      </div>
      {loading ? (
        <div className="flex items-center gap-2 py-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" />{t("antigravity.stateLoading")}
        </div>
      ) : null}
      {error ? <div className="rounded-lg bg-destructive/10 px-3 py-2 text-xs text-destructive">{error}</div> : null}
      {state ? (
        <>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
            {[
              [t("antigravity.stateCredential"), state.credential_kind === "api_key" ? t("antigravity.authKindApiKey") : t("antigravity.authKindOAuth")],
              [t("antigravity.stateIdentity"), state.identity.status],
              [t("antigravity.stateCatalog"), `${state.catalog.models.length} · ${state.catalog.source}`],
              [t("antigravity.stateVerification"), state.catalog.verified ? t("antigravity.verified") : t("antigravity.unverified")],
            ].map(([label, value]) => (
              <div key={label} className="min-w-0 rounded-lg bg-muted/35 px-2.5 py-2">
                <div className="text-[10px] text-muted-foreground">{label}</div>
                <div className="mt-0.5 truncate text-xs font-semibold" title={value}>{value}</div>
              </div>
            ))}
          </div>
          <div className="rounded-lg border border-border px-3 py-2 text-xs">
            <div className="flex items-center justify-between gap-2">
              <span className="font-semibold">{t("antigravity.capabilityTitle")}</span>
              <Badge variant={latest?.verified ? "default" : "outline"}>
                {latest?.verified ? t("antigravity.verified") : t("antigravity.notProbed")}
              </Badge>
            </div>
            <div className="mt-1 text-[11px] text-muted-foreground">
              {latest
                ? `${latest.protocol} · ${latest.model_id} · ${latest.status}${latest.http_status ? ` · HTTP ${latest.http_status}` : ""}`
                : t("antigravity.capabilityEmpty")}
            </div>
          </div>
          {state.warnings.length > 0 ? (
            <div className="space-y-1 rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-[11px] text-amber-700 dark:text-amber-300">
              {state.warnings.map((warning) => <div key={warning}>{warning}</div>)}
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

function AntigravityAccounts({ headerSlot }: { headerSlot?: ReactNode } = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const requestAbortRef = useRef<AbortController | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [allGroups, setAllGroups] = useState<AccountGroup[]>([]);
  const antigravityGroups = useMemo(
    () => allGroups.filter((group) => group.channel === "antigravity"),
    [allGroups],
  );
  const [serverSummary, setServerSummary] = useState<AccountListSummary | null>(null);
  const [totalAccounts, setTotalAccounts] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize(
    "antigravity-accounts",
    20,
    DEFAULT_PAGE_SIZE_OPTIONS,
  );
  const [searchQuery, setSearchQuery] = useState("");
  const [debouncedSearchQuery, setDebouncedSearchQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [groupFilter, setGroupFilter] = useState<AccountGroupFilterValue>(
    EMPTY_ACCOUNT_GROUP_FILTER,
  );
  const [busy, setBusy] = useState<{ id: number; action: BusyAction } | null>(null);

  // 代理池 + 代理池开关 + 全局代理:代理徽章的判定输入。本页此前只有纯文本代理
  // 输入框,没接过代理池,这里补上(拉取失败静默留空,不影响手填)。
  const [proxyPool, setProxyPool] = useState<ProxyRow[]>([]);
  const [proxyPoolEnabled, setProxyPoolEnabled] = useState(false);
  const [globalProxyURL, setGlobalProxyURL] = useState("");
  const [quickProxyAccount, setQuickProxyAccount] = useState<AccountRow | null>(
    null,
  );
  useEffect(() => {
    let cancelled = false;
    void api
      .listProxies()
      .then((res) => {
        if (!cancelled) setProxyPool(res.proxies ?? []);
      })
      .catch(() => {
        if (!cancelled) setProxyPool([]);
      });
    void api
      .getSettings()
      .then((settings) => {
        if (cancelled) return;
        setProxyPoolEnabled(Boolean(settings.proxy_pool_enabled));
        setGlobalProxyURL((settings.proxy_url ?? "").trim());
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);
  // 分组用全量而非 antigravityGroups:后端解析组代理不看渠道,按渠道过滤会把
  // 跨渠道的存量成员误报成"无组代理"。
  const proxyBindingCtx = useMemo<ProxyBindingContext>(
    () =>
      buildProxyBindingContext({
        proxies: proxyPool,
        groups: allGroups,
        poolEnabled: proxyPoolEnabled,
        globalProxy: globalProxyURL,
      }),
    [proxyPool, allGroups, proxyPoolEnabled, globalProxyURL],
  );

  const [showImport, setShowImport] = useState(false);
  const [importMode, setImportMode] = useState<ImportMode>("single");
  const [importDraft, setImportDraft] = useState<ImportDraft>(EMPTY_IMPORT_DRAFT);
  const [credentialFiles, setCredentialFiles] = useState<CredentialFile[]>([]);
  const [importing, setImporting] = useState(false);
  const [importResult, setImportResult] = useState<AntigravityImportResponse | null>(null);

  const [showOAuth, setShowOAuth] = useState(false);
  const [oauthDraft, setOAuthDraft] = useState<OAuthDraft>(EMPTY_OAUTH_DRAFT);
  const [oauthSession, setOAuthSession] = useState<AntigravityOAuthStartResponse | null>(null);
  const [oauthStatus, setOAuthStatus] = useState<AntigravityOAuthStatusResponse | null>(null);
  const [oauthCallbackURL, setOAuthCallbackURL] = useState("");
  const [oauthModalStatus, setOAuthModalStatus] = useState<OAuthModalStatus>("idle");
  const [oauthStarting, setOAuthStarting] = useState(false);
  const [oauthCompleting, setOAuthCompleting] = useState(false);
  const [oauthCancelling, setOAuthCancelling] = useState(false);
  const oauthPollTimerRef = useRef<number | null>(null);
  const oauthPollInFlightRef = useRef(false);
  const oauthPollGenerationRef = useRef(0);

  const [editingAccount, setEditingAccount] = useState<AccountRow | null>(null);
  const [editDraft, setEditDraft] = useState<EditDraft>({
    name: "",
    authKind: "oauth",
    authJson: "",
    apiKey: "",
    models: "",
    modelMapping: "",
    proxyUrl: "",
    groupIds: [],
  });
  const [editing, setEditing] = useState(false);
  const [editingDetailsLoading, setEditingDetailsLoading] = useState(false);
  const [editingDetailsError, setEditingDetailsError] = useState<string | null>(null);
  const editLoadGenerationRef = useRef(0);
  const [detailAccountId, setDetailAccountId] = useState<number | null>(null);
  const [managementState, setManagementState] = useState<AntigravityAccountState | null>(null);
  const [managementLoading, setManagementLoading] = useState(false);
  const [managementError, setManagementError] = useState<string | null>(null);
  const [managementAction, setManagementAction] = useState<"sync" | "probe" | null>(null);
  const detailAccountIdRef = useRef<number | null>(null);
  const detailGenerationRef = useRef(0);
  const managementAbortRef = useRef<AbortController | null>(null);
  const [exporting, setExporting] = useState(false);
  const detailAccount = useMemo(
    () => accounts.find((account) => account.id === detailAccountId) ?? null,
    [accounts, detailAccountId],
  );

  const isCurrentDetailRequest = useCallback(
    (accountId: number, generation: number) =>
      detailAccountIdRef.current === accountId &&
      detailGenerationRef.current === generation,
    [],
  );

  const openDetailAccount = useCallback((accountId: number) => {
    managementAbortRef.current?.abort();
    detailGenerationRef.current += 1;
    detailAccountIdRef.current = accountId;
    setManagementState(null);
    setManagementError(null);
    setManagementAction(null);
    setManagementLoading(true);
    setDetailAccountId(accountId);
  }, []);

  const closeDetailAccount = useCallback(() => {
    managementAbortRef.current?.abort();
    detailGenerationRef.current += 1;
    detailAccountIdRef.current = null;
    setDetailAccountId(null);
    setManagementState(null);
    setManagementError(null);
    setManagementAction(null);
    setManagementLoading(false);
  }, []);

  useEffect(() => {
    if (detailAccountId === null) {
      setManagementState(null);
      setManagementError(null);
      setManagementAction(null);
      setManagementLoading(false);
      return;
    }
    const accountId = detailAccountId;
    const generation = detailGenerationRef.current;
    const controller = new AbortController();
    managementAbortRef.current = controller;
    setManagementLoading(true);
    setManagementError(null);
    void api.getAntigravityAccountState(accountId, controller.signal)
      .then((state) => {
        if (isCurrentDetailRequest(accountId, generation)) {
          setManagementState(state);
        }
      })
      .catch((stateError) => {
        if (
          !controller.signal.aborted &&
          isCurrentDetailRequest(accountId, generation)
        ) {
          setManagementError(getErrorMessage(stateError));
        }
      })
      .finally(() => {
        if (
          !controller.signal.aborted &&
          isCurrentDetailRequest(accountId, generation)
        ) {
          setManagementLoading(false);
        }
      });
    return () => {
      controller.abort();
      if (managementAbortRef.current === controller) {
        managementAbortRef.current = null;
      }
    };
  }, [detailAccountId, isCurrentDetailRequest]);

  const reloadGroups = useCallback(async () => {
    const response = await api.listAccountGroups();
    const groups = response.groups ?? [];
    setAllGroups(groups);
    setGroupFilter((current) => pruneAccountGroupFilter(current, groups));
    return groups;
  }, []);

  useEffect(() => {
    void reloadGroups().catch(() => undefined);
  }, [reloadGroups]);

  const reload = useCallback(
    async (options?: { silent?: boolean }) => {
      requestAbortRef.current?.abort();
      const controller = new AbortController();
      requestAbortRef.current = controller;
      if (!options?.silent) setLoading(true);
      try {
        const response = await api.getAccountsPage(
          {
            channel: "antigravity",
            page,
            pageSize,
            search: debouncedSearchQuery,
            status: statusFilter,
            groupInclude: groupFilter.include,
            groupExclude: groupFilter.exclude,
            ungrouped: groupFilter.ungrouped,
            sort: "updated_at",
            order: "desc",
          },
          controller.signal,
        );
        if (controller.signal.aborted) return;
        const rows = (response.accounts ?? []).filter(
          (account) => account.antigravity_api !== false,
        );
        setAccounts(rows);
        setTotalAccounts(response.total ?? 0);
        setServerSummary(response.summary ?? null);
        if (response.page !== page) setPage(response.page);
        setError(null);
      } catch (loadError) {
        if (controller.signal.aborted) return;
        const message = getErrorMessage(loadError);
        setError(message);
        showToast(message, "error");
      } finally {
        if (requestAbortRef.current === controller) setLoading(false);
      }
    },
    [
      debouncedSearchQuery,
      groupFilter.exclude,
      groupFilter.include,
      groupFilter.ungrouped,
      page,
      pageSize,
      showToast,
      statusFilter,
    ],
  );

  useEffect(() => {
    void reload();
  }, [reload]);

  useEffect(() => () => requestAbortRef.current?.abort(), []);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedSearchQuery(searchQuery.trim());
      setPage(1);
    }, 250);
    return () => window.clearTimeout(timer);
  }, [searchQuery]);

  useEffect(() => {
    setPage(1);
  }, [groupFilter, statusFilter]);

  const stopOAuthPolling = useCallback(() => {
    oauthPollGenerationRef.current += 1;
    oauthPollInFlightRef.current = false;
    if (oauthPollTimerRef.current !== null) {
      window.clearTimeout(oauthPollTimerRef.current);
      oauthPollTimerRef.current = null;
    }
  }, []);

  const resetOAuth = useCallback(() => {
    stopOAuthPolling();
    setOAuthSession(null);
    setOAuthStatus(null);
    setOAuthCallbackURL("");
    setOAuthDraft(EMPTY_OAUTH_DRAFT);
    setOAuthModalStatus("idle");
  }, [stopOAuthPolling]);

  const pollOAuthStatus = useCallback(
    async (sessionId: string) => {
      if (oauthPollInFlightRef.current) return;
      const generation = oauthPollGenerationRef.current;
      oauthPollInFlightRef.current = true;
      try {
        const result = await api.getAntigravityOAuthStatus(sessionId);
        if (generation !== oauthPollGenerationRef.current) return;
        setOAuthStatus(result);
        if (result.status === "completed") {
          stopOAuthPolling();
          setOAuthModalStatus("completed");
          showToast(
            result.email
              ? t("antigravity.oauthSuccessWithEmail", { email: result.email })
              : t("antigravity.oauthSuccess"),
            result.warning ? "warning" : "success",
          );
          setPage(1);
          await reload({ silent: true });
          return;
        }
        if (result.status === "failed" || result.status === "cancelled") {
          stopOAuthPolling();
          setOAuthModalStatus(result.status);
          if (result.error) showToast(result.error, "error");
          return;
        }

        setOAuthModalStatus(result.status === "processing" ? "processing" : "waiting");
        oauthPollTimerRef.current = window.setTimeout(() => {
          if (generation !== oauthPollGenerationRef.current) return;
          oauthPollTimerRef.current = null;
          void pollOAuthStatus(sessionId);
        }, 1200);
      } catch (pollError) {
        if (generation !== oauthPollGenerationRef.current) return;
        stopOAuthPolling();
        const message = getErrorMessage(pollError);
        setOAuthModalStatus("failed");
        setOAuthStatus((current) =>
          current
            ? { ...current, status: "failed", error: message }
            : {
                session_id: sessionId,
                status: "failed",
                error: message,
                expires_at: new Date().toISOString(),
              },
        );
        showToast(message, "error");
      } finally {
        oauthPollInFlightRef.current = false;
      }
    },
    [reload, showToast, stopOAuthPolling, t],
  );

  const handleOAuthStart = useCallback(async () => {
    // Open synchronously inside the click handler so browsers do not classify
    // the later OAuth navigation (after the API round trip) as a blocked
    // popup. The window is only navigated after the server returns a
    // state-bound authorization URL.
    const popup = window.open("about:blank", "_blank");
    setOAuthStarting(true);
    stopOAuthPolling();
    setOAuthStatus(null);
    setOAuthCallbackURL("");
    setOAuthModalStatus("starting");
    try {
      const result = await api.startAntigravityOAuth({
        name: oauthDraft.name.trim() || undefined,
        proxy_url: oauthDraft.proxyUrl.trim() || undefined,
        group_ids: oauthDraft.groupIds,
      });
      setOAuthSession(result);
      setOAuthStatus({
        session_id: result.session_id,
        status: "waiting",
        expires_at: result.expires_at,
      });
      setOAuthModalStatus("waiting");
      if (popup) {
        try {
          popup.opener = null;
          popup.location.href = result.auth_url;
        } catch {
          showToast(t("antigravity.oauthPopupBlocked"), "warning");
        }
      } else {
        showToast(t("antigravity.oauthPopupBlocked"), "warning");
      }
      oauthPollTimerRef.current = window.setTimeout(() => {
        oauthPollTimerRef.current = null;
        void pollOAuthStatus(result.session_id);
      }, 700);
    } catch (startError) {
      try {
        popup?.close();
      } catch {
        // Ignore a popup that the user already closed.
      }
      setOAuthModalStatus("failed");
      showToast(getErrorMessage(startError), "error");
    } finally {
      setOAuthStarting(false);
    }
  }, [oauthDraft.groupIds, oauthDraft.name, oauthDraft.proxyUrl, pollOAuthStatus, showToast, stopOAuthPolling, t]);

  const handleOAuthComplete = useCallback(async () => {
    if (!oauthSession) return;
    const callbackURL = oauthCallbackURL.trim();
    if (!callbackURL) {
      showToast(t("antigravity.oauthCallbackRequired"), "error");
      return;
    }
    setOAuthCompleting(true);
    setOAuthModalStatus("processing");
    setOAuthStatus((current) => (current ? { ...current, status: "processing", error: "" } : current));
    try {
      await api.completeAntigravityOAuth({
        session_id: oauthSession.session_id,
        callback_url: callbackURL,
      });
      setOAuthCallbackURL("");
      stopOAuthPolling();
      oauthPollTimerRef.current = window.setTimeout(() => {
        oauthPollTimerRef.current = null;
        void pollOAuthStatus(oauthSession.session_id);
      }, 250);
    } catch (completeError) {
      const message = getErrorMessage(completeError);
      // Validation failures (for example a pasted URL with the wrong state)
      // leave the server session in `waiting`. Keep that state visible so the
      // user can correct the URL or cancel it cleanly instead of orphaning a
      // loopback listener by treating every 4xx as terminal.
      setOAuthModalStatus("waiting");
      setOAuthStatus((current) =>
        current ? { ...current, status: "failed", error: message } : current,
      );
      showToast(message, "error");
      oauthPollTimerRef.current = window.setTimeout(() => {
        oauthPollTimerRef.current = null;
        void pollOAuthStatus(oauthSession.session_id);
      }, 500);
    } finally {
      setOAuthCompleting(false);
    }
  }, [oauthCallbackURL, oauthSession, pollOAuthStatus, showToast, stopOAuthPolling, t]);

  const handleOAuthCancel = useCallback(async () => {
    const sessionId = oauthSession?.session_id;
    stopOAuthPolling();
    if (!sessionId) {
      resetOAuth();
      setShowOAuth(false);
      return;
    }
    setOAuthCancelling(true);
    try {
      await api.cancelAntigravityOAuth(sessionId);
      if (oauthStatus?.status !== "completed") {
        showToast(t("antigravity.oauthCancelled"), "success");
      }
    } catch (cancelError) {
      showToast(getErrorMessage(cancelError), "error");
    } finally {
      setOAuthCancelling(false);
      resetOAuth();
      setShowOAuth(false);
    }
  }, [oauthSession, oauthStatus?.status, resetOAuth, showToast, stopOAuthPolling, t]);

  const openOAuth = useCallback(() => {
    resetOAuth();
    setShowOAuth(true);
  }, [resetOAuth]);

  useEffect(() => () => stopOAuthPolling(), [stopOAuthPolling]);

  const resetImport = useCallback(() => {
    setImportDraft(EMPTY_IMPORT_DRAFT);
    setCredentialFiles([]);
    setImportResult(null);
    setImportMode("single");
    if (fileInputRef.current) fileInputRef.current.value = "";
  }, []);

  const closeImport = useCallback(() => {
    if (importing) return;
    setShowImport(false);
    resetImport();
  }, [importing, resetImport]);

  const createAntigravityGroup = useCallback(
    async (name: string): Promise<number | null> => {
      try {
        const response = await api.createAccountGroup({
          name: name.trim(),
          channel: "antigravity",
        });
        await reloadGroups();
        showToast(t("accounts.groupCreated"), "success");
        return response.id;
      } catch (createError) {
        showToast(getErrorMessage(createError), "error");
        return null;
      }
    },
    [reloadGroups, showToast, t],
  );

  const handleCredentialFiles = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    if (files.length === 0) return;
    try {
      const contents = await Promise.all(
        files.map(async (file) => ({ name: file.name, content: await file.text() })),
      );
      setCredentialFiles(contents);
      setImportResult(null);
    } catch (fileError) {
      showToast(getErrorMessage(fileError), "error");
    }
  };

  const handleImport = async () => {
    if (importing) return;
    if (
      importMode === "single" &&
      importDraft.authKind === "oauth" &&
      !importDraft.authJson.trim()
    ) {
      showToast(t("antigravity.authJsonRequired"), "error");
      return;
    }
    if (
      importMode === "single" &&
      importDraft.authKind === "api_key" &&
      !importDraft.apiKey.trim()
    ) {
      showToast(t("antigravity.apiKeyRequired"), "error");
      return;
    }
    if (importMode === "files" && credentialFiles.length === 0) {
      showToast(t("antigravity.filesRequired"), "error");
      return;
    }
    setImporting(true);
    setImportResult(null);
    try {
      if (importMode === "single") {
        const result = await api.addAntigravityAccount({
          name: importDraft.name.trim() || undefined,
          auth_kind: importDraft.authKind,
          auth_json:
            importDraft.authKind === "oauth"
              ? importDraft.authJson.trim()
              : undefined,
          api_key:
            importDraft.authKind === "api_key"
              ? importDraft.apiKey.trim()
              : undefined,
          models:
            importDraft.authKind === "api_key"
              ? parseModelList(importDraft.models)
              : undefined,
          model_mapping:
            importDraft.authKind === "api_key"
              ? importDraft.modelMapping.trim()
              : undefined,
          proxy_url: importDraft.proxyUrl.trim() || undefined,
          group_ids: importDraft.groupIds,
        });
        const warning = result.warning?.trim();
        showToast(
          warning || (result.synced ? t("antigravity.addSuccess") : t("antigravity.addDegraded")),
          warning || !result.synced ? "warning" : "success",
        );
        setShowImport(false);
        resetImport();
      } else {
        const result = await api.importAntigravityAccounts({
          files: credentialFiles.map((file) => file.content),
          proxy_url: importDraft.proxyUrl.trim() || undefined,
          group_ids: importDraft.groupIds,
          import_proxy: importDraft.importFileProxies || undefined,
        });
        setImportResult(result);
        const needsReview = result.failed > 0 || (result.degraded ?? 0) > 0;
        const proxyWarning = result.proxy_warning?.trim();
        // Toast 只有一个槽位,连续调用会互相顶掉——代理结果和告警必须拼进同一条。
        const summary = needsReview
          ? t("antigravity.importReview", {
              imported: result.imported,
              synced: result.synced ?? 0,
              degraded: result.degraded ?? 0,
              failed: result.failed,
            })
          : t("antigravity.importDone", {
              imported: result.imported,
              total: result.total,
            });
        const parts = [summary];
        if (result.proxies_imported !== undefined) {
          parts.push(
            t("accounts.importProxySummary", {
              imported: result.proxies_imported,
              skipped: result.proxies_skipped ?? 0,
            }),
          );
          if (proxyWarning) parts.push(proxyWarning);
        }
        showToast(
          parts.join(" "),
          needsReview || proxyWarning ? "warning" : "success",
        );
        if (result.failed === 0 && (result.degraded ?? 0) === 0) {
          setShowImport(false);
          resetImport();
        }
      }
      setPage(1);
      await reload({ silent: true });
    } catch (importError) {
      showToast(getErrorMessage(importError), "error");
    } finally {
      setImporting(false);
    }
  };

  const closeEditor = useCallback(() => {
    if (editing) return;
    editLoadGenerationRef.current += 1;
    setEditingAccount(null);
    setEditingDetailsLoading(false);
    setEditingDetailsError(null);
  }, [editing]);

  const openEditor = async (account: AccountRow) => {
    const generation = editLoadGenerationRef.current + 1;
    editLoadGenerationRef.current = generation;
    setEditingAccount(account);
    setEditingDetailsError(null);

    if (account.detail_loaded !== false) {
      setEditDraft(editDraftFromAccount(account));
      setEditingDetailsLoading(false);
      return;
    }

    setEditingDetailsLoading(true);
    try {
      const detail = await api.getAccount(account.id);
      if (editLoadGenerationRef.current !== generation) return;
      if (detail.id !== account.id) throw new Error(t("common.loadFailed"));
      setEditingAccount(detail);
      setEditDraft(editDraftFromAccount(detail));
    } catch (detailError) {
      if (editLoadGenerationRef.current === generation) {
        setEditingDetailsError(getErrorMessage(detailError));
      }
    } finally {
      if (editLoadGenerationRef.current === generation) {
        setEditingDetailsLoading(false);
      }
    }
  };

  const handleEdit = async () => {
    if (
      !editingAccount ||
      editing ||
      editingDetailsLoading ||
      editingDetailsError
    ) return;
    const payload: UpdateAntigravityAccountRequest = {
      name: editDraft.name.trim(),
      proxy_url: editDraft.proxyUrl.trim(),
      group_ids: editDraft.groupIds,
    };
    if (editDraft.authKind === "oauth" && editDraft.authJson.trim()) {
      payload.auth_json = editDraft.authJson.trim();
    }
    if (editDraft.authKind === "api_key") {
      if (editDraft.apiKey.trim()) payload.api_key = editDraft.apiKey.trim();
      payload.models = parseModelList(editDraft.models);
      payload.model_mapping = editDraft.modelMapping.trim();
    }
    setEditing(true);
    try {
      const result = await api.updateAntigravityAccount(editingAccount.id, payload);
      showToast(
        result.warning || t("antigravity.editSuccess"),
        result.warning ? "warning" : "success",
      );
      editLoadGenerationRef.current += 1;
      setEditingAccount(null);
      setEditingDetailsError(null);
      await reload({ silent: true });
    } catch (editError) {
      showToast(getErrorMessage(editError), "error");
    } finally {
      setEditing(false);
    }
  };

  const runAccountAction = useCallback(
    async (
      account: AccountRow,
      action: BusyAction,
      operation: () => Promise<{ warning?: string }>,
      successMessage: string,
    ) => {
      setBusy({ id: account.id, action });
      try {
        const result = await operation();
        showToast(
          result.warning || successMessage,
          result.warning ? "warning" : "success",
        );
      } catch (actionError) {
        showToast(getErrorMessage(actionError), "error");
      } finally {
        await reload({ silent: true });
        setBusy(null);
      }
    },
    [reload, showToast],
  );

  const handleDelete = useCallback(
    async (account: AccountRow) => {
      const confirmed = await confirm({
        title: t("antigravity.deleteTitle"),
        description: t("antigravity.deleteDescription", {
          account: accountLabel(account),
        }),
        confirmText: t("common.delete"),
        tone: "destructive",
        confirmVariant: "destructive",
      });
      if (!confirmed) return;
      await runAccountAction(
        account,
        "delete",
        () => api.deleteAccount(account.id),
        t("antigravity.deleteSuccess"),
      );
    },
    [confirm, runAccountAction, t],
  );

  const filtersActive = Boolean(
    debouncedSearchQuery ||
      statusFilter !== "all" ||
      !isAccountGroupFilterEmpty(groupFilter),
  );

  const handleStateSync = useCallback(async () => {
    const accountId = detailAccountIdRef.current;
    if (accountId === null || managementAction !== null) return;
    const generation = detailGenerationRef.current;
    setManagementAction("sync");
    setManagementError(null);
    try {
      const result = await api.syncAntigravityAccountState(accountId);
      if (!isCurrentDetailRequest(accountId, generation)) return;
      setManagementState(result.state);
      showToast(result.message, result.verified ? "success" : "warning");
      await reload({ silent: true });
    } catch (syncError) {
      if (!isCurrentDetailRequest(accountId, generation)) return;
      const message = getErrorMessage(syncError);
      setManagementError(message);
      showToast(message, "error");
    } finally {
      if (isCurrentDetailRequest(accountId, generation)) {
        setManagementAction(null);
      }
    }
  }, [isCurrentDetailRequest, managementAction, reload, showToast]);

  const handleCapabilityProbe = useCallback(async () => {
    const accountId = detailAccountIdRef.current;
    if (accountId === null || managementAction !== null) return;
    const generation = detailGenerationRef.current;
    setManagementAction("probe");
    setManagementError(null);
    try {
      const result = await api.probeAntigravityAccountCapabilities(accountId);
      if (!isCurrentDetailRequest(accountId, generation)) return;
      setManagementState(result.state);
      showToast(result.warning || result.message, result.result.verified ? "success" : "warning");
    } catch (probeError) {
      if (!isCurrentDetailRequest(accountId, generation)) return;
      const message = getErrorMessage(probeError);
      setManagementError(message);
      showToast(message, "error");
    } finally {
      if (isCurrentDetailRequest(accountId, generation)) {
        setManagementAction(null);
      }
    }
  }, [isCurrentDetailRequest, managementAction, showToast]);

  const handleExport = useCallback(async (ids?: number[]) => {
    if (exporting) return;
    const exportingAll = !ids || ids.length === 0;
    if (exportingAll) {
      const confirmed = await confirm({
        title: t("antigravity.exportAllConfirmTitle"),
        description: filtersActive
          ? t("antigravity.exportAllConfirmFiltered")
          : t("antigravity.exportAllConfirmDescription"),
        confirmText: t("antigravity.exportAllConfirmAction"),
        tone: "warning",
      });
      if (!confirmed) return;
    }
    setExporting(true);
    try {
      const { blob, filename, count: responseCount } =
        await api.exportAntigravityAccounts(ids);
      const count = responseCount ?? (ids?.length || undefined);
      const fallback = `codex2api-antigravity-${new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19)}.${blob.type.includes("zip") ? "zip" : "json"}`;
      downloadAntigravityBlob(blob, filename || fallback);
      showToast(
        count === undefined
          ? t("antigravity.exportSuccessUnknownCount")
          : t("antigravity.exportSuccess", { count }),
        "success",
      );
    } catch (exportError) {
      showToast(t("antigravity.exportFailed", { error: getErrorMessage(exportError) }), "error");
    } finally {
      setExporting(false);
    }
  }, [confirm, exporting, filtersActive, showToast, t]);

  const stats = {
    total: serverSummary?.total ?? totalAccounts,
    active: serverSummary?.active ?? 0,
    error: serverSummary?.error ?? 0,
    disabled: serverSummary?.disabled ?? 0,
  };
  const totalPages = Math.max(1, Math.ceil(totalAccounts / pageSize));

  const clearFilters = () => {
    setSearchQuery("");
    setDebouncedSearchQuery("");
    setStatusFilter("all");
    setGroupFilter(EMPTY_ACCOUNT_GROUP_FILTER);
    setPage(1);
  };

  const renderActions = (account: AccountRow) => {
    const accountBusy = busy?.id === account.id;
    const isAPIKey = account.antigravity_auth_kind === "api_key";
    return (
      <div className="flex items-center justify-end gap-0.5">
        <Button
          variant="ghost"
          size="icon-sm"
          disabled={accountBusy || exporting}
          onClick={() => void handleExport([account.id])}
          title={t("antigravity.exportOne")}
          aria-label={t("antigravity.exportOne")}
        >
          <Download />
        </Button>
        {!isAPIKey ? (
          <>
            <Button
              variant="ghost"
              size="icon-sm"
              disabled={accountBusy}
              onClick={() =>
                void runAccountAction(
                  account,
                  "quota",
                  () => api.refreshAntigravityQuota(account.id),
                  t("antigravity.quotaRefreshSuccess"),
                )
              }
              title={t("antigravity.refreshQuota")}
              aria-label={t("antigravity.refreshQuota")}
            >
              {busy?.id === account.id && busy.action === "quota" ? (
                <Loader2 className="animate-spin" />
              ) : (
                <CircleGauge />
              )}
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              disabled={accountBusy}
              onClick={() =>
                void runAccountAction(
                  account,
                  "refresh",
                  () => api.refreshAntigravityAccount(account.id),
                  t("antigravity.refreshSuccess"),
                )
              }
              title={t("antigravity.refreshCredential")}
              aria-label={t("antigravity.refreshCredential")}
            >
              {busy?.id === account.id && busy.action === "refresh" ? (
                <Loader2 className="animate-spin" />
              ) : (
                <RotateCw />
              )}
            </Button>
          </>
        ) : null}
        <Button
          variant="ghost"
          size="icon-sm"
          disabled={accountBusy}
          onClick={() => void openEditor(account)}
          title={t("common.edit")}
          aria-label={t("common.edit")}
        >
          <Pencil />
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          disabled={accountBusy}
          onClick={() =>
            void runAccountAction(
              account,
              "toggle",
              () => api.toggleAccountEnabled(account.id, account.enabled === false),
              account.enabled === false
                ? t("antigravity.enableSuccess")
                : t("antigravity.disableSuccess"),
            )
          }
          title={
            account.enabled === false
              ? t("antigravity.enableAccount")
              : t("antigravity.disableAccount")
          }
          aria-label={
            account.enabled === false
              ? t("antigravity.enableAccount")
              : t("antigravity.disableAccount")
          }
        >
          {busy?.id === account.id && busy.action === "toggle" ? (
            <Loader2 className="animate-spin" />
          ) : account.enabled === false ? (
            <Power />
          ) : (
            <PowerOff />
          )}
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          disabled={accountBusy}
          onClick={() => void handleDelete(account)}
          title={t("common.delete")}
          aria-label={t("common.delete")}
        >
          {busy?.id === account.id && busy.action === "delete" ? (
            <Loader2 className="animate-spin" />
          ) : (
            <Trash2 className="text-destructive" />
          )}
        </Button>
      </div>
    );
  };

  return (
    <div className="relative @container/antigravity-accounts">
      <PageHeader
        title={t("antigravity.pageTitle")}
        description={t("antigravity.pageSubtitle")}
        hideTitle
        actionsBelow
        titleAdornment={headerSlot}
        onRefresh={() => void reload()}
        actions={
          <>
            <Button variant="outline" size="sm" disabled={exporting || stats.total === 0} onClick={() => void handleExport()}>
              {exporting ? <Loader2 className="size-3.5 animate-spin" /> : <Download className="size-3.5" />}
              {exporting ? t("antigravity.exporting") : t("antigravity.exportAll")}
            </Button>
            <Button variant="outline" size="sm" onClick={openOAuth}>
              <Link2 className="size-3.5" />
              {t("antigravity.oauthAddAccount")}
            </Button>
            <Button size="sm" onClick={() => setShowImport(true)}>
              <Plus className="size-3.5" />
              {t("antigravity.addAccount")}
            </Button>
          </>
        }
      />

      <div className="mb-4 grid grid-cols-2 gap-2 sm:grid-cols-4 sm:gap-3">
        <CompactStat
          label={t("antigravity.statTotal")}
          value={stats.total}
          tone="neutral"
          active={statusFilter === "all"}
          onClick={() => setStatusFilter("all")}
        />
        <CompactStat
          label={t("antigravity.statActive")}
          value={stats.active}
          tone="success"
          active={statusFilter === "active"}
          onClick={() => setStatusFilter("active")}
        />
        <CompactStat
          label={t("antigravity.statError")}
          value={stats.error}
          tone="warning"
          active={statusFilter === "error"}
          onClick={() => setStatusFilter("error")}
        />
        <CompactStat
          label={t("antigravity.statDisabled")}
          value={stats.disabled}
          tone="danger"
          active={statusFilter === "disabled"}
          onClick={() => setStatusFilter("disabled")}
        />
      </div>

      <div className="mb-4 flex flex-col gap-2 rounded-lg border border-border bg-card p-3 sm:flex-row sm:items-center">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={searchQuery}
            onChange={(event) => setSearchQuery(event.target.value)}
            placeholder={t("antigravity.searchPlaceholder")}
            className="pl-9"
          />
        </div>
        <div className="w-full sm:w-40">
          <Select
            value={statusFilter}
            onValueChange={(value) => setStatusFilter(value as StatusFilter)}
            options={[
              { value: "all", label: t("antigravity.filterAll") },
              { value: "active", label: t("antigravity.filterActive") },
              { value: "disabled", label: t("antigravity.filterDisabled") },
              { value: "error", label: t("antigravity.filterError") },
            ]}
          />
        </div>
        <AccountGroupFilterSelect
          groups={antigravityGroups}
          value={groupFilter}
          onChange={setGroupFilter}
          className="w-full sm:w-48"
        />
        {filtersActive ? (
          <Button variant="ghost" size="sm" onClick={clearFilters}>
            <X className="size-3.5" />
            {t("antigravity.clearFilters")}
          </Button>
        ) : null}
      </div>

      <StateShell
        variant="page"
        loading={loading && accounts.length === 0}
        error={accounts.length === 0 ? error : null}
        onRetry={() => void reload()}
        isEmpty={!loading && !error && accounts.length === 0}
        emptyIcon={<ChannelLogo channel="antigravity" size={30} />}
        loadingTitle={t("antigravity.loadingTitle")}
        loadingDescription={t("antigravity.loadingDescription")}
        errorTitle={t("antigravity.errorTitle")}
        emptyTitle={
          filtersActive
            ? t("antigravity.noMatchesTitle")
            : t("antigravity.emptyTitle")
        }
        emptyDescription={
          filtersActive
            ? t("antigravity.noMatchesDescription")
            : t("antigravity.emptyDescription")
        }
        action={
          filtersActive ? (
            <Button variant="outline" onClick={clearFilters}>
              {t("antigravity.clearFilters")}
            </Button>
          ) : (
            <Button onClick={() => setShowImport(true)}>
              <Plus className="size-4" />
              {t("antigravity.addAccount")}
            </Button>
          )
        }
      >
        <div className="hidden overflow-hidden rounded-lg border border-border bg-card md:block">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("antigravity.columnAccount")}</TableHead>
                <TableHead>{t("antigravity.columnProject")}</TableHead>
                <TableHead>{t("antigravity.columnPermission")}</TableHead>
                <TableHead>{t("antigravity.columnQuota")}</TableHead>
                <TableHead>{t("accounts.proxyColumn")}</TableHead>
                <TableHead>{t("antigravity.columnStatus")}</TableHead>
                <TableHead>{t("antigravity.columnUpdated")}</TableHead>
                <TableHead className="w-[184px] text-right">
                  {t("antigravity.columnActions")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {accounts.map((account) => (
                <TableRow key={account.id} className={account.enabled === false ? "opacity-65" : undefined}>
                  <TableCell className="min-w-[220px]">
                    <button
                      type="button"
                      onClick={() => openDetailAccount(account.id)}
                      className="flex min-w-0 items-center gap-3 text-left"
                    >
                      <AccountAvatar account={account} />
                      <span className="min-w-0">
                        <span className="flex flex-wrap items-center gap-1.5">
                          <span className="block max-w-[220px] truncate text-sm font-semibold text-foreground">
                            {account.name || account.email || `#${account.id}`}
                          </span>
                          <Badge variant="outline" className="text-[10px]">
                            {account.antigravity_auth_kind === "api_key"
                              ? t("antigravity.authKindApiKey")
                              : t("antigravity.authKindOAuth")}
                          </Badge>
                          {identityVerified(account) ? (
                            <CheckCircle2 className="size-3.5 shrink-0 text-emerald-500" />
                          ) : null}
                        </span>
                        {account.name && account.email ? (
                          <span className="mt-0.5 block max-w-[220px] truncate text-xs text-muted-foreground">
                            {account.email}
                          </span>
                        ) : null}
                        <GroupChips account={account} groups={allGroups} />
                      </span>
                    </button>
                  </TableCell>
                  <TableCell className="max-w-[220px]">
                    <div className="truncate text-sm font-medium text-foreground">
                      {subscriptionTier(account) || t("antigravity.tierUnknown")}
                    </div>
                    <div
                      className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground"
                      title={identityProjectID(account)}
                    >
                      {identityProjectID(account) || t("antigravity.projectPending")}
                    </div>
                  </TableCell>
                  <TableCell className="max-w-[190px]">
                    <PermissionBadge permissions={account.antigravity_permissions} />
                    {account.antigravity_permissions?.reason ? (
                      <div
                        className="mt-1 max-w-[180px] truncate text-[11px] text-muted-foreground"
                        title={account.antigravity_permissions.reason}
                      >
                        {account.antigravity_permissions.reason}
                      </div>
                    ) : null}
                    <SyncWarningAction warning={account.antigravity_sync_warning} />
                  </TableCell>
                  <TableCell>
                    <CompactQuota
                      account={account}
                      onOpen={() => openDetailAccount(account.id)}
                    />
                  </TableCell>
                  <TableCell className="min-w-[120px] max-w-[180px]">
                    <AccountProxyBadge
                      account={account}
                      ctx={proxyBindingCtx}
                      onClick={() => setQuickProxyAccount(account)}
                    />
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col items-start gap-1">
                      <StatusBadge
                        status={account.enabled === false ? "paused" : account.status}
                        errorMessage={account.error_message}
                      />
                      {account.locked ? (
                        <Badge variant="outline" className="text-[10px]">
                          <KeyRound className="size-3" />
                          {t("antigravity.locked")}
                        </Badge>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="text-xs text-foreground">
                      {formatRelativeTime(account.updated_at, { variant: "compact" })}
                    </div>
                    <div className="mt-0.5 text-[10px] text-muted-foreground">
                      {formatBeijingTime(account.updated_at)}
                    </div>
                  </TableCell>
                  <TableCell>{renderActions(account)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>

        <div className="grid gap-2 md:hidden">
          {accounts.map((account) => (
            <article
              key={account.id}
              className={cn(
                "rounded-lg border border-border bg-card p-3 shadow-sm",
                account.enabled === false && "opacity-65",
              )}
            >
              <div className="flex items-start gap-3">
                <button
                  type="button"
                  className="flex min-w-0 flex-1 items-start gap-3 text-left"
                  onClick={() => openDetailAccount(account.id)}
                >
                  <AccountAvatar account={account} size={40} />
                  <span className="min-w-0 flex-1">
                    <span className="flex flex-wrap items-center gap-1.5">
                      <span className="truncate text-sm font-semibold text-foreground">
                        {account.name || account.email || `#${account.id}`}
                      </span>
                      <Badge variant="outline" className="text-[10px]">
                        {account.antigravity_auth_kind === "api_key"
                          ? t("antigravity.authKindApiKey")
                          : t("antigravity.authKindOAuth")}
                      </Badge>
                      {identityVerified(account) ? (
                        <CheckCircle2 className="size-3.5 shrink-0 text-emerald-500" />
                      ) : null}
                    </span>
                    {account.name && account.email ? (
                      <span className="mt-0.5 block truncate text-xs text-muted-foreground">
                        {account.email}
                      </span>
                    ) : null}
                    <GroupChips account={account} groups={allGroups} />
                  </span>
                </button>
                <StatusBadge
                  status={account.enabled === false ? "paused" : account.status}
                  errorMessage={account.error_message}
                />
              </div>
              <div className="mt-3 grid grid-cols-2 gap-3 border-t border-border pt-3 text-xs">
                <div className="min-w-0">
                  <div className="text-[10px] font-semibold uppercase text-muted-foreground">
                    {t("antigravity.columnProject")}
                  </div>
                  <div className="mt-1 truncate font-medium text-foreground">
                    {subscriptionTier(account) || t("antigravity.tierUnknown")}
                  </div>
                </div>
                <div className="min-w-0">
                  <div className="text-[10px] font-semibold uppercase text-muted-foreground">
                    {t("antigravity.columnPermission")}
                  </div>
                  <div className="mt-1">
                    <PermissionBadge permissions={account.antigravity_permissions} />
                  </div>
                </div>
              </div>
              <div className="mt-3 border-t border-border pt-3">
                <CompactQuota
                  account={account}
                  onOpen={() => openDetailAccount(account.id)}
                />
              </div>
              <div className="mt-3 flex border-t border-border pt-3">
                <AccountProxyBadge
                  account={account}
                  ctx={proxyBindingCtx}
                  onClick={() => setQuickProxyAccount(account)}
                />
              </div>
              <div className="mt-3 flex items-center justify-between gap-2 border-t border-border pt-2">
                <span className="text-[10px] text-muted-foreground">
                  {formatRelativeTime(account.updated_at, { variant: "compact" })}
                </span>
                {renderActions(account)}
              </div>
            </article>
          ))}
        </div>

        <Pagination
          page={page}
          totalPages={totalPages}
          onPageChange={setPage}
          totalItems={totalAccounts}
          pageSize={pageSize}
          onPageSizeChange={(next) => {
            setPageSize(next);
            setPage(1);
          }}
          pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
        />
      </StateShell>

      <Modal
        show={showOAuth}
        title={t("antigravity.oauthTitle")}
        onClose={() => {
          if (!oauthStarting && !oauthCompleting && !oauthCancelling) {
            void handleOAuthCancel();
          }
        }}
        contentClassName="sm:max-w-[640px]"
        footer={
          <>
            <Button
              variant="outline"
              onClick={() => void handleOAuthCancel()}
              disabled={oauthStarting || oauthCompleting || oauthCancelling}
            >
              {oauthModalStatus === "completed" || oauthModalStatus === "failed" || oauthModalStatus === "cancelled"
                ? t("common.close")
                : t("common.cancel")}
            </Button>
            {!oauthSession ? (
              <Button onClick={() => void handleOAuthStart()} disabled={oauthStarting}>
                {oauthStarting ? <Loader2 className="animate-spin" /> : <Link2 />}
                {oauthStarting ? t("antigravity.oauthStarting") : t("antigravity.oauthStart")}
              </Button>
            ) : oauthModalStatus === "failed" || oauthModalStatus === "cancelled" ? (
              <Button
                onClick={() => {
                  resetOAuth();
                }}
                disabled={oauthCancelling}
              >
                <RotateCw />
                {t("antigravity.oauthRetry")}
              </Button>
            ) : oauthModalStatus === "completed" ? null : (
              <Button
                onClick={() => void handleOAuthComplete()}
                disabled={oauthCompleting || !oauthCallbackURL.trim()}
              >
                {oauthCompleting ? <Loader2 className="animate-spin" /> : <Link2 />}
                {oauthCompleting
                  ? t("antigravity.oauthSubmitting")
                  : t("antigravity.oauthComplete")}
              </Button>
            )}
          </>
        }
      >
        <div className="space-y-4">
          {!oauthSession ? (
            <>
              <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground">
                <p className="mb-1 font-semibold text-foreground">
                  {t("antigravity.oauthStep1Title")}
                </p>
                <p>{t("antigravity.oauthStep1Description")}</p>
              </div>
              <label className="block space-y-1.5">
                <span className="text-xs font-semibold text-muted-foreground">
                  {t("antigravity.accountName")}
                </span>
                <Input
                  value={oauthDraft.name}
                  onChange={(event) =>
                    setOAuthDraft((current) => ({ ...current, name: event.target.value }))
                  }
                  placeholder={t("antigravity.accountNamePlaceholder")}
                />
              </label>
              <AccountMetadataFields
                proxyUrl={oauthDraft.proxyUrl}
                proxies={proxyPool}
                onProxyUrlChange={(proxyUrl) =>
                  setOAuthDraft((current) => ({ ...current, proxyUrl }))
                }
                groupIds={oauthDraft.groupIds}
                onGroupIdsChange={(groupIds) =>
                  setOAuthDraft((current) => ({ ...current, groupIds }))
                }
                groups={antigravityGroups}
                onCreateGroup={createAntigravityGroup}
              />
            </>
          ) : (
            <>
              <div
                className={cn(
                  "rounded-xl border px-4 py-3 text-sm",
                  oauthModalStatus === "failed"
                    ? "border-red-500/30 bg-red-500/5 text-red-700 dark:text-red-300"
                    : oauthModalStatus === "completed"
                      ? "border-emerald-500/30 bg-emerald-500/5 text-emerald-700 dark:text-emerald-300"
                      : "border-primary/30 bg-primary/5 text-muted-foreground",
                )}
              >
                <div className="flex items-center gap-2 font-semibold text-foreground">
                  {oauthModalStatus === "completed" ? (
                    <CheckCircle2 className="size-4 text-emerald-500" />
                  ) : oauthModalStatus === "failed" || oauthModalStatus === "cancelled" ? (
                    <TriangleAlert className="size-4 text-red-500" />
                  ) : (
                    <Loader2 className="size-4 animate-spin text-primary" />
                  )}
                  {oauthModalStatus === "waiting"
                    ? t("antigravity.oauthWaiting")
                    : oauthModalStatus === "processing"
                      ? t("antigravity.oauthProcessing")
                      : oauthModalStatus === "completed"
                        ? t("antigravity.oauthCompleted")
                        : oauthModalStatus === "cancelled"
                          ? t("antigravity.oauthCancelled")
                          : t("antigravity.oauthFailed")}
                </div>
                <p className="mt-1 text-xs leading-relaxed">
                  {oauthModalStatus === "completed"
                    ? oauthStatus?.email
                      ? t("antigravity.oauthSuccessWithEmail", { email: oauthStatus.email })
                      : t("antigravity.oauthSuccess")
                    : t("antigravity.oauthStep2Description")}
                </p>
              </div>

              {oauthSession.auth_url ? (
                <div className="rounded-xl border border-border px-4 py-3">
                  <p className="mb-2 text-xs font-semibold text-muted-foreground">
                    {t("antigravity.oauthOpenLink")}
                  </p>
                  <a
                    href={oauthSession.auth_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-start gap-1.5 text-sm font-semibold text-primary hover:underline"
                  >
                    <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                    <span className="break-all">{oauthSession.auth_url}</span>
                  </a>
                </div>
              ) : null}

              {oauthModalStatus !== "completed" ? (
                <label className="block space-y-1.5">
                  <span className="text-xs font-semibold text-muted-foreground">
                    {t("antigravity.oauthCallbackLabel")}
                  </span>
                  <textarea
                    value={oauthCallbackURL}
                    onChange={(event) => setOAuthCallbackURL(event.target.value)}
                    rows={3}
                    spellCheck={false}
                    placeholder={t("antigravity.oauthCallbackPlaceholder")}
                    className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
                  />
                  <p className="text-[11px] leading-relaxed text-muted-foreground">
                    {t("antigravity.oauthCallbackHint")}
                  </p>
                </label>
              ) : null}

              {oauthStatus?.warning ? (
                <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
                  {oauthStatus.warning}
                </div>
              ) : null}
              {oauthStatus?.error ? (
                <div className="rounded-lg border border-red-500/30 bg-red-500/5 px-3 py-2 text-xs text-red-700 dark:text-red-300">
                  {oauthStatus.error}
                </div>
              ) : null}
            </>
          )}
        </div>
      </Modal>

      <Modal
        show={showImport}
        title={t("antigravity.importTitle")}
        onClose={closeImport}
        contentClassName="sm:max-w-[620px]"
        footer={
          <>
            <Button variant="outline" onClick={closeImport} disabled={importing}>
              {t("common.cancel")}
            </Button>
            <Button onClick={() => void handleImport()} disabled={importing}>
              {importing ? <Loader2 className="animate-spin" /> : <Upload />}
              {importing
                ? t("antigravity.importing")
                : importMode === "single"
                  ? t("antigravity.addAccount")
                  : t("antigravity.importFiles")}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <div className="grid grid-cols-2 rounded-lg border border-border bg-muted/40 p-0.5">
            {(
              [
                ["single", t("antigravity.singleCredential"), FileJson],
                ["files", t("antigravity.batchFiles"), FolderOpen],
              ] as const
            ).map(([mode, label, Icon]) => (
              <button
                key={mode}
                type="button"
                onClick={() => {
                  setImportMode(mode);
                  setImportResult(null);
                }}
                className={cn(
                  "inline-flex items-center justify-center gap-2 rounded-md px-3 py-2 text-sm font-semibold transition-colors",
                  importMode === mode
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                <Icon className="size-4" />
                {label}
              </button>
            ))}
          </div>

          {importMode === "single" ? (
            <>
              <label className="block space-y-1.5">
                <span className="text-xs font-semibold text-muted-foreground">
                  {t("antigravity.accountName")}
                </span>
                <Input
                  value={importDraft.name}
                  onChange={(event) =>
                    setImportDraft((current) => ({ ...current, name: event.target.value }))
                  }
                  placeholder={t("antigravity.accountNamePlaceholder")}
                />
              </label>
              <div className="space-y-1.5">
                <span className="text-xs font-semibold text-muted-foreground">
                  {t("antigravity.authKind")}
                </span>
                <Select
                  value={importDraft.authKind}
                  onValueChange={(value) =>
                    setImportDraft((current) => ({
                      ...current,
                      authKind: value as AntigravityAuthKind,
                    }))
                  }
                  options={[
                    { value: "oauth", label: t("antigravity.authKindOAuth") },
                    { value: "api_key", label: t("antigravity.authKindApiKey") },
                  ]}
                />
              </div>
              {importDraft.authKind === "oauth" ? (
                <label className="block space-y-1.5">
                  <span className="text-xs font-semibold text-muted-foreground">
                    {t("antigravity.authJson")}
                  </span>
                  <textarea
                    value={importDraft.authJson}
                    onChange={(event) =>
                      setImportDraft((current) => ({
                        ...current,
                        authJson: event.target.value,
                      }))
                    }
                    rows={9}
                    spellCheck={false}
                    placeholder={t("antigravity.authJsonPlaceholder")}
                    className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
                  />
                  <p className="text-[11px] leading-relaxed text-muted-foreground">
                    {t("antigravity.authJsonHint")}
                  </p>
                </label>
              ) : (
                <div className="space-y-4">
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("antigravity.apiKey")}
                    </span>
                    <Input
                      type="password"
                      autoComplete="new-password"
                      value={importDraft.apiKey}
                      onChange={(event) =>
                        setImportDraft((current) => ({ ...current, apiKey: event.target.value }))
                      }
                      placeholder={t("antigravity.apiKeyPlaceholder")}
                    />
                    <p className="text-[11px] leading-relaxed text-muted-foreground">
                      {t("antigravity.apiKeyHint")}
                    </p>
                  </label>
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("antigravity.models")}
                    </span>
                    <textarea
                      value={importDraft.models}
                      onChange={(event) =>
                        setImportDraft((current) => ({ ...current, models: event.target.value }))
                      }
                      rows={4}
                      spellCheck={false}
                      placeholder={t("antigravity.modelsPlaceholder")}
                      className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
                    />
                    <div className="flex flex-wrap gap-1.5">
                      {ANTIGRAVITY_DEFAULT_MODELS.map((model) => (
                        <Button
                          key={model}
                          type="button"
                          variant="outline"
                          size="sm"
                          onClick={() =>
                            setImportDraft((current) => ({
                              ...current,
                              models: Array.from(
                                new Set([...parseModelList(current.models), model]),
                              ).join("\n"),
                            }))
                          }
                        >
                          {model}
                        </Button>
                      ))}
                    </div>
                  </label>
                  <label className="block space-y-1.5">
                    <span className="text-xs font-semibold text-muted-foreground">
                      {t("antigravity.modelMapping")}
                    </span>
                    <textarea
                      value={importDraft.modelMapping}
                      onChange={(event) =>
                        setImportDraft((current) => ({
                          ...current,
                          modelMapping: event.target.value,
                        }))
                      }
                      rows={4}
                      spellCheck={false}
                      placeholder={t("antigravity.modelMappingPlaceholder")}
                      className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
                    />
                    <p className="text-[11px] leading-relaxed text-muted-foreground">
                      {t("antigravity.modelMappingHint")}
                    </p>
                  </label>
                  <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
                    {t("antigravity.interactionsExperimental")}
                  </div>
                </div>
              )}
            </>
          ) : (
            <div className="space-y-2">
              <input
                ref={fileInputRef}
                type="file"
                accept=".json,application/json"
                multiple
                className="hidden"
                onChange={(event) => void handleCredentialFiles(event)}
              />
              <button
                type="button"
                onClick={() => fileInputRef.current?.click()}
                className="flex min-h-[120px] w-full flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border bg-muted/20 px-5 py-6 text-center transition-colors hover:border-primary/40 hover:bg-primary/5"
              >
                <FolderOpen className="size-7 text-primary" />
                <span className="text-sm font-semibold text-foreground">
                  {t("antigravity.chooseFiles")}
                </span>
                <span className="text-xs text-muted-foreground">
                  {t("antigravity.chooseFilesHint")}
                </span>
              </button>
              {credentialFiles.length > 0 ? (
                <div className="max-h-28 overflow-y-auto rounded-lg border border-border px-3 py-2">
                  {credentialFiles.map((file) => (
                    <div
                      key={file.name}
                      className="flex items-center gap-2 py-1 text-xs text-foreground"
                    >
                      <FileJson className="size-3.5 shrink-0 text-muted-foreground" />
                      <span className="min-w-0 flex-1 truncate">{file.name}</span>
                    </div>
                  ))}
                </div>
              ) : null}
            </div>
          )}

          <AccountMetadataFields
            proxyUrl={importDraft.proxyUrl}
            proxies={proxyPool}
            onProxyUrlChange={(proxyUrl) =>
              setImportDraft((current) => ({ ...current, proxyUrl }))
            }
            groupIds={importDraft.groupIds}
            onGroupIdsChange={(groupIds) =>
              setImportDraft((current) => ({ ...current, groupIds }))
            }
            groups={antigravityGroups}
            onCreateGroup={createAntigravityGroup}
          />

          {importMode === "files" ? (
            <div className="space-y-1">
              <label className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                <input
                  type="checkbox"
                  className="size-3.5"
                  checked={importDraft.importFileProxies}
                  onChange={(event) =>
                    setImportDraft((current) => ({
                      ...current,
                      importFileProxies: event.target.checked,
                    }))
                  }
                />
                {t("accounts.importFileProxies")}
              </label>
              <p className="text-[11px] text-muted-foreground">
                {t("antigravity.importFileProxiesHint")}
              </p>
            </div>
          ) : null}

          {importResult && (importResult.failed > 0 || (importResult.degraded ?? 0) > 0) ? (
            <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
              <div className="text-sm font-semibold text-amber-700 dark:text-amber-300">
                {t("antigravity.importReview", {
                  imported: importResult.imported,
                  synced: importResult.synced ?? 0,
                  degraded: importResult.degraded ?? 0,
                  failed: importResult.failed,
                })}
              </div>
              <div className="mt-2 max-h-28 space-y-1 overflow-y-auto text-xs text-muted-foreground">
                {importResult.items
                  .filter((item) => !item.ok || !item.synced || item.warning || item.error)
                  .slice(0, 20)
                  .map((item) => (
                    <div
                      key={`${item.index}-${item.sub_index ?? 0}-${item.email ?? "item"}`}
                    >
                      {importItemDisplayLabel(item)}: {item.error || item.warning || t("antigravity.syncPending")}
                    </div>
                  ))}
              </div>
            </div>
          ) : null}
        </div>
      </Modal>

      <Modal
        show={editingAccount !== null}
        title={t("antigravity.editTitle")}
        onClose={closeEditor}
        contentClassName="sm:max-w-[580px]"
        footer={
          <>
            <Button
              variant="outline"
              onClick={closeEditor}
              disabled={editing}
            >
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => void handleEdit()}
              disabled={
                editing || editingDetailsLoading || Boolean(editingDetailsError)
              }
            >
              {editing ? <Loader2 className="animate-spin" /> : <Pencil />}
              {editing ? t("common.saving") : t("common.save")}
            </Button>
          </>
        }
      >
        {editingDetailsLoading ? (
          <div className="flex min-h-36 items-center justify-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            {t("antigravity.editDetailsLoading")}
          </div>
        ) : editingDetailsError ? (
          <div className="space-y-3 rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
            <p>
              {t("antigravity.editDetailsLoadFailed", {
                error: editingDetailsError,
              })}
            </p>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => {
                if (editingAccount) void openEditor(editingAccount);
              }}
            >
              <RotateCw className="size-3.5" />
              {t("common.retry")}
            </Button>
          </div>
        ) : (
          <div className="space-y-4">
          <label className="block space-y-1.5">
            <span className="text-xs font-semibold text-muted-foreground">
              {t("antigravity.accountName")}
            </span>
            <Input
              value={editDraft.name}
              onChange={(event) =>
                setEditDraft((current) => ({ ...current, name: event.target.value }))
              }
            />
          </label>
          {editDraft.authKind === "oauth" ? (
            <label className="block space-y-1.5">
              <span className="text-xs font-semibold text-muted-foreground">
                {t("antigravity.replacementAuthJson")}
              </span>
              <textarea
                value={editDraft.authJson}
                onChange={(event) =>
                  setEditDraft((current) => ({
                    ...current,
                    authJson: event.target.value,
                  }))
                }
                rows={7}
                spellCheck={false}
                placeholder={t("antigravity.replacementAuthJsonPlaceholder")}
                className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
              />
              <p className="text-[11px] text-muted-foreground">
                {t("antigravity.replacementAuthJsonHint")}
              </p>
            </label>
          ) : (
            <div className="space-y-4">
              <label className="block space-y-1.5">
                <span className="text-xs font-semibold text-muted-foreground">
                  {t("antigravity.replacementApiKey")}
                </span>
                <Input
                  type="password"
                  autoComplete="new-password"
                  value={editDraft.apiKey}
                  onChange={(event) =>
                    setEditDraft((current) => ({ ...current, apiKey: event.target.value }))
                  }
                  placeholder={t("antigravity.replacementApiKeyPlaceholder")}
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("antigravity.replacementApiKeyHint")}
                </p>
              </label>
              <label className="block space-y-1.5">
                <span className="text-xs font-semibold text-muted-foreground">
                  {t("antigravity.models")}
                </span>
                <textarea
                  value={editDraft.models}
                  onChange={(event) =>
                    setEditDraft((current) => ({ ...current, models: event.target.value }))
                  }
                  rows={4}
                  spellCheck={false}
                  placeholder={t("antigravity.modelsPlaceholder")}
                  className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
                />
                <div className="flex flex-wrap gap-1.5">
                  {ANTIGRAVITY_DEFAULT_MODELS.map((model) => (
                    <Button
                      key={model}
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() =>
                        setEditDraft((current) => ({
                          ...current,
                          models: Array.from(
                            new Set([...parseModelList(current.models), model]),
                          ).join("\n"),
                        }))
                      }
                    >
                      {model}
                    </Button>
                  ))}
                </div>
              </label>
              <label className="block space-y-1.5">
                <span className="text-xs font-semibold text-muted-foreground">
                  {t("antigravity.modelMapping")}
                </span>
                <textarea
                  value={editDraft.modelMapping}
                  onChange={(event) =>
                    setEditDraft((current) => ({
                      ...current,
                      modelMapping: event.target.value,
                    }))
                  }
                  rows={4}
                  spellCheck={false}
                  placeholder={t("antigravity.modelMappingPlaceholder")}
                  className="w-full resize-y rounded-md border border-input bg-background px-3 py-2 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/30"
                />
                <p className="text-[11px] text-muted-foreground">
                  {t("antigravity.modelMappingHint")}
                </p>
              </label>
              <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">
                {t("antigravity.interactionsExperimental")}
              </div>
            </div>
          )}
          <AccountMetadataFields
            proxyUrl={editDraft.proxyUrl}
            proxies={proxyPool}
            onProxyUrlChange={(proxyUrl) =>
              setEditDraft((current) => ({ ...current, proxyUrl }))
            }
            groupIds={editDraft.groupIds}
            onGroupIdsChange={(groupIds) =>
              setEditDraft((current) => ({ ...current, groupIds }))
            }
            groups={antigravityGroups}
            onCreateGroup={createAntigravityGroup}
          />
          </div>
        )}
      </Modal>

      <Modal
        show={detailAccount !== null}
        title={
          detailAccount ? (
            <span className="flex min-w-0 items-center gap-3">
              <AccountAvatar account={detailAccount} size={36} />
              <span className="min-w-0">
                <span className="block truncate">{accountLabel(detailAccount)}</span>
                {detailAccount.name && detailAccount.email ? (
                  <span className="mt-0.5 block truncate text-xs font-normal text-muted-foreground">
                    {detailAccount.email}
                  </span>
                ) : null}
              </span>
            </span>
          ) : (
            t("antigravity.detailsTitle")
          )
        }
        onClose={closeDetailAccount}
        contentClassName="sm:max-w-[760px]"
        footer={
          <Button variant="outline" onClick={closeDetailAccount}>
            {t("common.close")}
          </Button>
        }
      >
        {detailAccount ? (
          <div className="space-y-4">
            <AntigravityManagementState
              state={managementState}
              loading={managementLoading}
              error={managementError}
              action={managementAction}
              onSync={() => void handleStateSync()}
              onProbe={() => void handleCapabilityProbe()}
            />
            <QuotaDetail account={detailAccount} />
          </div>
        ) : null}
      </Modal>

      {/* 代理徽章直达的快速绑定弹窗：与 Codex / Grok 账号页共用组件 */}
      <AccountProxyQuickEditor
        account={quickProxyAccount}
        accountLabel={
          quickProxyAccount
            ? quickProxyAccount.name ||
              quickProxyAccount.email ||
              `#${quickProxyAccount.id}`
            : ""
        }
        proxies={proxyPool}
        ctx={proxyBindingCtx}
        onClose={() => setQuickProxyAccount(null)}
        onSaved={() => reload({ silent: true })}
      />

      {confirmDialog}
    </div>
  );
}

export default memo(AntigravityAccounts);

function downloadAntigravityBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}
