import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
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
        ? api.cancelBuildAttempt(attempt.id, input.idempotencyKey)
        : api.retryBuildAttempt(attempt.id, input.idempotencyKey),
    retry: retryNetworkOnce,
    onSuccess: (updated) => {
      queryClient.setQueryData(["build-attempt", updated.id], updated);
      void queryClient.invalidateQueries({
        queryKey: ["build-attempts", application.id],
      });
      setCommand(null);
      setConfirmation("");
      onUpdated?.(updated);
    },
  });

  const prepare = (kind: Command["kind"]) => {
    mutation.reset();
    setConfirmation("");
    setCommand({ kind, idempotencyKey: crypto.randomUUID() });
  };

  if (!canCancel && !canRetry && !command) return null;

  return (
    <div className="build-attempt-actions">
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
        <div className="build-command-confirmation">
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
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => setConfirmation(event.target.value)}
            />
          </Field>
          {mutation.error ? <ErrorPanel error={mutation.error} /> : null}
          <div className="build-command-confirmation__actions">
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
