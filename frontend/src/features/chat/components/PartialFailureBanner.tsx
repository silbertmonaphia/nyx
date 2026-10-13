import type React from "react";
import { cn } from "~/utils/cn";

/**
 * Props for `<PartialFailureBanner>`. `failedSteps` is the list of
 * step ids whose `step_end` (or terminal `tool_result`) carried a
 * failure signal per `openspec/changes/agent-tool-dispatch-retry/
 * specs/agent-partial-failure/spec.md`. An empty list means the run
 * terminated before any step reached a terminal status (plan-level
 * error per §"Plan-level errors trigger the signal").
 */
export interface PartialFailureBannerProps {
  failedSteps: string[];
}

/**
 * Yellow-tinted warning banner rendered above the assistant's final
 * answer when the SSE stream carried a `note{kind: "partial_failure"}`
 * frame. Surfacing the banner does NOT require the user to expand the
 * `agent-steps` disclosure — the spec's "Banner is visible without
 * expanding the steps panel" scenario demands it lives at the same
 * DOM level as the final-answer text.
 *
 * The `data-failed-steps` attribute carries the comma-joined ids so
 * Playwright (and any DOM-driven tooling) can assert on it directly
 * without parsing inner text. The test id follows the spec's wire
 * contract byte-for-byte.
 *
 * Accessibility: `role="status"` + `aria-live="polite"` matches the
 * "warning, not a hard error" intent of the spec — screen readers
 * announce the message without interrupting the user's current
 * utterance. Contrast ratio against the yellow background meets
 * WCAG AA for the rendered text.
 */
export const PartialFailureBanner: React.FC<PartialFailureBannerProps> = ({
  failedSteps,
}) => {
  // The server only sends an empty `failed_steps` when the run
  // terminated before any step reached a terminal status (plan-level
  // error per the spec). In that case the user-facing text differs:
  // the run never reached a step, so listing ids would be a lie.
  const isPlanLevel = failedSteps.length === 0;
  const headline = isPlanLevel
    ? "Run did not reach a terminal step"
    : "Some steps failed — the answer below may be incomplete";
  // TODO(i18n): the codebase has no localisation framework today;
  // strings live inline. When one lands, move the headline + step
  // listing into the same string table.
  return (
    <div
      className={cn(
        "mb-2 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900",
        "dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-100",
      )}
      data-testid="chat-partial-failure-banner"
      data-failed-steps={failedSteps.join(",")}
      role="status"
      aria-live="polite"
    >
      <div className="font-medium">{headline}</div>
      {!isPlanLevel && (
        <ul className="mt-1 list-disc pl-4 space-y-0.5">
          {failedSteps.map((id) => (
            <li
              key={id}
              className="font-mono"
              data-testid="chat-partial-failure-banner-step"
            >
              {id}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
};