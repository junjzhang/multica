"use client";

import { useState } from "react";
import { Check, Copy, ExternalLink, Link2 } from "lucide-react";
import { toast } from "sonner";
import type { ExternalSession } from "@multica/core/types";
import { parsePaseoSessionUrl } from "@multica/core/agents/paseo-session-url";
import { Button } from "@multica/ui/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@multica/ui/components/ui/tooltip";
import { copyText } from "@multica/ui/lib/clipboard";
import { cn } from "@multica/ui/lib/utils";
import { isDesktopShell, openExternal } from "../../platform";
import { useT } from "../../i18n";

/**
 * Typed external-session locator. This is the only product surface allowed to
 * hand a `paseo://` URL to the opener: the URL must match the exact session
 * shape, and opening is offered only where the desktop main process's narrow
 * gate can honor it. Everywhere else the session id remains copyable.
 */
export function ExternalSessionLink({
  session,
  detailed = false,
  templateName,
  className,
}: {
  session: ExternalSession;
  detailed?: boolean;
  /** Host template the session belongs to, when the task/agent names it. */
  templateName?: string | null;
  className?: string;
}) {
  const { t } = useT("issues");
  const [copied, setCopied] = useState(false);
  const parsed = parsePaseoSessionUrl(session.url);
  const canOpen = parsed !== null && isDesktopShell();
  const name = session.display_name || session.session_id;
  const provider = session.provider || "external";
  // "Template › session title" is the identity the task view names; a legacy
  // per-session agent has no template and keeps the bare session title.
  const label = templateName ? `${templateName} › ${name}` : name;

  const copySessionId = async () => {
    if (await copyText(session.session_id)) {
      setCopied(true);
      toast.success(t(($) => $.detail.external_session_copied));
      window.setTimeout(() => setCopied(false), 1500);
      return;
    }
    toast.error(t(($) => $.detail.external_copy_failed));
  };

  const actions = (
    <span className="inline-flex shrink-0 items-center gap-0.5">
      {canOpen && session.url && (
        <Tooltip>
          <TooltipTrigger
            render={
              <Button
                type="button"
                variant="ghost"
                size="icon-xs"
                aria-label={t(($) => $.detail.external_open_session)}
                onClick={() => openExternal(session.url!)}
              >
                <ExternalLink />
              </Button>
            }
          />
          <TooltipContent side="top">{t(($) => $.detail.external_open_session)}</TooltipContent>
        </Tooltip>
      )}
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.detail.external_copy_session_id)}
              onClick={copySessionId}
            >
              {copied ? <Check /> : <Copy />}
            </Button>
          }
        />
        <TooltipContent side="top">{t(($) => $.detail.external_copy_session_id)}</TooltipContent>
      </Tooltip>
    </span>
  );

  if (!detailed) {
    return (
      <span
        className={cn(
          "inline-flex min-w-0 max-w-full items-center gap-1 rounded-md border border-border/70 bg-muted/40 px-1.5 py-0.5 text-micro text-muted-foreground",
          className,
        )}
        title={t(($) => $.detail.external_session_title, {
          provider,
          host: session.host_id,
          agent: session.session_id,
        })}
      >
        <Link2 className="size-3 shrink-0" />
        <span className="truncate">{label}</span>
        {actions}
      </span>
    );
  }

  return (
    <div className={cn("min-w-0 rounded-md border border-border/70 bg-muted/30 p-2 text-caption", className)}>
      <div className="flex min-w-0 items-start gap-1.5">
        <Link2 className="mt-0.5 size-3 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="truncate font-medium text-foreground">{label}</div>
          <div className="truncate text-micro text-muted-foreground">
            {t(($) => $.detail.external_session_meta, { provider, host: session.host_id })}
          </div>
          <div className="break-all text-micro text-muted-foreground">
            {t(($) => $.detail.external_session_id, { id: session.session_id })}
          </div>
        </div>
        {actions}
      </div>
    </div>
  );
}
