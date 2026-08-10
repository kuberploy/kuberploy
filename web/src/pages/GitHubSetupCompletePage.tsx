import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  ErrorPanel,
  PageHeader,
  StatusPill,
} from "../components/ui";

export function GitHubSetupCompletePage() {
  const queryClient = useQueryClient();
  const idempotencyKey = useRef(crypto.randomUUID());
  const request = useRef<ReturnType<typeof api.completeGitHubSetup> | null>(
    null,
  );
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<"linking" | "linked" | "failed">(
    "linking",
  );
  const [error, setError] = useState<unknown>();

  useEffect(() => {
    let active = true;
    setState("linking");
    setError(undefined);
    // The browser sends the exact-path HttpOnly one-time cookie. JavaScript
    // never reads a handoff and the POST deliberately has no request body.
    request.current ??= api.completeGitHubSetup(idempotencyKey.current);
    const current = request.current;
    void current.then(
      async () => {
        request.current = null;
        if (!active) return;
        await queryClient.invalidateQueries({
          queryKey: ["github-installations"],
        });
        if (active) setState("linked");
      },
      (reason: unknown) => {
        request.current = null;
        if (!active) return;
        setError(reason);
        setState("failed");
      },
    );
    return () => {
      active = false;
    };
  }, [attempt, queryClient]);

  return (
    <div className="page github-setup-complete">
      <PageHeader
        eyebrow="Source access"
        title="Complete GitHub setup"
        description="Kuberploy is consuming the one-time browser handoff and linking only provider-verified metadata."
      />
      <Card>
        <div className="github-setup-complete__state">
          <span className="github-setup-complete__icon">
            {state === "linking" ? (
              <span className="spinner" aria-hidden="true" />
            ) : (
              <Icon name={state === "linked" ? "check" : "close"} />
            )}
          </span>
          <div>
            <span className="eyebrow">Verified handoff</span>
            <h2>
              {state === "linking"
                ? "Linking installation"
                : state === "linked"
                  ? "GitHub installation linked"
                  : "Installation was not linked"}
            </h2>
            <p>
              {state === "linked"
                ? "The installation and active repository catalog are ready. No App credential or checkout token entered browser storage."
                : state === "failed"
                  ? "The one-time handoff may have expired or already been consumed. Start a fresh GitHub setup flow if retry does not succeed."
                  : "Keep this page open while the same-origin completion request finishes."}
            </p>
          </div>
          <StatusPill
            value={state === "linked" ? "ready" : state}
            label={state === "linked" ? "Linked" : undefined}
          />
        </div>
        {error ? (
          <ErrorPanel title="Could not complete GitHub setup" error={error} />
        ) : null}
        <div className="github-setup-complete__actions">
          {state === "failed" ? (
            <Button
              variant="secondary"
              onClick={() => {
                request.current = null;
                setAttempt((value) => value + 1);
              }}
            >
              <Icon name="refresh" /> Retry same handoff
            </Button>
          ) : null}
          {state === "linked" ? (
            <Link className="button button--primary" to="/builds">
              Continue to source builds <Icon name="arrow" />
            </Link>
          ) : null}
          <Link className="button button--ghost" to="/builds">
            Back to source builds
          </Link>
        </div>
      </Card>
    </div>
  );
}
