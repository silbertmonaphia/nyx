import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { AlertCircle, CheckCircle, History, X } from "lucide-react";
import { Button } from "~/components/ui/Button";
import { Card, CardContent } from "~/components/ui/Card";
import type { ApprovalDecision, ChatStepConfirm } from "../types/chat";

/**
 * Props for `<ConfirmationCard>`. `confirm` is the store record built
 * from the step's `confirm_request` SSE frame; `onDecide` records the
 * decision (the store POSTs it and flips the card's decided state).
 */
export interface ConfirmationCardProps {
  confirm: ChatStepConfirm;
  onDecide: (decision: ApprovalDecision) => Promise<void>;
  onExpire: () => void;
}

function formatRemaining(ms: number): string {
  const totalSeconds = Math.max(Math.ceil(ms / 1000), 0);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, "0")}`;
}

/**
 * Render an unknown JSON arg payload for the card body. The frame's
 * `args` are masked producer-side (same policy as `tool_call` frames),
 * so the card renders them verbatim — it never reaches into the
 * payload to un-mask or redact.
 */
function formatArgs(args: unknown): string {
  if (args === undefined) return "{}";
  try {
    return JSON.stringify(args, null, 2) ?? String(args);
  } catch {
    return String(args);
  }
}

/**
 * Inline approve/deny card for a paused mutating tool call
 * (openspec/changes/agent-feed-tools §6.2). Resolved as a card inside
 * the chat turn — not a modal — so the paused run's context, stepper
 * and streamed text stay visible while the user decides.
 *
 * Pending: tool + args + Approve/Deny. Decided: a status line replaces
 * the buttons (the card stays in the turn as a record). A failed
 * decision POST keeps the card pending with a short local hint so the
 * user can retry — except a 404 (dead approval id, design.md D2),
 * which resolves the card as expired since a retry could never
 * succeed. The denial itself arrives as the paused call's
 * `tool_result` frame and renders through the regular agent-step
 * result path — this card only carries the decision state.
 */
export const ConfirmationCard: React.FC<ConfirmationCardProps> = ({
  confirm,
  onDecide,
  onExpire,
}) => {
  const { t } = useTranslation();
  // Local in-flight guard: the decision POST is idempotent server-side
  // (first decision wins), but the buttons shouldn't invite a
  // double-click while the request is on the wire.
  const [submitting, setSubmitting] = useState(false);
  const [nowMs, setNowMs] = useState(() => Date.now());
  const notifiedExpiry = useRef(false);
  const isPending = confirm.decision === "pending";
  const expiresAtMs = confirm.expiresAt * 1000;
  const remainingMs = Math.max(expiresAtMs - nowMs, 0);

  useEffect(() => {
    if (!isPending) return;
    const interval = window.setInterval(() => setNowMs(Date.now()), 1000);
    return () => window.clearInterval(interval);
  }, [isPending]);

  useEffect(() => {
    if (!isPending || remainingMs > 0 || notifiedExpiry.current) return;
    notifiedExpiry.current = true;
    onExpire();
  }, [isPending, remainingMs, onExpire]);

  const handleDecide = async (decision: ApprovalDecision) => {
    if (submitting) return;
    setSubmitting(true);
    try {
      await onDecide(decision);
    } finally {
      setSubmitting(false);
    }
  };

  const StatusIcon =
    confirm.decision === "approved"
      ? CheckCircle
      : confirm.decision === "denied"
        ? X
        : confirm.decision === "expired"
          ? History
          : AlertCircle;
  const headline =
    confirm.decision === "approved"
      ? t("chat.confirmation.approved")
      : confirm.decision === "denied"
        ? t("chat.confirmation.denied")
        : confirm.decision === "expired"
          ? t("chat.confirmation.expired")
          : t("chat.confirmation.needed");

  return (
    <Card
      className="mb-2 border-amber-300 bg-amber-50 text-amber-950 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-100"
      data-testid="chat-confirm-card"
      data-approval-id={confirm.approvalId}
      data-decision={confirm.decision}
    >
      <CardContent className="p-3 space-y-2 text-xs">
        <div className="flex items-center gap-1.5 font-medium">
          <StatusIcon className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
          <span>{headline}</span>
        </div>
        <div className="flex flex-wrap items-center gap-1">
          <span className="opacity-70">{t("chat.confirmation.tool")}</span>
          <span
            className="font-mono font-medium"
            data-testid="chat-confirm-tool"
          >
            {confirm.tool}
          </span>
        </div>
        <pre
          className="whitespace-pre-wrap break-words rounded border border-amber-200 bg-background/60 p-2 font-mono text-[10px] text-foreground"
          data-testid="chat-confirm-args"
        >
          {formatArgs(confirm.args)}
        </pre>
        {isPending ? (
          <div className="flex items-center justify-between gap-2">
            <span
              className="opacity-70 tabular-nums"
              data-testid="chat-confirm-countdown"
              role="timer"
            >
              {t("chat.confirmation.expiresIn", {
                time: formatRemaining(remainingMs),
              })}
            </span>
            <div className="flex gap-2">
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={submitting}
                data-testid="chat-confirm-deny"
                onClick={() => void handleDecide("deny")}
              >
                {t("chat.confirmation.deny")}
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={submitting}
                data-testid="chat-confirm-approve"
                onClick={() => void handleDecide("approve")}
              >
                {t("chat.confirmation.approve")}
              </Button>
            </div>
          </div>
        ) : (
          <div
            className="opacity-80"
            data-testid="chat-confirm-status"
            role="status"
            aria-live="polite"
          >
            {confirm.decision === "approved"
              ? t("chat.confirmation.approvedBody")
              : confirm.decision === "denied"
                ? t("chat.confirmation.deniedBody")
                : t("chat.confirmation.expiredBody")}
          </div>
        )}
        {confirm.error && (
          <div className="text-destructive" data-testid="chat-confirm-error">
            {confirm.error}
          </div>
        )}
      </CardContent>
    </Card>
  );
};
