"use client";

import { useId } from "react";
import type { AgentTask, ResultNotification } from "@multica/core/types";
import { useActorName } from "@multica/core/workspace/hooks";
import { ActorAvatar } from "../../common/actor-avatar";
import { redactSecrets } from "../../common/task-transcript/redact";
import { useT } from "../../i18n";
import { ResultTakeover } from "./result-takeover";

function notificationPhase(notification: ResultNotification) {
  if (notification.kind !== "managed_task" && notification.kind !== "external_session") return "unknown";
  switch (notification.status) {
    case "pending":
    case "queued":
    case "deferred":
    case "dispatched":
    case "waiting_local_directory":
      return "waiting";
    case "leased": return "delivering";
    case "running": return "processing";
    case "received":
    case "delivered":
      return "received";
    case "refused": return "refused";
    case "suppressed": return "suppressed";
    case "failed": return "failed";
    case "cancelled": return "cancelled";
    default: return "unknown";
  }
}

export function ResultNotifications({ notifications, issueId, task }: { notifications: ResultNotification[] | undefined; issueId?: string; task?: AgentTask }) {
  const { t } = useT("issues");
  const { getActorName } = useActorName();
  const headingId = useId();
  const labels = {
    waiting: t(($) => $.result_notifications.waiting),
    delivering: t(($) => $.result_notifications.delivering),
    processing: t(($) => $.result_notifications.processing),
    received: t(($) => $.result_notifications.received),
    refused: t(($) => $.result_notifications.refused),
    suppressed: t(($) => $.result_notifications.suppressed),
    failed: t(($) => $.result_notifications.failed),
    cancelled: t(($) => $.result_notifications.cancelled),
    unknown: t(($) => $.result_notifications.unknown),
  };

  return (
    <section aria-labelledby={headingId} className="min-w-0 border-t pt-3 md:col-span-2">
      <h4 id={headingId} className="mb-2 text-caption font-medium">
        {t(($) => $.result_notifications.title)}
      </h4>
      {notifications === undefined ? (
        <p className="text-caption text-muted-foreground">{t(($) => $.result_notifications.unavailable)}</p>
      ) : notifications.length === 0 ? (
        <p className="text-caption text-muted-foreground">{t(($) => $.result_notifications.empty)}</p>
      ) : (
        <ul className="space-y-3">
          {notifications.map((notification) => {
            const phase = notificationPhase(notification);
            const tone = phase === "received" ? "text-success"
              : phase === "failed" || phase === "refused" ? "text-destructive"
                : phase === "unknown" ? "text-warning" : "text-muted-foreground";
            const error = notification.error?.trim();
            return (
              <li key={notification.return_id} className="min-w-0 space-y-1">
                <div className="flex items-center gap-2 text-caption">
                  <ActorAvatar actorType="agent" actorId={notification.recipient_agent_id} size="xs" profileLink={false} />
                  <span className="min-w-0 flex-1 break-words">{getActorName("agent", notification.recipient_agent_id)}</span>
                  <span className={`shrink-0 ${tone}`}>{labels[phase]}</span>
                </div>
                {issueId && task && (task.status === "completed" || task.status === "failed")
                  && notification.current_return_id && notification.kind !== "unknown" && (
                    <ResultTakeover issueId={issueId} task={task} notification={notification} notifications={notifications} />
                  )}
                {error && (
                  <div className="text-caption text-muted-foreground">
                    <span className="font-medium">{t(($) => $.result_notifications.error_details)}</span>
                    <p className="break-words whitespace-pre-wrap">{redactSecrets(error)}</p>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {notifications?.some((notification) => notificationPhase(notification) === "received") && (
        <p className="mt-2 text-caption text-muted-foreground">{t(($) => $.result_notifications.receipt_hint)}</p>
      )}
    </section>
  );
}
