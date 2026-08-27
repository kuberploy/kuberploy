import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { ApiError, api } from "../api/client";
import type {
  Application,
  BuildAttempt,
  Capability,
  Project,
} from "../api/types";
import { hasBuildApplicationCapability } from "../lib/buildAccess";
import { Icon } from "./Icon";
import { Button, ErrorPanel, Field } from "./ui";

type Command = {
  kind: "cancel" | "retry";
  attemptId: string;
  applicationId: string;
  idempotencyKey: string;
};

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

export function BuildAttemptActions({
  attempt,
  application,
  project,
  capabilities,
  humanSession,
  onUpdated,
}: {
  attempt: BuildAttempt;
  application: Application;
  project: Project;
  capabilities: Capability[];
  humanSession: boolean;
  onUpdated?: (attempt: BuildAttempt) => void;
}) {
  const queryClient = useQueryClient();
  const [command, setCommand] = useState<Command | null>(null);
  const [confirmation, setConfirmation] = useState("");
  const scopeRef = useRef(`${application.id}:${attempt.id}`);
  scopeRef.current = `${application.id}:${attempt.id}`;
  const canCancel =
    humanSession &&
    ["queued", "preparing", "running"].includes(attempt.state) &&
    hasBuildApplicationCapability(
      capabilities,
      "builds:cancel",
      application,
      project,
    );
  const canRetry =
    humanSession &&
    ["failed", "cancelled"].includes(attempt.state) &&
    hasBuildApplicationCapability(
      capabilities,
      "builds:retry",
      application,
      project,
    );
  const mutation = useMutation({
    mutationFn: (input: Command) =>
      input.kind === "cancel"
        ? api.cancelBuildAttempt(input.attemptId, input.idempotencyKey)
        : api.retryBuildAttempt(input.attemptId, input.idempotencyKey),
    retry: retryNetworkOnce,
    onSuccess: (updated, input) => {
      queryClient.setQueryData(["build-attempt", updated.id], updated);
      void queryClient.invalidateQueries({
        queryKey: ["build-attempts", input.applicationId],
      });
      if (scopeRef.current !== `${input.applicationId}:${input.attemptId}`) {
        return;
      }
      setCommand(null);
      setConfirmation("");
      onUpdated?.(updated);
    },
  });

  useEffect(() => {
    setCommand(null);
    setConfirmation("");
    mutation.reset();
  }, [application.id, attempt.id]);

  const prepare = (kind: Command["kind"]) => {
    mutation.reset();
    setConfirmation("");
    setCommand({
      kind,
      attemptId: attempt.id,
      applicationId: application.id,
      idempotencyKey: crypto.randomUUID(),
    });
  };

  if (!canCancel && !canRetry && !command) return null;

  return (
    <div className="grid gap-4 grid-flow-col justify-start">
      {!command ? (
        <>
          {canCancel ? (
            <Button variant="danger" onClick={() => prepare("cancel")}>
              <Icon name="close" /> Cancel build
            </Button>
          ) : null}
          {canRetry ? (
            <Button variant="secondary" onClick={() => prepare("retry")}>
              <Icon name="refresh" /> Retry build
            </Button>
          ) : null}
        </>
      ) : (
        <div className="grid gap-4 grid-flow-row w-[min(560px,_100%)] p-4 border border-line-strong rounded-[10px] bg-surface-soft [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5]">
          <div>
            <strong>
              {command.kind === "cancel"
                ? "Cancel this build?"
                : "Create an immutable retry?"}
            </strong>
            <p>
              Type the exact build ID. A retry preserves the authoritative
              commit, ref, definition digest, registry, and cache policy.
            </p>
          </div>
          <Field label={`Type ${attempt.id}`} required>
            <input
              value={confirmation}
              disabled={mutation.isPending}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => setConfirmation(event.target.value)}
            />
          </Field>
          {mutation.error ? <ErrorPanel error={mutation.error} /> : null}
          <div className="flex flex-wrap gap-2">
            <Button
              variant={command.kind === "cancel" ? "danger" : "primary"}
              disabled={confirmation !== attempt.id}
              busy={mutation.isPending}
              onClick={() => mutation.mutate(command)}
            >
              {command.kind === "cancel" ? "Confirm cancel" : "Confirm retry"}
            </Button>
            <Button
              variant="ghost"
              disabled={mutation.isPending}
              onClick={() => {
                setCommand(null);
                setConfirmation("");
                mutation.reset();
              }}
            >
              Keep build
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
