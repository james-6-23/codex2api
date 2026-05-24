import type { ChangeEvent, FormEvent, ReactNode } from "react";
import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import ChipInput from "../components/ChipInput";
import Modal from "../components/Modal";
import PageHeader from "../components/PageHeader";
import StateShell from "../components/StateShell";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { useDataLoader } from "../hooks/useDataLoader";
import { useToast } from "../hooks/useToast";
import type { AccountGroup, APIKeyLimits, APIKeyRow } from "../types";
import { getErrorMessage } from "../utils/error";
import { formatBeijingTime, formatRelativeTime } from "../utils/time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
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
  Copy,
  CalendarClock,
  CircleDollarSign,
  Eye,
  EyeOff,
  Fingerprint,
  KeyRound,
  LockKeyhole,
  Pencil,
  Plus,
  ShieldCheck,
  Trash2,
} from "lucide-react";

type ExpireMode = "never" | "7" | "30" | "90" | "custom";

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
  rpm: string;
  rpd: string;
  costLimit5h: string;
  costLimit7d: string;
  tokenLimit5h: string;
  tokenLimit7d: string;
}

const emptyLimitsForm: LimitsFormState = {
  modelAllow: [],
  modelDeny: [],
  rpm: "",
  rpd: "",
  costLimit5h: "",
  costLimit7d: "",
  tokenLimit5h: "",
  tokenLimit7d: "",
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
  const [visibleKeys, setVisibleKeys] = useState<Set<number>>(new Set());
  const [creating, setCreating] = useState(false);
  const [deletingIds, setDeletingIds] = useState<Set<number>>(new Set());
  const [editingKey, setEditingKey] = useState<APIKeyRow | null>(null);
  const [editForm, setEditForm] = useState<EditKeyFormState>(initialEditForm);
  const [saving, setSaving] = useState(false);
  const { toast, showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();

  const loadKeys = useCallback(async () => {
    const [keysResponse, groupsResponse, modelsResponse] = await Promise.all([
      api.getAPIKeys(),
      api.listAccountGroups().catch(() => ({ groups: [] })),
      api
        .getModels()
        .catch(() => ({ models: [] as string[] })) as Promise<{
        models?: string[];
      }>,
    ]);
    return {
      keys: keysResponse.keys ?? [],
      groups: groupsResponse.groups ?? [],
      modelOptions: modelsResponse.models ?? [],
    };
  }, []);

  const { data, loading, error, reload } = useDataLoader<{
    keys: APIKeyRow[];
    groups: AccountGroup[];
    modelOptions: string[];
  }>({
    initialData: { keys: [], groups: [], modelOptions: [] },
    load: loadKeys,
  });
  const keys = data.keys;
  const groups = data.groups;
  const modelOptions = data.modelOptions;

  const latestKey = useMemo(() => {
    return keys
      .slice()
      .sort(
        (a, b) =>
          new Date(b.created_at || 0).getTime() -
          new Date(a.created_at || 0).getTime(),
      )[0];
  }, [keys]);

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
      setVisibleKeys((prev) => new Set(prev).add(result.id));
      setCreateForm(initialCreateForm);
      setCreateDialogOpen(false);
      showToast(t("apiKeys.keyCreateSuccess"));
      void reload();
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
    try {
      await api.deleteAPIKey(id);
      showToast(t("apiKeys.keyDeleted"));
      if (createdKeyId === id) setCreatedKeyId(null);
      setVisibleKeys((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
      void reload();
    } catch (error) {
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

  const toggleVisible = (id: number) => {
    setVisibleKeys((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
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
  };

  const closeEditDialog = () => {
    if (saving) return;
    setEditingKey(null);
    setEditForm(initialEditForm);
  };

  const updateEditForm = (patch: Partial<EditKeyFormState>) => {
    setEditForm((current) => ({ ...current, ...patch }));
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
      await api.updateAPIKey(editingKey.id, {
        name: trimmed,
        quota_limit: quotaLimit,
        allowed_group_ids: editForm.allowedGroupIds,
        limits: limitsFormToPayload(editForm.limits),
        ...buildExpirationPayload(editForm, t, { clearNever: true }),
      });
      showToast(t("apiKeys.keyUpdated"));
      setEditingKey(null);
      setEditForm(initialEditForm);
      void reload();
    } catch (error) {
      showToast(
        `${t("apiKeys.updateFailed")}: ${getErrorMessage(error)}`,
        "error",
      );
    } finally {
      setSaving(false);
    }
  };

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t("apiKeys.loadingTitle")}
      loadingDescription={t("apiKeys.loadingDesc")}
      errorTitle={t("apiKeys.errorTitle")}
    >
      <>
        <PageHeader
          title={t("apiKeys.title")}
          description={t("apiKeys.description")}
          onRefresh={() => void reload()}
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

        <div className="mb-4 grid gap-3 md:grid-cols-3">
          <KeySummaryCard
            icon={<KeyRound className="size-5" />}
            label={t("apiKeys.totalKeys")}
            value={String(keys.length)}
            sub={
              keys.length > 0
                ? t("apiKeys.totalKeysDesc")
                : t("apiKeys.noKeysShort")
            }
            tone="info"
          />
          <KeySummaryCard
            icon={<ShieldCheck className="size-5" />}
            label={t("apiKeys.authMode")}
            value={
              keys.length > 0
                ? t("apiKeys.authEnabled")
                : t("apiKeys.authDisabled")
            }
            sub={
              keys.length > 0
                ? t("apiKeys.authEnabledDesc")
                : t("apiKeys.authDisabledDesc")
            }
            tone={keys.length > 0 ? "success" : "warning"}
          />
          <KeySummaryCard
            icon={<Fingerprint className="size-5" />}
            label={t("apiKeys.newestKey")}
            value={latestKey?.name || "-"}
            sub={
              latestKey
                ? formatRelativeTime(latestKey.created_at, {
                    variant: "compact",
                  })
                : t("apiKeys.noLatest")
            }
            tone="neutral"
          />
        </div>

        <div className="space-y-4">
          <Card>
            <CardContent className="p-3 sm:p-4">
              <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
                <div>
                  <h3 className="text-base font-semibold text-foreground">
                    {t("apiKeys.tableTitle")}
                  </h3>
                  <p className="mt-1 text-sm text-muted-foreground">
                    {t("apiKeys.tableDesc")}
                  </p>
                </div>
                <Badge variant={keys.length > 0 ? "default" : "secondary"}>
                  {t("apiKeys.keyCount", { count: keys.length })}
                </Badge>
              </div>

              <StateShell
                variant="section"
                isEmpty={keys.length === 0}
                emptyTitle={t("apiKeys.noKeys")}
                emptyDescription={t("apiKeys.noKeysDesc")}
              >
                <div className="data-table-shell">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("common.name")}</TableHead>
                        <TableHead>{t("apiKeys.keyColumn")}</TableHead>
                        <TableHead>{t("apiKeys.allowedGroups")}</TableHead>
                        <TableHead>{t("apiKeys.quotaColumn")}</TableHead>
                        <TableHead>{t("apiKeys.expiresColumn")}</TableHead>
                        <TableHead>{t("common.createdAt")}</TableHead>
                        <TableHead className="text-right">
                          {t("common.actions")}
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {keys.map((keyRow) => {
                        const isVisible = visibleKeys.has(keyRow.id);
                        const isNew = createdKeyId === keyRow.id;
                        const displayKey = isVisible
                          ? keyRow.raw_key || keyRow.key
                          : keyRow.key;
                        const copyValue = keyRow.raw_key || keyRow.key;
                        const status = getAPIKeyStatus(keyRow);
                        return (
                          <TableRow
                            key={keyRow.id}
                            className={
                              isNew ? "bg-[hsl(var(--success-bg))]" : ""
                            }
                          >
                            <TableCell className="font-medium text-foreground">
                              <div className="flex items-center gap-2">
                                <span>{keyRow.name}</span>
                                {isNew ? (
                                  <Badge
                                    variant="outline"
                                    className="border-transparent bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]"
                                  >
                                    {t("apiKeys.newBadge")}
                                  </Badge>
                                ) : null}
                                {status !== "active" ? (
                                  <Badge
                                    variant={
                                      status === "expired"
                                        ? "secondary"
                                        : "destructive"
                                    }
                                  >
                                    {t(`apiKeys.status.${status}`)}
                                  </Badge>
                                ) : null}
                              </div>
                            </TableCell>
                            <TableCell>
                              <div className="flex min-w-[260px] items-center gap-2">
                                <code
                                  className="min-w-0 max-w-[420px] truncate rounded-md bg-muted px-2 py-1 font-mono text-[13px] text-foreground"
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
                                  onClick={() => void handleCopy(copyValue)}
                                  title={t("common.copy")}
                                >
                                  <Copy className="size-3.5" />
                                </Button>
                              </div>
                            </TableCell>
                            <TableCell className="min-w-[180px]">
                              <AllowedGroupsDisplay
                                ids={keyRow.allowed_group_ids ?? []}
                                groups={groups}
                                t={t}
                              />
                            </TableCell>
                            <TableCell className="min-w-[150px] text-sm text-muted-foreground">
                              <div className="space-y-1">
                                <div className="font-medium text-foreground">
                                  {formatQuotaLimit(keyRow, t)}
                                </div>
                                {keyRow.quota_limit > 0 ? (
                                  <div className="h-1.5 w-28 overflow-hidden rounded-full bg-muted">
                                    <div
                                      className="h-full rounded-full bg-primary"
                                      style={{
                                        width: `${Math.min(100, Math.max(0, (keyRow.quota_used / keyRow.quota_limit) * 100))}%`,
                                      }}
                                    />
                                  </div>
                                ) : null}
                              </div>
                            </TableCell>
                            <TableCell className="text-muted-foreground">
                              {formatExpiration(keyRow, t)}
                            </TableCell>
                            <TableCell className="text-muted-foreground">
                              {formatRelativeTime(keyRow.created_at, {
                                variant: "compact",
                              })}
                            </TableCell>
                            <TableCell>
                              <div className="flex justify-end gap-1.5">
                                <Button
                                  variant="outline"
                                  size="sm"
                                  onClick={() => startEditing(keyRow)}
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
                                >
                                  <Trash2 className="size-3.5" />
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
              </StateShell>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:justify-between">
              <div className="flex min-w-0 items-start gap-3">
                <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                  <LockKeyhole className="size-4" />
                </div>
                <div className="min-w-0">
                  <div className="text-sm font-semibold text-foreground">
                    {t("apiKeys.securityTitle")}
                  </div>
                  <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
                    {t("apiKeys.keyAuthNote")}
                  </p>
                </div>
              </div>
              <Button
                variant="outline"
                onClick={() => setCreateDialogOpen(true)}
                className="shrink-0"
              >
                <Plus className="size-3.5" />
                {t("apiKeys.createKey")}
              </Button>
            </CardContent>
          </Card>
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
          onClose={closeEditDialog}
          contentClassName="sm:max-w-[640px]"
          footer={
            <>
              <Button
                type="button"
                variant="outline"
                onClick={closeEditDialog}
                disabled={saving}
              >
                {t("common.cancel")}
              </Button>
              <Button
                type="submit"
                form="edit-api-key-form"
                disabled={saving || !editForm.name.trim()}
              >
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

              <LimitsEditor
                value={editForm.limits}
                onChange={(limits) => updateEditForm({ limits })}
                modelOptions={modelOptions}
              />
            </form>
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
  return {
    modelAllow: Array.isArray(limits.model_allow) ? limits.model_allow : [],
    modelDeny: Array.isArray(limits.model_deny) ? limits.model_deny : [],
    rpm: limits.rpm && limits.rpm > 0 ? String(limits.rpm) : "",
    rpd: limits.rpd && limits.rpd > 0 ? String(limits.rpd) : "",
    costLimit5h:
      limits.cost_limit_5h && limits.cost_limit_5h > 0
        ? String(limits.cost_limit_5h)
        : "",
    costLimit7d:
      limits.cost_limit_7d && limits.cost_limit_7d > 0
        ? String(limits.cost_limit_7d)
        : "",
    tokenLimit5h:
      limits.token_limit_5h && limits.token_limit_5h > 0
        ? String(limits.token_limit_5h)
        : "",
    tokenLimit7d:
      limits.token_limit_7d && limits.token_limit_7d > 0
        ? String(limits.token_limit_7d)
        : "",
  };
}

// limitsFormToPayload 把表单值转为后端期望的 APIKeyLimits。
// 空字符串或 0 在后端被视为 "未配置";所以不一一过滤,直接把全部字段都发出去。
// (sanitizeAPIKeyLimits 在后端会把负值与空白清理掉)
function limitsFormToPayload(form: LimitsFormState): APIKeyLimits {
  const num = (s: string) => {
    const n = Number(s.trim());
    return Number.isFinite(n) && n > 0 ? n : 0;
  };
  return {
    model_allow: form.modelAllow.map((m) => m.trim()).filter(Boolean),
    model_deny: form.modelDeny.map((m) => m.trim()).filter(Boolean),
    rpm: num(form.rpm),
    rpd: num(form.rpd),
    cost_limit_5h: num(form.costLimit5h),
    cost_limit_7d: num(form.costLimit7d),
    token_limit_5h: num(form.tokenLimit5h),
    token_limit_7d: num(form.tokenLimit7d),
  };
}

function toDateTimeLocalValue(value?: string | null) {
  if (!value) return "";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "";
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000);
  return local.toISOString().slice(0, 16);
}

function getAPIKeyStatus(
  keyRow: APIKeyRow,
): "active" | "expired" | "quota_exhausted" {
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

function formatUSD(value: number) {
  if (!Number.isFinite(value)) return "$0";
  if (value >= 1) return `$${value.toFixed(2)}`;
  if (value >= 0.01) return `$${value.toFixed(4)}`;
  return `$${value.toFixed(6)}`;
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

// LimitsEditor 渲染 API Key 的"高级限制"配置:模型白/黑名单 + 滑动窗口配额。
// 默认折叠,有任一字段非默认时展开。
function LimitsEditor({
  value,
  onChange,
  modelOptions,
}: {
  value: LimitsFormState;
  onChange: (next: LimitsFormState) => void;
  modelOptions: string[];
}) {
  const { t } = useTranslation();
  const hasAny =
    value.modelAllow.length > 0 ||
    value.modelDeny.length > 0 ||
    value.rpm !== "" ||
    value.rpd !== "" ||
    value.costLimit5h !== "" ||
    value.costLimit7d !== "" ||
    value.tokenLimit5h !== "" ||
    value.tokenLimit7d !== "";
  const [open, setOpen] = useState(hasAny);

  const patch = (next: Partial<LimitsFormState>) =>
    onChange({ ...value, ...next });

  return (
    <div className="rounded-lg border border-border">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center justify-between px-3 py-2.5 text-left text-sm font-medium hover:bg-muted/30 transition-colors"
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
      {open && (
        <div className="space-y-3 border-t border-border p-3">
          <p className="text-[11px] text-muted-foreground">
            {t("apiKeys.limits.desc")}
          </p>
          <div className="space-y-2">
            <label className="text-xs font-medium">
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
          <div className="space-y-2">
            <label className="text-xs font-medium">
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
          <div className="grid grid-cols-2 gap-3">
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
              label={t("apiKeys.limits.tokens5h")}
              value={value.tokenLimit5h}
              onChange={(tokenLimit5h) => patch({ tokenLimit5h })}
              suffix="tk"
            />
            <LimitNumberField
              label={t("apiKeys.limits.tokens7d")}
              value={value.tokenLimit7d}
              onChange={(tokenLimit7d) => patch({ tokenLimit7d })}
              suffix="tk"
            />
          </div>
        </div>
      )}
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

function KeySummaryCard({
  icon,
  label,
  value,
  sub,
  tone,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  sub: string;
  tone: "neutral" | "info" | "success" | "warning";
}) {
  const toneClassName = {
    neutral: "bg-muted text-muted-foreground",
    info: "bg-primary/10 text-primary",
    success: "bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]",
    warning: "bg-[hsl(var(--warning-bg))] text-[hsl(var(--warning))]",
  }[tone];

  return (
    <Card className="py-0">
      <CardContent className="flex items-center justify-between gap-3 p-4">
        <div className="min-w-0">
          <div className="text-[11px] font-bold uppercase text-muted-foreground">
            {label}
          </div>
          <div className="mt-2 truncate text-[24px] font-bold leading-none text-foreground">
            {value}
          </div>
          <div className="mt-2 truncate text-xs text-muted-foreground">
            {sub}
          </div>
        </div>
        <div
          className={`flex size-10 shrink-0 items-center justify-center rounded-lg ${toneClassName}`}
        >
          {icon}
        </div>
      </CardContent>
    </Card>
  );
}
