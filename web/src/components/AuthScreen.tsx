import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useLayoutEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Principal, User } from "../api/types";
import {
  clearInvitationFragment,
  invitationTokenFromHash,
} from "../lib/invitationLink";
import { Button, Field } from "./ui";
import { Icon } from "./Icon";

type BootstrapForm = {
  displayName: string;
  token: string;
  password: string;
};

type InvitationAcceptanceForm = {
  displayName: string;
  token: string;
  password: string;
};

type LoginForm = { login: string; password: string };

function sessionPrincipal(user: User): Principal {
  return { ...user, authentication: { kind: "session" } };
}

export function AuthScreen({
  connectionError,
  invitationToken,
  onInvitationAccepted,
  onInvitationDismissed,
}: {
  connectionError?: unknown;
  invitationToken?: string;
  onInvitationAccepted?: () => void;
  onInvitationDismissed?: () => void;
}) {
  const queryClient = useQueryClient();
  const [linkedInvitationToken] = useState(
    () =>
      invitationToken ?? invitationTokenFromHash(window.location.hash) ?? "",
  );
  const [inviteMode, setInviteMode] = useState(
    () => linkedInvitationToken.length > 0,
  );
  const meta = useQuery({
    queryKey: ["meta"],
    queryFn: api.meta,
    retry: false,
  });
  const bootstrapForm = useForm<BootstrapForm>({
    defaultValues: { displayName: "", token: "", password: "" },
  });
  const invitationForm = useForm<InvitationAcceptanceForm>({
    defaultValues: {
      displayName: "",
      token: linkedInvitationToken,
      password: "",
    },
  });
  const loginForm = useForm<LoginForm>({
    defaultValues: { login: "", password: "" },
  });
  const bootstrap = useMutation({
    mutationFn: api.bootstrap,
    onSuccess: (user) => {
      bootstrapForm.reset();
      queryClient.setQueryData(["me"], sessionPrincipal(user));
      queryClient.invalidateQueries({ queryKey: ["meta"] });
    },
  });
  const acceptInvitation = useMutation({
    mutationFn: api.acceptInvitation,
    onSuccess: (user) => {
      invitationForm.reset();
      queryClient.setQueryData(["me"], sessionPrincipal(user));
      onInvitationAccepted?.();
    },
  });
  const login = useMutation({
    mutationFn: api.login,
    onSuccess: (user) => {
      loginForm.reset();
      queryClient.setQueryData(["me"], sessionPrincipal(user));
    },
  });

  useLayoutEffect(() => {
    clearInvitationFragment();
  }, []);

  const offline =
    connectionError &&
    !(
      connectionError instanceof Error &&
      "status" in connectionError &&
      (connectionError as { status?: number }).status === 401
    );
  const bootstrapRequired = meta.data?.bootstrapRequired !== false;
  const mode = offline
    ? "offline"
    : inviteMode
      ? "invitation"
      : bootstrapRequired
        ? "bootstrap"
        : "session";

  return (
    <main className="auth-layout">
      <section className="auth-story">
        <div className="brand brand--auth">
          <span className="brand__mark" aria-hidden="true">
            <span />
            <span />
            <span />
          </span>
          <span>Kuberploy</span>
        </div>
        <div className="auth-story__content">
          <span className="eyebrow eyebrow--light">
            Kubernetes, without the ceremony
          </span>
          <h1>
            Ship from a digest.
            <br />
            Reconcile from Git.
          </h1>
          <p>
            A self-hosted control plane for immutable releases, explicit routes,
            and an honest view of what Argo CD is doing.
          </p>
          <div className="auth-flow" aria-label="Kuberploy deployment flow">
            <span>OCI image</span>
            <Icon name="arrow" />
            <span>Git commit</span>
            <Icon name="arrow" />
            <span>Argo sync</span>
          </div>
        </div>
        <small className="auth-story__foot">
          Your workloads keep running even when the control plane is offline.
        </small>
      </section>

      <section className="auth-panel">
        <div className="auth-card">
          <div className="auth-card__icon">
            <Icon name={offline ? "refresh" : "terminal"} />
          </div>
          <span className="eyebrow">
            {mode === "offline"
              ? "Connection"
              : mode === "invitation"
                ? "Team invitation"
                : mode === "bootstrap"
                  ? "First run"
                  : "Session required"}
          </span>
          <h2>
            {mode === "offline"
              ? "Control plane unavailable"
              : mode === "invitation"
                ? "Join your Kuberploy team"
                : mode === "bootstrap"
                  ? "Claim this installation"
                  : "Sign in to continue"}
          </h2>
          <p className="auth-card__lead">
            {mode === "offline"
              ? "The UI cannot reach the same-origin API. Check the API service and retry without changing your cluster."
              : mode === "invitation"
                ? "Choose your own password to join. The one-time token from the invitation link is sent directly to this installation and is never saved by the UI."
                : mode === "bootstrap"
                  ? "Use the one-time token printed by the installer. The first account becomes platform administrator."
                  : "Authentication is managed by this installation. Restore your session, then retry."}
          </p>

          {mode === "offline" ? (
            <div className="notice notice--error" role="alert">
              <p>{errorMessage(connectionError)}</p>
            </div>
          ) : mode === "bootstrap" ? (
            <form
              onSubmit={bootstrapForm.handleSubmit((values) =>
                bootstrap.mutate(values),
              )}
              className="stack-form"
            >
              <Field
                label="Display name"
                required
                error={bootstrapForm.formState.errors.displayName?.message}
              >
                <input
                  autoComplete="name"
                  placeholder="Platform admin"
                  {...bootstrapForm.register("displayName", {
                    required: "Enter a display name.",
                  })}
                />
              </Field>
              <Field
                label="Bootstrap token"
                required
                hint="Stored only long enough to create your session."
                error={bootstrapForm.formState.errors.token?.message}
              >
                <input
                  type="password"
                  autoComplete="one-time-code"
                  spellCheck={false}
                  placeholder="kp_bootstrap_••••••••"
                  {...bootstrapForm.register("token", {
                    required: "Enter the installer token.",
                  })}
                />
              </Field>
              <Field
                label="Password"
                required
                hint="At least 12 characters. Kuberploy stores only a hardened password hash."
                error={bootstrapForm.formState.errors.password?.message}
              >
                <input
                  type="password"
                  autoComplete="new-password"
                  {...bootstrapForm.register("password", {
                    required: "Create a password.",
                    minLength: {
                      value: 12,
                      message: "Use at least 12 characters.",
                    },
                  })}
                />
              </Field>
              {bootstrap.error ? (
                <div className="notice notice--error" role="alert">
                  {errorMessage(bootstrap.error)}
                </div>
              ) : null}
              <Button type="submit" busy={bootstrap.isPending}>
                Create administrator <Icon name="arrow" />
              </Button>
            </form>
          ) : mode === "invitation" ? (
            <form
              onSubmit={invitationForm.handleSubmit((values) =>
                acceptInvitation.mutate(values),
              )}
              className="stack-form"
            >
              <Field
                label="Display name"
                required
                error={invitationForm.formState.errors.displayName?.message}
              >
                <input
                  autoComplete="name"
                  placeholder="Your name"
                  {...invitationForm.register("displayName", {
                    required: "Enter your display name.",
                  })}
                />
              </Field>
              <Field
                label="Password"
                required
                hint="At least 12 characters; the invitation remains one-time."
                error={invitationForm.formState.errors.password?.message}
              >
                <input
                  type="password"
                  autoComplete="new-password"
                  {...invitationForm.register("password", {
                    required: "Create a password.",
                    minLength: {
                      value: 12,
                      message: "Use at least 12 characters.",
                    },
                  })}
                />
              </Field>
              <Field
                label="Invitation token"
                required
                hint="One-time secret; this UI does not retain it."
                error={invitationForm.formState.errors.token?.message}
              >
                <input
                  type="password"
                  autoComplete="one-time-code"
                  spellCheck={false}
                  placeholder="kp_invite_••••••••"
                  {...invitationForm.register("token", {
                    required: "Enter your invitation token.",
                  })}
                />
              </Field>
              {acceptInvitation.error ? (
                <div className="notice notice--error" role="alert">
                  {errorMessage(acceptInvitation.error)}
                </div>
              ) : null}
              <Button type="submit" busy={acceptInvitation.isPending}>
                Accept invitation <Icon name="arrow" />
              </Button>
            </form>
          ) : (
            <form
              onSubmit={loginForm.handleSubmit((values) =>
                login.mutate(values),
              )}
              className="stack-form"
            >
              <Field
                label="Login"
                required
                error={loginForm.formState.errors.login?.message}
              >
                <input
                  autoComplete="username"
                  {...loginForm.register("login", {
                    required: "Enter your login.",
                  })}
                />
              </Field>
              <Field
                label="Password"
                required
                error={loginForm.formState.errors.password?.message}
              >
                <input
                  type="password"
                  autoComplete="current-password"
                  {...loginForm.register("password", {
                    required: "Enter your password.",
                  })}
                />
              </Field>
              {login.error ? (
                <div className="notice notice--error" role="alert">
                  {errorMessage(login.error)}
                </div>
              ) : null}
              <Button type="submit" busy={login.isPending}>
                Sign in <Icon name="arrow" />
              </Button>
            </form>
          )}

          {mode !== "offline" ? (
            <Button
              variant="secondary"
              onClick={() => {
                acceptInvitation.reset();
                if (inviteMode && linkedInvitationToken) {
                  onInvitationDismissed?.();
                }
                setInviteMode((current) => !current);
              }}
            >
              <Icon name={inviteMode ? "close" : "user"} />
              {inviteMode
                ? bootstrapRequired
                  ? "Use installation bootstrap"
                  : "Back to sign in"
                : "Use a team invitation"}
            </Button>
          ) : null}
          <Button
            variant="secondary"
            onClick={() => queryClient.invalidateQueries({ queryKey: ["me"] })}
          >
            <Icon name="refresh" /> Retry session
          </Button>
          <p className="auth-card__meta">
            API{" "}
            {meta.data?.apiVersion ??
              meta.data?.version ??
              "version unavailable"}{" "}
            · Same-origin secure session
          </p>
        </div>
      </section>
    </main>
  );
}
