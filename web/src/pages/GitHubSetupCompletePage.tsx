import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  ErrorPanel,
  Eyebrow,
  Page,
  PageHeader,
  StatusPill,
  buttonVariants,
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
    <Page className="[&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-base max-w-[760px] mx-[auto]">
      <PageHeader
        eyebrow="Source access"
        title="Complete GitHub setup"
        description="Kuberploy is consuming the one-time browser handoff and linking only provider-verified metadata."
      />
      <Card>
        <div className="grid grid-cols-[46px_minmax(0,_1fr)_auto] items-center gap-4 [&_p]:mt-2 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.55] to-520:grid-cols-[1fr] to-520:[&>[data-slot='status-pill']]:justify-self-start">
          <span className="grid w-[42px] h-[42px] place-items-center rounded-[11px] text-mint-dark bg-surface-soft [&_svg]:w-[19px]">
            {state === "linking" ? (
              <span
                className="inline-block size-3.5 animate-spin rounded-full border-2 border-current border-r-transparent"
                aria-hidden="true"
              />
            ) : (
              <Icon name={state === "linked" ? "check" : "close"} />
            )}
          </span>
          <div>
            <Eyebrow>Verified handoff</Eyebrow>
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
        <div className="grid gap-4 grid-flow-col justify-start mt-5 to-520:grid-flow-row">
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
            <Link className={buttonVariants({ variant: "primary" })} to="/git">
              Continue to Git providers <Icon name="arrow" />
            </Link>
          ) : null}
          <Link className={buttonVariants({ variant: "ghost" })} to="/git">
            Back to Git providers
          </Link>
        </div>
      </Card>
    </Page>
  );
}
