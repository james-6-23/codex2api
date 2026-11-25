import type { ChangeEvent, ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Copy, ExternalLink } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

// Presentation only: the caller owns the session, transport and available settings.
export default function AccountReauthorizationForm({
  accountName,
  step,
  session,
  callbackUrl,
  onCallbackChange,
  onCopy,
  onRestart,
  busy,
  extraSettings,
}: {
  accountName: string;
  step: "generate" | "exchange";
  session: { auth_url: string } | null;
  callbackUrl: string;
  onCallbackChange: (value: string) => void;
  onCopy: () => void;
  onRestart: () => void;
  busy: boolean;
  extraSettings?: ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <div className="space-y-4">
      <div className="rounded-xl border border-border/80 bg-muted/30 p-4.5 text-sm text-muted-foreground">
        <p className="font-semibold text-foreground mb-1">
          {t("accounts.oauthEditIntroTitle")}
        </p>
        <p>{t("accounts.oauthEditIntroDesc")}</p>
      </div>
      <div>
        <label className="block mb-2 text-xs font-semibold text-muted-foreground">
          {t("accounts.oauthCurrentAccount")}
        </label>
        <div className="rounded-lg border border-dashed border-border bg-muted/20 px-3 py-2 text-sm text-muted-foreground">
          {accountName}
        </div>
      </div>
      {step === "generate" ? (
        <>
          <div className="rounded-xl border border-border/80 bg-muted/30 p-4.5 text-sm text-muted-foreground">
            <p className="font-semibold text-foreground mb-1">
              {t("accounts.oauthStep1Title")}
            </p>
            <p>{t("accounts.oauthStep1Desc")}</p>
          </div>
          {extraSettings}
        </>
      ) : (
        <>
          <div className="rounded-xl border border-border/80 bg-muted/30 p-4.5 text-sm text-muted-foreground">
            <p className="font-semibold text-foreground mb-1">
              {t("accounts.oauthStep2Title")}
            </p>
            <p>{t("accounts.oauthStep2Desc")}</p>
          </div>
          {session && (
            <div className="rounded-xl border border-primary/30 bg-primary/5 p-4.5">
              <p className="text-xs font-semibold text-muted-foreground mb-2">
                {t("accounts.oauthAuthLinkLabel")}
              </p>
              <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-start">
                <a
                  href={session.auth_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  title={session.auth_url}
                  className="inline-flex min-h-10 min-w-0 max-w-full flex-1 items-start gap-1.5 overflow-hidden rounded-lg border bg-background px-3 py-2 text-sm font-semibold text-primary hover:bg-muted/50"
                >
                  <ExternalLink className="mt-0.5 size-3.5 shrink-0" />
                  <span className="block min-w-0 flex-1 break-all leading-relaxed [overflow-wrap:anywhere]">
                    {session.auth_url}
                  </span>
                </a>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => void onCopy()}
                  className="w-full shrink-0 sm:w-auto"
                >
                  <Copy className="size-4" />
                  {t("common.copy")}
                </Button>
              </div>
            </div>
          )}
          <div>
            <label className="block mb-2 text-xs font-semibold text-muted-foreground">
              {t("accounts.oauthCallbackUrlLabel")}
            </label>
            <Input
              placeholder={t("accounts.oauthCallbackUrlPlaceholder")}
              aria-label={t("accounts.oauthCallbackUrlLabel")}
              autoComplete="off"
              spellCheck={false}
              value={callbackUrl}
              onChange={(event: ChangeEvent<HTMLInputElement>) =>
                onCallbackChange(event.target.value)
              }
            />
            <p className="mt-1.5 text-xs text-muted-foreground">
              {t("accounts.oauthCallbackUrlHint")}
            </p>
          </div>
          <button
            type="button"
            onClick={() => void onRestart()}
            disabled={busy}
            className="text-xs text-muted-foreground hover:text-foreground underline underline-offset-2"
          >
            {busy ? t("accounts.oauthGenerating") : t("accounts.oauthRestart")}
          </button>
        </>
      )}
    </div>
  );
}
