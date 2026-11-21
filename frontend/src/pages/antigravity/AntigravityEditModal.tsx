import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Boxes,
  Flame,
  Gauge,
  Globe,
  KeyRound,
  Loader2,
  Network,
  Pencil,
  RotateCw,
  ShieldAlert,
  ShieldCheck,
  Sliders,
  Tag,
  Timer,
  UserRound,
  Zap,
} from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type {
  AccountGroup,
  AccountRow,
  AntigravityAuthKind,
  APIKeyRow,
  UpdateAntigravityAccountRequest,
} from "../../types";
import Modal from "../../components/Modal";
import { ConfigCategory } from "../../components/AccountConfigCards";
import APIKeyMultiSelect from "../../components/APIKeyMultiSelect";
import ChipInput from "../../components/ChipInput";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { useToast } from "../../hooks/useToast";
import { ANTIGRAVITY_DEFAULT_MODELS } from "../../lib/antigravityModels";
import { accountLabel, parseModelList } from "../../lib/antigravityQuota";
import {
  buildSchedulerPatch,
  schedulerDraftFromAccount,
  type AntigravitySchedulerDraft,
  type AntigravitySchedulerError,
} from "../../lib/antigravityScheduler";
import { getErrorMessage } from "../../utils/error";
import { formatBeijingTime } from "../../utils/time";
import {
  AccountMetadataFields,
  APIKeyModelFields,
  FieldHint,
  FieldLabel,
  FormSection,
  SecretInput,
  TEXTAREA_CLASS,
} from "./AntigravityFormFields";
import { AccountAvatar, AntigravityAuthKindChip } from "./AntigravityStatePanel";

interface EditDraft {
  name: string;
  authKind: AntigravityAuthKind;
  authJson: string;
  apiKey: string;
  models: string;
  modelMapping: string;
  proxyUrl: string;
  groupIds: number[];
  userProjectId: string;
}

const EMPTY_EDIT_DRAFT: EditDraft = {
  name: "",
  authKind: "oauth",
  authJson: "",
  apiKey: "",
  models: "",
  modelMapping: "",
  proxyUrl: "",
  groupIds: [],
  userProjectId: "",
};

const SCHEDULER_ERROR_KEYS: Record<AntigravitySchedulerError, string> = {
  score: "accounts.schedulerScoreRange",
  concurrency: "accounts.schedulerConcurrencyRange",
  priority: "accounts.schedulerPriorityRange",
  dispatchLimit: "accounts.dispatchCountLimitRange",
  autoPause: "accounts.autoPauseThresholdRange",
};

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
    userProjectId: account.antigravity_user_project_id ?? "",
  };
}

/**
 * 编辑 Antigravity 账号。列表行是精简投影(detail_loaded=false),带密钥配置的
 * 字段(模型映射等)必须先拉完整详情再初始化草稿,否则保存会把它们清空。
 */
export default function AntigravityEditModal({
  account,
  proxies,
  groups,
  onCreateGroup,
  onClose,
  onSaved,
}: {
  account: AccountRow | null;
  proxies: ProxyRow[];
  groups: AccountGroup[];
  onCreateGroup: (name: string) => Promise<number | null>;
  onClose: () => void;
  onSaved: () => Promise<void> | void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [editingAccount, setEditingAccount] = useState<AccountRow | null>(null);
  const [editDraft, setEditDraft] = useState<EditDraft>(EMPTY_EDIT_DRAFT);
  const [schedulerDraft, setSchedulerDraft] = useState<AntigravitySchedulerDraft | null>(null);
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([]);
  const [editing, setEditing] = useState(false);
  const [editingDetailsLoading, setEditingDetailsLoading] = useState(false);
  const [editingDetailsError, setEditingDetailsError] = useState<string | null>(null);
  const editLoadGenerationRef = useRef(0);

  const openEditor = async (account: AccountRow) => {
    const generation = editLoadGenerationRef.current + 1;
    editLoadGenerationRef.current = generation;
    setEditingAccount(account);
    setEditingDetailsError(null);

    if (account.detail_loaded !== false) {
      setEditDraft(editDraftFromAccount(account));
      setSchedulerDraft(schedulerDraftFromAccount(account));
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
      setSchedulerDraft(schedulerDraftFromAccount(detail));
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

  // 父组件每次换一个编辑目标就重新 hydrate;openEditor 自身用 generation 防串。
  useEffect(() => {
    if (account) {
      void openEditor(account);
    } else {
      editLoadGenerationRef.current += 1;
      setEditingAccount(null);
      setEditingDetailsLoading(false);
      setEditingDetailsError(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [account]);

  // 「允许调用 Token」的候选列表;失败时按空列表处理(等同全部 Key 可调用)。
  const isOpen = account !== null;
  useEffect(() => {
    if (!isOpen) return;
    let cancelled = false;
    void api
      .getAPIKeys({ view: "lite" })
      .then((res) => {
        if (!cancelled) setAPIKeys(res.keys ?? []);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [isOpen]);

  const closeEditor = useCallback(() => {
    if (editing) return;
    editLoadGenerationRef.current += 1;
    onClose();
  }, [editing, onClose]);

  const handleEdit = async () => {
    if (!editingAccount || !schedulerDraft || editing || editingDetailsLoading || editingDetailsError) return;
    const scheduler = buildSchedulerPatch(editingAccount, schedulerDraft);
    if ("error" in scheduler) {
      showToast(t(SCHEDULER_ERROR_KEYS[scheduler.error]), "error");
      return;
    }
    const payload: UpdateAntigravityAccountRequest = {
      name: editDraft.name.trim(),
      proxy_url: editDraft.proxyUrl.trim(),
      group_ids: editDraft.groupIds,
    };
    if (editDraft.authKind === "oauth" && editDraft.authJson.trim()) {
      payload.auth_json = editDraft.authJson.trim();
    }
    const userProjectId = editDraft.userProjectId.trim();
    if (editDraft.authKind === "oauth" && userProjectId !== (editingAccount.antigravity_user_project_id ?? "")) {
      payload.user_project_id = userProjectId;
    }
    if (editDraft.authKind === "api_key") {
      if (editDraft.apiKey.trim()) payload.api_key = editDraft.apiKey.trim();
      payload.models = parseModelList(editDraft.models);
      payload.model_mapping = editDraft.modelMapping.trim();
    }
    setEditing(true);
    try {
      const result = await api.updateAntigravityAccount(editingAccount.id, payload);
      if (Object.keys(scheduler.patch).length > 0) {
        await api.updateAccountScheduler(editingAccount.id, scheduler.patch);
      }
      showToast(result.warning || t("antigravity.editSuccess"), result.warning ? "warning" : "success");
      editLoadGenerationRef.current += 1;
      onClose();
      await onSaved();
    } catch (editError) {
      showToast(getErrorMessage(editError), "error");
    } finally {
      setEditing(false);
    }
  };

  return (
    <Modal
      show={account !== null}
      title={
        editingAccount ? (
          <span className="flex min-w-0 items-center gap-2.5">
            <AccountAvatar account={editingAccount} size={28} />
            <span className="min-w-0">
              <span className="block truncate">{t("antigravity.editTitle")}</span>
              <span className="mt-0.5 flex items-center gap-1.5 text-xs font-normal text-muted-foreground">
                <span className="truncate">{accountLabel(editingAccount)}</span>
                <AntigravityAuthKindChip account={editingAccount} />
              </span>
            </span>
          </span>
        ) : (
          t("antigravity.editTitle")
        )
      }
      onClose={closeEditor}
      contentClassName="sm:max-w-[760px]"
      footer={
        <>
          <Button variant="outline" onClick={closeEditor} disabled={editing}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => void handleEdit()}
            disabled={editing || editingDetailsLoading || Boolean(editingDetailsError)}
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
          <p>{t("antigravity.editDetailsLoadFailed", { error: editingDetailsError })}</p>
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
        <div className="space-y-6">
          <ConfigCategory icon={UserRound} title={t("antigravity.editSectionBasic")}>
            <FormSection icon={UserRound} iconClassName="text-blue-500" title={t("antigravity.accountName")} wide>
              <Input
                value={editDraft.name}
                onChange={(event) => setEditDraft((current) => ({ ...current, name: event.target.value }))}
              />
            </FormSection>

            <FormSection
              icon={KeyRound}
              iconClassName="text-emerald-500"
              title={t("antigravity.editSectionCredential")}
              description={t("antigravity.editSectionCredentialHint")}
              wide
            >
              {editDraft.authKind === "oauth" ? (
                <label className="block space-y-1.5">
                  <FieldLabel>{t("antigravity.replacementAuthJson")}</FieldLabel>
                  <textarea
                    value={editDraft.authJson}
                    onChange={(event) => setEditDraft((current) => ({ ...current, authJson: event.target.value }))}
                    rows={5}
                    spellCheck={false}
                    placeholder={t("antigravity.replacementAuthJsonPlaceholder")}
                    className={TEXTAREA_CLASS}
                  />
                  <FieldHint>{t("antigravity.replacementAuthJsonHint")}</FieldHint>
                </label>
              ) : (
                <label className="block space-y-1.5">
                  <FieldLabel>{t("antigravity.replacementApiKey")}</FieldLabel>
                  <SecretInput
                    value={editDraft.apiKey}
                    onChange={(apiKey) => setEditDraft((current) => ({ ...current, apiKey }))}
                    placeholder={t("antigravity.replacementApiKeyPlaceholder")}
                  />
                  <FieldHint>{t("antigravity.replacementApiKeyHint")}</FieldHint>
                </label>
              )}
              {editDraft.authKind === "oauth" ? (
                <label className="block space-y-1.5">
                  <FieldLabel>{t("antigravity.userProjectId")}</FieldLabel>
                  <Input
                    value={editDraft.userProjectId}
                    onChange={(event) => setEditDraft((current) => ({ ...current, userProjectId: event.target.value }))}
                    spellCheck={false}
                    placeholder={t("antigravity.userProjectIdPlaceholder")}
                    className="font-mono"
                  />
                  <FieldHint>{t("antigravity.userProjectIdHint")}</FieldHint>
                </label>
              ) : null}
            </FormSection>

            {editDraft.authKind === "api_key" ? (
              <FormSection icon={Boxes} iconClassName="text-violet-500" title={t("antigravity.editSectionModels")} wide>
                <APIKeyModelFields
                  models={editDraft.models}
                  modelMapping={editDraft.modelMapping}
                  onModelsChange={(models) => setEditDraft((current) => ({ ...current, models }))}
                  onModelMappingChange={(modelMapping) => setEditDraft((current) => ({ ...current, modelMapping }))}
                />
              </FormSection>
            ) : null}
          </ConfigCategory>

          {schedulerDraft ? (
            <ConfigCategory icon={Gauge} title={t("accounts.categoryDispatch")}>
              <FormSection icon={Gauge} iconClassName="text-blue-500" title={t("accounts.schedulerScoreLabel")} description={t("antigravity.scoreBiasHint")}>
                <Input
                  value={schedulerDraft.scoreBias}
                  onChange={(e) => setSchedulerDraft({ ...schedulerDraft, scoreBias: e.target.value })}
                  placeholder={t("accounts.schedulerScorePlaceholder")}
                  inputMode="numeric"
                />
              </FormSection>
              <FormSection icon={Zap} iconClassName="text-amber-500" title={t("accounts.schedulerConcurrencyLabel")} description={t("accounts.schedulerConcurrencyHint")}>
                <Input
                  value={schedulerDraft.concurrency}
                  onChange={(e) => setSchedulerDraft({ ...schedulerDraft, concurrency: e.target.value })}
                  placeholder={t("accounts.schedulerConcurrencyAutoValue", {
                    value: editingAccount?.base_concurrency_effective ?? "-",
                  })}
                  inputMode="numeric"
                />
              </FormSection>
              <FormSection icon={Sliders} iconClassName="text-indigo-500" title={t("accounts.schedulerPriorityTitle")} description={t("accounts.schedulerPriorityHint")} wide>
                <Input
                  value={schedulerDraft.priority}
                  onChange={(e) => setSchedulerDraft({ ...schedulerDraft, priority: e.target.value })}
                  placeholder={t("accounts.schedulerPriorityPlaceholder")}
                  inputMode="numeric"
                />
              </FormSection>
            </ConfigCategory>
          ) : null}

          {schedulerDraft ? (
            <ConfigCategory icon={ShieldCheck} title={t("accounts.categorySafety")}>
              <FormSection
                icon={Flame}
                iconClassName="text-orange-500"
                title={t("accounts.schedulerSkipWarmLabel")}
                description={t("accounts.schedulerSkipWarmHint")}
                action={
                  <Switch
                    aria-label={t("accounts.schedulerSkipWarmLabel")}
                    checked={schedulerDraft.skipWarmTier}
                    onCheckedChange={(skipWarmTier) => setSchedulerDraft({ ...schedulerDraft, skipWarmTier })}
                  />
                }
              />
              <FormSection
                icon={Gauge}
                iconClassName="text-sky-500"
                title={t("accounts.schedulerKeepConcurrencyLabel")}
                description={t("antigravity.keepConcurrencyHint")}
                action={
                  <Switch
                    aria-label={t("accounts.schedulerKeepConcurrencyLabel")}
                    checked={schedulerDraft.keepConcurrency}
                    onCheckedChange={(keepConcurrency) => setSchedulerDraft({ ...schedulerDraft, keepConcurrency })}
                  />
                }
              />
              <FormSection icon={Timer} iconClassName="text-purple-500" title={t("accounts.dispatchCountLimitTitle")} description={t("antigravity.dispatchCountLimitHint")} wide>
                <Input
                  value={schedulerDraft.dispatchLimit}
                  onChange={(e) => setSchedulerDraft({ ...schedulerDraft, dispatchLimit: e.target.value })}
                  placeholder={t("accounts.dispatchCountLimitPlaceholder")}
                  inputMode="numeric"
                />
                {editingAccount?.dispatch_count_limit ? (
                  <FieldHint>
                    {t("accounts.dispatchCountLimitStatus", {
                      used: editingAccount.dispatch_count_used ?? 0,
                      limit: editingAccount.dispatch_count_limit,
                    })}
                    {editingAccount.dispatch_count_reset_at
                      ? ` · ${t("accounts.dispatchCountLimitResetAt", { time: formatBeijingTime(editingAccount.dispatch_count_reset_at) })}`
                      : ""}
                  </FieldHint>
                ) : null}
              </FormSection>
              <FormSection icon={ShieldAlert} iconClassName="text-rose-500" title={t("accounts.autoPauseTitle")} description={t("antigravity.autoPauseHint")} wide>
                <div className="grid gap-3 md:grid-cols-2">
                  {(
                    [
                      ["autoPause5h", "autoPause5hDisabled", "antigravity.autoPause5hThreshold", "antigravity.autoPause5hDisabled"],
                      ["autoPause7d", "autoPause7dDisabled", "antigravity.autoPause7dThreshold", "antigravity.autoPause7dDisabled"],
                    ] as const
                  ).map(([valueKey, disabledKey, label, disabledLabel]) => (
                    <div key={valueKey} className="space-y-2 rounded-lg border border-border/60 bg-muted/20 p-3">
                      <label className="flex cursor-pointer items-center justify-between gap-3 text-xs font-semibold text-foreground">
                        <span>{t(disabledLabel)}</span>
                        <Switch
                          checked={schedulerDraft[disabledKey]}
                          onCheckedChange={(checked) => setSchedulerDraft({ ...schedulerDraft, [disabledKey]: checked })}
                        />
                      </label>
                      <label className="block space-y-1.5">
                        <FieldLabel>{t(label)}</FieldLabel>
                        <Input
                          value={schedulerDraft[valueKey]}
                          disabled={schedulerDraft[disabledKey]}
                          onChange={(e) => setSchedulerDraft({ ...schedulerDraft, [valueKey]: e.target.value })}
                          placeholder={t("accounts.autoPauseThresholdPlaceholder")}
                          inputMode="decimal"
                        />
                      </label>
                    </div>
                  ))}
                </div>
                <FieldHint>{t("accounts.autoPauseThresholdHint")}</FieldHint>
              </FormSection>
            </ConfigCategory>
          ) : null}

          <ConfigCategory icon={Globe} title={t("accounts.categoryNetwork")}>
            {schedulerDraft ? (
              <FormSection icon={KeyRound} iconClassName="text-emerald-500" title={t("accounts.allowedAPIKeysLabel")} description={t("accounts.allowedAPIKeysHint")} wide>
                <APIKeyMultiSelect
                  options={apiKeys}
                  value={schedulerDraft.allowedAPIKeyIDs}
                  disabled={apiKeys.length === 0}
                  onChange={(next) => setSchedulerDraft({ ...schedulerDraft, allowedAPIKeyIDs: next })}
                  allLabel={t("accounts.allowedAPIKeysAll")}
                  selectedLabel={t("accounts.allowedAPIKeysSelected", { count: schedulerDraft.allowedAPIKeyIDs.length })}
                  placeholder={t("accounts.allowedAPIKeysPlaceholder")}
                  emptyLabel={t("accounts.allowedAPIKeysNoOptions")}
                  emptyHint={t("accounts.allowedAPIKeysNoOptionsHint")}
                />
              </FormSection>
            ) : null}
            <FormSection icon={Network} iconClassName="text-cyan-500" title={t("antigravity.editSectionNetwork")} wide>
              <AccountMetadataFields
                stacked
                proxyUrl={editDraft.proxyUrl}
                proxies={proxies}
                onProxyUrlChange={(proxyUrl) => setEditDraft((current) => ({ ...current, proxyUrl }))}
                groupIds={editDraft.groupIds}
                onGroupIdsChange={(groupIds) => setEditDraft((current) => ({ ...current, groupIds }))}
                groups={groups}
                onCreateGroup={onCreateGroup}
              />
            </FormSection>
            {schedulerDraft ? (
              <FormSection icon={Tag} iconClassName="text-pink-500" title={t("accounts.tagsLabel")} description={t("accounts.tagsHint")} wide>
                <ChipInput
                  value={schedulerDraft.tags}
                  onChange={(tags) => setSchedulerDraft({ ...schedulerDraft, tags })}
                  placeholder={t("accounts.tagsPlaceholder")}
                />
              </FormSection>
            ) : null}
          </ConfigCategory>
        </div>
      )}
    </Modal>
  );
}
