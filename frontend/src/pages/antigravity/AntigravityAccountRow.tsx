import { memo, useEffect, useState } from "react";
import type { MouseEvent } from "react";
import { useTranslation } from "react-i18next";
import {
  Activity,
  BarChart3,
  CheckCircle2,
  CircleGauge,
  Coins,
  Download,
  Hourglass,
  Loader2,
  Lock,
  MoreHorizontal,
  Pencil,
  Power,
  PowerOff,
  RotateCcw,
  RotateCw,
  Sparkles,
  Trash2,
  Unlock,
  Zap,
} from "lucide-react";
import type { AccountGroup, AccountHealthBucket, AccountRow } from "../../types";
import AccountHealthBar from "../../components/AccountHealthBar";
import AccountProxyBadge from "../../components/AccountProxyBadge";
import { HeaderActionMenu, type HeaderActionMenuItem } from "../../components/HeaderActionMenu";
import RequestCountPills from "../../components/RequestCountPills";
import StatusBadge from "../../components/StatusBadge";
import type { ProxyBindingContext } from "../../lib/accountProxyBinding";
import { getAccountStatusBadgeStatus } from "../../lib/usageFormat";
import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { cn } from "@/lib/utils";
import {
  accountLabel,
  identityProjectID,
  identityVerified,
  subscriptionTier,
} from "../../lib/antigravityQuota";
import { formatBeijingTime, formatRelativeTime } from "../../utils/time";
import { AntigravityQuotaCell } from "./AntigravityQuotaCells";
import {
  AccountAvatar,
  AntigravityAuthKindChip,
  PermissionBadge,
  SyncWarningAction,
} from "./AntigravityStatePanel";

export const ANTIGRAVITY_TABLE_COLUMNS = [
  "id", "tier", "quota", "proxy", "status", "today", "requests", "updatedAt",
] as const;

export type AntigravityTableColumn = (typeof ANTIGRAVITY_TABLE_COLUMNS)[number];

export type AntigravityRowBusyAction = "sync" | "toggle" | "lock" | "reset" | "delete";

export interface AntigravityRowProps {
  account: AccountRow;
  sequence: number;
  selected: boolean;
  detailOpen: boolean;
  columns: Record<AntigravityTableColumn, boolean>;
  groups: AccountGroup[];
  healthBuckets?: AccountHealthBucket[];
  proxyCtx: ProxyBindingContext;
  busyAction: AntigravityRowBusyAction | null;
  exporting: boolean;
  onToggleSelect: (id: number) => void;
  onOpenDetail: (account: AccountRow) => void;
  /** 打开"额度与权限"宽版窗口(API Key 号没有配额快照,回落到详情抽屉)。 */
  onOpenQuota: (account: AccountRow) => void;
  onTest: (account: AccountRow) => void;
  onSync: (account: AccountRow) => void;
  onEdit: (account: AccountRow) => void;
  onUsage: (account: AccountRow) => void;
  onExport: (account: AccountRow) => void;
  onToggleEnabled: (account: AccountRow) => void;
  onToggleLock: (account: AccountRow) => void;
  onResetStatus: (account: AccountRow) => void;
  onEditProxy: (account: AccountRow) => void;
  onDelete: (account: AccountRow) => void;
}

// 行点击打开详情;命中按钮、链接、输入框、菜单(含 portal 里的菜单项)时交给它们自己处理。
const ROW_INTERACTIVE_SELECTOR =
  'button, a, input, label, [role="menuitem"], [role="menu"], [data-slot="button"]';

function isInteractiveTarget(event: MouseEvent<HTMLElement>): boolean {
  const target = event.target as HTMLElement | null;
  return Boolean(target?.closest(ROW_INTERACTIVE_SELECTOR));
}

function formatCompactNum(value: unknown): string {
  const n = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(Math.round(n));
}

function CooldownCountdown({ until }: { until?: string }) {
  const { t } = useTranslation();
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!until) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [until]);
  if (!until) return null;
  const target = new Date(until).getTime();
  if (!Number.isFinite(target)) return null;
  const remain = Math.max(0, Math.floor((target - now) / 1000));
  if (remain <= 0) return null;
  const d = Math.floor(remain / 86400);
  const h = Math.floor((remain % 86400) / 3600);
  const m = Math.floor((remain % 3600) / 60);
  const s = remain % 60;
  const text = d > 0 ? `${d}d${h}h` : h > 0 ? `${h}h${m}m` : m > 0 ? `${m}m${s}s` : `${s}s`;
  return (
    <span
      className="inline-flex h-6 shrink-0 items-center gap-1 rounded-full bg-amber-50 px-2 font-mono text-[11px] leading-none tabular-nums text-amber-700 ring-1 ring-inset ring-amber-200/70 dark:bg-amber-950/40 dark:text-amber-300 dark:ring-amber-400/20"
      title={`${t("antigravity.cooldownUntil")} ${formatBeijingTime(until)}`}
    >
      <Hourglass className="size-3 shrink-0" aria-hidden />
      {text}
    </span>
  );
}

function ModelCooldownChip({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const cooldowns = account.model_cooldowns ?? [];
  if (cooldowns.length === 0) return null;
  const title = cooldowns
    .map((cooldown) => `${cooldown.model} · ${cooldown.reason} · ${cooldown.remaining_seconds}s`)
    .join("\n");
  return (
    <span
      className="inline-flex h-6 shrink-0 items-center gap-1 rounded-full bg-amber-500/10 px-2 text-[11px] font-medium text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:text-amber-300"
      title={title}
    >
      <Hourglass className="size-3 shrink-0" aria-hidden />
      {t("antigravity.modelCooldownCount", { count: cooldowns.length })}
    </span>
  );
}

function TodayStatsCell({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  const detail = account.usage_today_detail;
  if (!detail) return <span className="font-mono text-[12px] text-muted-foreground/40">-</span>;
  const requests = detail.requests ?? 0;
  const tokens = detail.tokens ?? 0;
  const billed = typeof detail.account_billed === "number" ? detail.account_billed : 0;
  const tooltip = [
    `${t("accounts.todayStats")}:`,
    `Requests: ${requests.toLocaleString()} req`,
    `Tokens: ${tokens.toLocaleString()} tok`,
    `${t("accounts.accountBilledLabel")}: $${billed.toFixed(4)}`,
  ].join("\n");
  return (
    <div className="flex flex-col items-start gap-1 whitespace-nowrap text-[12px] tabular-nums" title={tooltip}>
      <div className="flex items-center gap-2">
        <span className={cn("inline-flex items-center gap-1", requests > 0 ? "font-semibold text-foreground" : "font-normal text-muted-foreground/50")}>
          <Activity className={cn("size-3 shrink-0", requests > 0 ? "text-sky-500" : "text-muted-foreground/40")} aria-hidden />
          <span>{requests > 0 ? requests.toLocaleString() : 0}</span>
          <span className="text-[11px] font-normal text-muted-foreground/60">{t("accounts.usageReqUnit")}</span>
        </span>
        <span className={cn("inline-flex items-center gap-1", tokens > 0 ? "font-semibold text-foreground" : "font-normal text-muted-foreground/50")}>
          <Sparkles className={cn("size-3 shrink-0", tokens > 0 ? "text-purple-500 dark:text-purple-400" : "text-muted-foreground/40")} aria-hidden />
          <span>{formatCompactNum(tokens)}</span>
          <span className="text-[11px] font-normal text-muted-foreground/60">{t("accounts.usageTokUnit")}</span>
        </span>
      </div>
      {billed > 0 ? (
        <span
          className="inline-flex items-center gap-1 rounded-md bg-emerald-500/10 px-1.5 py-0.5 font-mono text-[11px] font-medium tabular-nums text-emerald-700 ring-1 ring-inset ring-emerald-500/20 dark:text-emerald-400"
          title={t("accounts.accountBilledLabel")}
        >
          <Coins className="size-3 shrink-0 text-emerald-500" aria-hidden />${billed < 0.01 ? "<0.01" : billed.toFixed(2)}
        </span>
      ) : null}
    </div>
  );
}

function GroupChips({ account, groups }: { account: AccountRow; groups: AccountGroup[] }) {
  const ids = account.group_ids ?? [];
  if (ids.length === 0) return null;
  const byID = new Map(groups.map((group) => [group.id, group]));
  const resolved = ids.map((id) => byID.get(id)).filter(Boolean) as AccountGroup[];
  if (resolved.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-1">
      {resolved.slice(0, 3).map((group) => {
        const color = /^#[0-9a-fA-F]{6}$/.test(group.color || "") ? group.color! : "#2563eb";
        return (
          <span
            key={group.id}
            className="inline-flex max-w-28 items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-medium"
            style={{ backgroundColor: `${color}14`, color, boxShadow: `inset 0 0 0 1px ${color}33` }}
            title={group.description || group.name}
          >
            <span className="size-1.5 shrink-0 rounded-full bg-current" />
            <span className="truncate">{group.name}</span>
          </span>
        );
      })}
      {resolved.length > 3 ? (
        <span className="rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
          +{resolved.length - 3}
        </span>
      ) : null}
    </div>
  );
}

function AccountIdentity({
  account,
  groups,
  onOpenDetail,
  avatarSize,
}: {
  account: AccountRow;
  groups: AccountGroup[];
  onOpenDetail: () => void;
  avatarSize: number;
}) {
  const { t } = useTranslation();
  return (
    <div className="flex min-w-0 items-center gap-2.5">
      <AccountAvatar account={account} size={avatarSize} />
      <div className="flex min-w-0 flex-col items-start gap-1">
        <span className="flex max-w-full flex-wrap items-center gap-1.5">
          <button
            type="button"
            onClick={(event) => {
              event.stopPropagation();
              onOpenDetail();
            }}
            className="max-w-[220px] truncate text-left text-[13px] font-semibold text-foreground transition-colors hover:text-primary"
            title={t("accounts.openDetail")}
          >
            {accountLabel(account)}
          </button>
          {identityVerified(account) ? (
            <CheckCircle2 className="size-3.5 shrink-0 text-emerald-500" aria-label={t("antigravity.verified")} />
          ) : null}
        </span>
        {account.name && account.email ? (
          <span className="max-w-[220px] truncate text-[11px] text-muted-foreground">{account.email}</span>
        ) : null}
        <span className="flex flex-wrap items-center gap-1">
          <AntigravityAuthKindChip account={account} />
          {account.locked ? (
            <span className="inline-flex items-center rounded-md bg-blue-50 px-1.5 py-0.5 text-[10px] font-medium text-blue-700 ring-1 ring-inset ring-blue-600/20 dark:bg-blue-950 dark:text-blue-400 dark:ring-blue-400/20">
              <Lock className="mr-0.5 size-2.5" />
              {t("accounts.lock")}
            </span>
          ) : null}
          {account.antigravity_auth_kind === "api_key" && account.models?.length ? (
            <span className="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
              {t("antigravity.modelsCount", { count: account.models.length })}
            </span>
          ) : null}
        </span>
        <GroupChips account={account} groups={groups} />
      </div>
    </div>
  );
}

function TierCell({ account }: { account: AccountRow }) {
  const { t } = useTranslation();
  if (account.antigravity_auth_kind === "api_key") {
    return <span className="text-[12px] text-muted-foreground">{t("antigravity.apiKeyTier")}</span>;
  }
  const projectID = identityProjectID(account);
  return (
    <div className="min-w-[150px] max-w-[220px] space-y-1">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="truncate text-[13px] font-medium text-foreground">
          {subscriptionTier(account) || t("antigravity.tierUnknown")}
        </span>
        <PermissionBadge permissions={account.antigravity_permissions} />
      </div>
      <div className="truncate font-mono text-[11px] text-muted-foreground" title={projectID}>
        {projectID || t("antigravity.projectPending")}
      </div>
      {account.antigravity_permissions?.reason ? (
        <div className="truncate text-[11px] text-muted-foreground" title={account.antigravity_permissions.reason}>
          {account.antigravity_permissions.reason}
        </div>
      ) : null}
      <SyncWarningAction warning={account.antigravity_sync_warning} />
    </div>
  );
}

function StatusCell({ account, healthBuckets }: { account: AccountRow; healthBuckets?: AccountHealthBucket[] }) {
  return (
    <div className="min-w-[150px] max-w-[240px] space-y-1.5">
      <div className="flex min-h-6 flex-wrap items-center gap-1.5">
        <StatusBadge
          status={account.enabled === false ? "paused" : getAccountStatusBadgeStatus(account)}
          errorMessage={account.error_message}
        />
        <CooldownCountdown until={account.cooldown_until} />
        <ModelCooldownChip account={account} />
      </div>
      <AccountHealthBar buckets={healthBuckets} />
    </div>
  );
}

function useRowMenuItems(props: AntigravityRowProps): HeaderActionMenuItem[] {
  const { t } = useTranslation();
  const { account, busyAction, exporting } = props;
  const busy = busyAction !== null;
  const disabled = account.enabled === false;
  const isAPIKey = account.antigravity_auth_kind === "api_key";
  return [
    ...(isAPIKey
      ? []
      : [
          {
            key: "quota",
            label: t("antigravity.quotaDetailTitle"),
            icon: <CircleGauge className="size-3.5" />,
            onSelect: () => props.onOpenQuota(account),
          },
        ]),
    {
      key: "usage",
      label: t("accounts.usageDetail"),
      icon: <BarChart3 className="size-3.5" />,
      onSelect: () => props.onUsage(account),
    },
    {
      key: "export",
      label: t("antigravity.exportOne"),
      icon: <Download className="size-3.5" />,
      disabled: exporting,
      onSelect: () => props.onExport(account),
    },
    {
      key: "toggle",
      label: disabled ? t("antigravity.enableAccount") : t("antigravity.disableAccount"),
      icon: disabled ? <Power className="size-3.5" /> : <PowerOff className="size-3.5" />,
      disabled: busy,
      onSelect: () => props.onToggleEnabled(account),
    },
    {
      key: "lock",
      label: account.locked ? t("accounts.unlock") : t("accounts.lock"),
      icon: account.locked ? <Unlock className="size-3.5" /> : <Lock className="size-3.5" />,
      disabled: busy,
      onSelect: () => props.onToggleLock(account),
    },
    {
      key: "reset",
      label: t("accounts.resetStatus"),
      icon: <RotateCcw className="size-3.5" />,
      disabled: busy,
      onSelect: () => props.onResetStatus(account),
    },
    {
      key: "delete",
      label: t("common.delete"),
      icon: <Trash2 className="size-3.5" />,
      destructive: true,
      disabled: busy,
      onSelect: () => props.onDelete(account),
    },
  ];
}

function RowActions(props: AntigravityRowProps) {
  const { t } = useTranslation();
  const { account, busyAction } = props;
  const menuItems = useRowMenuItems(props);
  const isAPIKey = account.antigravity_auth_kind === "api_key";
  const busy = busyAction !== null;
  return (
    <div className="flex items-center justify-end gap-0.5">
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        disabled={busy}
        onClick={() => props.onTest(account)}
        title={t("accounts.testConnection")}
        aria-label={t("accounts.testConnection")}
      >
        <Zap className="size-3.5" />
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        disabled={busy || isAPIKey}
        onClick={() => props.onSync(account)}
        title={isAPIKey ? t("antigravity.syncUnavailableApiKey") : t("antigravity.syncAccount")}
        aria-label={t("antigravity.syncAccount")}
      >
        {busyAction === "sync" ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-8"
        disabled={busy}
        onClick={() => props.onEdit(account)}
        title={t("common.edit")}
        aria-label={t("common.edit")}
      >
        <Pencil className="size-3.5" />
      </Button>
      <HeaderActionMenu
        label={t("accounts.rowActions")}
        icon={busy && busyAction !== "sync" ? <Loader2 className="size-3.5 animate-spin" /> : <MoreHorizontal className="size-3.5" />}
        align="end"
        compact
        items={menuItems}
        triggerVariant="ghost"
      />
    </div>
  );
}

export const AntigravityAccountRow = memo(function AntigravityAccountRow(props: AntigravityRowProps) {
  const { account, columns } = props;
  return (
    <TableRow
      data-state={props.selected ? "selected" : undefined}
      className={cn(
        "cursor-pointer",
        account.enabled === false && "opacity-65",
        props.detailOpen && "bg-primary/[0.04]",
      )}
      onClick={(event) => {
        if (isInteractiveTarget(event)) return;
        props.onOpenDetail(account);
      }}
    >
      <TableCell className="w-10">
        <input
          type="checkbox"
          className="size-4 cursor-pointer accent-primary"
          checked={props.selected}
          onChange={() => props.onToggleSelect(account.id)}
          aria-label={accountLabel(account)}
        />
      </TableCell>
      <TableCell className="font-mono text-[13px] text-muted-foreground" title={`ID ${account.id}`}>
        {props.sequence}
      </TableCell>
      {columns.id ? (
        <TableCell className="font-mono text-[13px] tabular-nums text-muted-foreground">{account.id}</TableCell>
      ) : null}
      <TableCell className="min-w-[220px]">
        <AccountIdentity
          account={account}
          groups={props.groups}
          avatarSize={32}
          onOpenDetail={() => props.onOpenDetail(account)}
        />
      </TableCell>
      {columns.tier ? (
        <TableCell>
          <TierCell account={account} />
        </TableCell>
      ) : null}
      {columns.quota ? (
        <TableCell>
          <AntigravityQuotaCell account={account} onOpen={() => props.onOpenQuota(account)} />
        </TableCell>
      ) : null}
      {columns.proxy ? (
        <TableCell className="min-w-[120px] max-w-[180px]">
          <AccountProxyBadge account={account} ctx={props.proxyCtx} onClick={() => props.onEditProxy(account)} />
        </TableCell>
      ) : null}
      {columns.status ? (
        <TableCell>
          <StatusCell account={account} healthBuckets={props.healthBuckets} />
        </TableCell>
      ) : null}
      {columns.today ? (
        <TableCell>
          <TodayStatsCell account={account} />
        </TableCell>
      ) : null}
      {columns.requests ? (
        <TableCell>
          <RequestCountPills account={account} compact />
        </TableCell>
      ) : null}
      {columns.updatedAt ? (
        <TableCell className="whitespace-nowrap">
          <div className="text-[13px] tabular-nums text-muted-foreground" title={formatBeijingTime(account.updated_at)}>
            {formatRelativeTime(account.updated_at, { variant: "compact" })}
          </div>
        </TableCell>
      ) : null}
      <TableCell data-account-actions className="text-right">
        <RowActions {...props} />
      </TableCell>
    </TableRow>
  );
});

export const AntigravityAccountCard = memo(function AntigravityAccountCard(props: AntigravityRowProps) {
  const { t } = useTranslation();
  const { account } = props;
  return (
    <article
      className={cn(
        "rounded-xl border bg-card p-3 shadow-sm transition-colors",
        props.selected ? "border-primary/40 ring-1 ring-primary/20" : "border-border",
        account.enabled === false && "opacity-65",
      )}
      onClick={(event) => {
        if (isInteractiveTarget(event)) return;
        props.onOpenDetail(account);
      }}
    >
      <div className="flex items-start gap-2.5">
        <input
          type="checkbox"
          className="mt-2.5 size-4 shrink-0 cursor-pointer accent-primary"
          checked={props.selected}
          onChange={() => props.onToggleSelect(account.id)}
          aria-label={accountLabel(account)}
        />
        <div className="min-w-0 flex-1">
          <AccountIdentity
            account={account}
            groups={props.groups}
            avatarSize={40}
            onOpenDetail={() => props.onOpenDetail(account)}
          />
        </div>
        <span className="shrink-0 font-mono text-[11px] text-muted-foreground">#{props.sequence}</span>
        {props.columns.id ? (
          <span className="shrink-0 font-mono text-[11px] text-muted-foreground">ID {account.id}</span>
        ) : null}
      </div>
      <div className="mt-3 grid grid-cols-1 gap-3 border-t border-border pt-3 text-xs">
        <StatusCell account={account} healthBuckets={props.healthBuckets} />
        {account.antigravity_auth_kind !== "api_key" ? (
          <div className="min-w-0 space-y-1">
            <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
              {t("antigravity.columnTier")}
            </div>
            <TierCell account={account} />
          </div>
        ) : null}
        <div className="min-w-0 space-y-1">
          <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            {t("antigravity.columnQuota")}
          </div>
          <AntigravityQuotaCell account={account} onOpen={() => props.onOpenQuota(account)} />
        </div>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <TodayStatsCell account={account} />
          <AccountProxyBadge account={account} ctx={props.proxyCtx} onClick={() => props.onEditProxy(account)} />
        </div>
      </div>
      <div className="mt-3 flex items-center justify-between gap-2 border-t border-border pt-2">
        <span className="text-[10px] text-muted-foreground" title={formatBeijingTime(account.updated_at)}>
          {formatRelativeTime(account.updated_at, { variant: "compact" })}
        </span>
        <RowActions {...props} />
      </div>
    </article>
  );
});
