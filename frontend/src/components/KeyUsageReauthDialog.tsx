import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { KeyRound } from "lucide-react";
import { api, type PublicKeyUsageAccount } from "../api";
import AccountReauthorizationForm from "./AccountReauthorizationForm";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { getErrorMessage } from "../utils/error";
import { copyTextToClipboard } from "../utils/copyTextToClipboard";
import { parseOAuthCallbackParams } from "../utils/oauthCallback";

export default function KeyUsageReauthDialog({
  apiKey,
  account,
  onComplete,
}: {
  apiKey: string;
  account: PublicKeyUsageAccount;
  onComplete: () => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [session, setSession] = useState<{
    session_id: string;
    auth_url: string;
  } | null>(null);
  const [callbackUrl, setCallbackUrl] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [completed, setCompleted] = useState(false);
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => pending.current?.abort(), []);
  function changeOpen(next: boolean) {
    pending.current?.abort();
    pending.current = null;
    setBusy(false);
    setOpen(next);
    setCompleted(false);
    setSession(null);
    setCallbackUrl("");
    setError("");
    setNotice("");
  }
  async function submit(restart = false) {
    if (pending.current || !account.reauth_allowed) return;
    const controller = new AbortController();
    pending.current = controller;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      if (!session || restart) {
        setCallbackUrl("");
        setSession(null);
        const next = await api.startPublicAccountReauth(
          apiKey,
          account.id,
          controller.signal,
        );
        if (!controller.signal.aborted) setSession(next);
      } else {
        const { code, state } = parseOAuthCallbackParams(callbackUrl);
        if (!code || !state) {
          setError(t("accounts.oauthParseError"));
          return;
        }
        const result = await api.completePublicAccountReauth(
          apiKey,
          account.id,
          {
            session_id: session.session_id,
            code,
            state,
          },
          controller.signal,
        );
        if (controller.signal.aborted) return;
        setCallbackUrl("");
        setSession(null);
        setCompleted(true);
        setNotice(
          t(
            result.reload_pending
              ? "keyUsageAccounts.reauthReloadPending"
              : "accounts.oauthUpdateSuccessNoEmail",
          ),
        );
        onComplete();
      }
    } catch (err) {
      if (!controller.signal.aborted) setError(getErrorMessage(err));
    } finally {
      if (pending.current === controller && !controller.signal.aborted) {
        pending.current = null;
        setBusy(false);
      }
    }
  }
  async function copy() {
    if (!session) return;
    try {
      await copyTextToClipboard(session.auth_url);
      setNotice(t("common.copied"));
    } catch {
      setError(t("common.copyFailed"));
    }
  }
  if (!account.reauth_allowed) return null;
  return (
    <Dialog open={open} onOpenChange={changeOpen}>
      <DialogTrigger asChild>
        <Button
          variant="ghost"
          size="sm"
          className="codex-account-card__action"
        >
          <KeyRound className="size-3.5" />
          {t("keyUsageAccounts.reauthorize")}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("accounts.oauthEditIntroTitle")}</DialogTitle>
          <DialogDescription>
            {account.email || account.name || `#${account.id}`}
          </DialogDescription>
        </DialogHeader>
        {!completed && (
          <AccountReauthorizationForm
            accountName={account.email || account.name || `#${account.id}`}
            step={session ? "exchange" : "generate"}
            session={session}
            callbackUrl={callbackUrl}
            onCallbackChange={setCallbackUrl}
            busy={busy}
            onCopy={copy}
            onRestart={() => void submit(true)}
          />
        )}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {notice && (
          <p role="status" className="text-sm">
            {notice}
          </p>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={() => changeOpen(false)}>
            {t("common.close")}
          </Button>
          {!completed && (
            <Button
              onClick={() => void submit()}
              disabled={busy || (!!session && !callbackUrl.trim())}
            >
              {t(
                session
                  ? busy
                    ? "accounts.oauthCompleting"
                    : "accounts.oauthUpdateAuth"
                  : busy
                    ? "accounts.oauthGenerating"
                    : "accounts.oauthGenerateBtn",
              )}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
