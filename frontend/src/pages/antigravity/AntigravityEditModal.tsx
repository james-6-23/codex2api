import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Loader2, Pencil, RotateCw } from "lucide-react";
import { api } from "../../api";
import type { ProxyRow } from "../../api";
import type {
  AccountGroup,
  AccountRow,
  AntigravityAuthKind,
  UpdateAntigravityAccountRequest,
} from "../../types";
import Modal from "../../components/Modal";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useToast } from "../../hooks/useToast";
import { ANTIGRAVITY_DEFAULT_MODELS } from "../../lib/antigravityModels";
import { accountLabel, parseModelList } from "../../lib/antigravityQuota";
import { getErrorMessage } from "../../utils/error";
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

  const closeEditor = useCallback(() => {
    if (editing) return;
    editLoadGenerationRef.current += 1;
    onClose();
  }, [editing, onClose]);

  const handleEdit = async () => {
    if (!editingAccount || editing || editingDetailsLoading || editingDetailsError) return;
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
      contentClassName="sm:max-w-[620px]"
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
        <div className="space-y-3">
          <FormSection title={t("antigravity.editSectionBasic")}>
            <label className="block space-y-1.5">
              <FieldLabel>{t("antigravity.accountName")}</FieldLabel>
              <Input
                value={editDraft.name}
                onChange={(event) => setEditDraft((current) => ({ ...current, name: event.target.value }))}
              />
            </label>
          </FormSection>

          <FormSection
            title={t("antigravity.editSectionCredential")}
            description={t("antigravity.editSectionCredentialHint")}
          >
            {editDraft.authKind === "oauth" ? (
              <label className="block space-y-1.5">
                <FieldLabel>{t("antigravity.replacementAuthJson")}</FieldLabel>
                <textarea
                  value={editDraft.authJson}
                  onChange={(event) => setEditDraft((current) => ({ ...current, authJson: event.target.value }))}
                  rows={6}
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
            <FormSection title={t("antigravity.editSectionModels")}>
              <APIKeyModelFields
                models={editDraft.models}
                modelMapping={editDraft.modelMapping}
                onModelsChange={(models) => setEditDraft((current) => ({ ...current, models }))}
                onModelMappingChange={(modelMapping) => setEditDraft((current) => ({ ...current, modelMapping }))}
              />
            </FormSection>
          ) : null}

          <FormSection title={t("antigravity.editSectionNetwork")}>
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
        </div>
      )}
    </Modal>
  );
}
