import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useLayoutEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type { Principal, User } from "../api/types";
import {
  clearInvitationFragment,
  invitationTokenFromHash,
} from "../lib/invitationLink";
import { Button, Eyebrow, Field, Notice } from "./ui";
import { Icon } from "./Icon";

type BootstrapForm = {
  email: string;
  displayName: string;
  token: string;
  password: string;
};

type InvitationAcceptanceForm = {
  displayName: string;
  token: string;
  password: string;
};

type LoginForm = { email: string; password: string };

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
  const [linkedInvitationToken, setLinkedInvitationToken] = useState(
    () =>
      invitationToken ?? invitationTokenFromHash(window.location.hash) ?? "",
  );
  const [invitationInputToken, setInvitationInputToken] = useState(
    () =>
      invitationToken ?? invitationTokenFromHash(window.location.hash) ?? "",
  );
  const [inviteMode, setInviteMode] = useState(
    () => linkedInvitationToken.length > 0,
  );
  const [invitationCompleted, setInvitationCompleted] = useState(false);
  const meta = useQuery({
    queryKey: ["meta"],
    queryFn: api.meta,
    retry: false,
  });
  const bootstrapForm = useForm<BootstrapForm>({
    defaultValues: {
      email: "",
      displayName: "Administrator",
      token: "",
      password: "",
    },
  });
  const invitationForm = useForm<InvitationAcceptanceForm>({
    defaultValues: {
      displayName: "",
      token: linkedInvitationToken,
      password: "",
    },
  });
  const loginForm = useForm<LoginForm>({
    defaultValues: { email: "", password: "" },
  });
  const establishSession = async (user: User) => {
    await queryClient.cancelQueries();
    queryClient.removeQueries({
      predicate: (query) => query.queryKey[0] !== "me",
    });
    queryClient.setQueryData(["me"], sessionPrincipal(user));
  };
  const bootstrap = useMutation({
    mutationFn: api.bootstrap,
    onSuccess: async (user) => {
      bootstrapForm.reset();
      await establishSession(user);
    },
  });
  const acceptInvitation = useMutation({
    mutationFn: api.acceptInvitation,
    onSuccess: async (user) => {
      setInvitationCompleted(true);
      setInviteMode(false);
      setLinkedInvitationToken("");
      setInvitationInputToken("");
      invitationForm.reset({ displayName: "", token: "", password: "" });
      await establishSession(user);
      onInvitationAccepted?.();
    },
  });
  const login = useMutation({
    mutationFn: api.login,
    onSuccess: async (user) => {
      loginForm.reset();
      await establishSession(user);
    },
  });

  useLayoutEffect(() => {
    if (invitationCompleted) {
      clearInvitationFragment();
      return;
    }
    const incomingInvitationToken =
      invitationToken ?? invitationTokenFromHash(window.location.hash) ?? "";
    if (
      incomingInvitationToken &&
      incomingInvitationToken !== linkedInvitationToken
    ) {
      setLinkedInvitationToken(incomingInvitationToken);
      setInvitationInputToken(incomingInvitationToken);
      setInviteMode(true);
    }
    clearInvitationFragment();
    const token = incomingInvitationToken || linkedInvitationToken;
    if (token) {
      invitationForm.setValue("token", token, {
        shouldDirty: false,
        shouldTouch: false,
        shouldValidate: false,
      });
    }
  }, [
    invitationCompleted,
    invitationForm,
    invitationToken,
    linkedInvitationToken,
  ]);

  useEffect(() => {
    if (invitationCompleted) return;
    const token =
      invitationToken ??
      linkedInvitationToken ??
      invitationTokenFromHash(window.location.hash);
    if (token) {
      invitationForm.reset(
        {
          ...invitationForm.getValues(),
          token,
        },
        {
          keepErrors: true,
          keepTouched: true,
        },
      );
    }
  }, [
    invitationCompleted,
    invitationForm,
    invitationToken,
    linkedInvitationToken,
  ]);

  const connectionOffline =
    connectionError &&
    !(
      connectionError instanceof Error &&
      "status" in connectionError &&
      (connectionError as { status?: number }).status === 401
    );
  const offline = Boolean(connectionOffline) || (!inviteMode && meta.isError);
  const bootstrapRequired = meta.data?.bootstrapRequired === true;
  const mode = offline
    ? "offline"
    : inviteMode
      ? "invitation"
      : meta.isPending
        ? "loading"
        : meta.isError
          ? "offline"
          : bootstrapRequired
            ? "bootstrap"
            : "session";
  const unavailableError = connectionOffline ?? meta.error ?? connectionError;

  return (
    <main className="grid min-h-screen grid-cols-[minmax(390px,_1.1fr)_minmax(480px,_0.9fr)] bg-surface to-820:grid-cols-[1fr]">
      <section className="relative flex min-h-screen flex-col justify-between overflow-hidden py-10 px-10 text-white bg-[#18181b] [&::before]:absolute [&::before]:right-[-120px] [&::before]:bottom-[80px] [&::before]:w-[410px] [&::before]:h-[410px] [&::before]:content-none [&::before]:transform-[rotate(19deg)] [&::before]:border [&::before]:border-[rgba(67,_215,_160,_0.12)] [&::before]:rounded-[70px] [&::before]:shadow-[0_0_0_48px_rgba(67_215_160_0.025)_0_0_0_96px_rgba(67_215_160_0.018)] to-820:hidden">
        <div className="relative p-0">
          <span
            className="relative inline-flex size-[27px] items-end justify-center gap-0.5 rounded-lg border border-line-strong bg-mint-soft p-1.5 [&>span]:w-[3px] [&>span]:rounded-sm [&>span]:bg-mint [&>span:nth-child(1)]:h-[7px] [&>span:nth-child(2)]:h-[14px] [&>span:nth-child(3)]:h-[10px]"
            aria-hidden="true"
          >
            <span />
            <span />
            <span />
          </span>
          <span>Kuberploy</span>
        </div>
        <div className="relative max-w-[620px] my-[auto] mx-0 [&_h1]:mt-3 [&_h1]:mx-0 [&_h1]:mb-5 [&_h1]:text-[#f1fff8] [&_h1]:text-[clamp(40px,_5vw,_70px)] [&_h1]:font-semibold [&_h1]:tracking-[-0.02em] [&_h1]:leading-[0.98] [&>p]:max-w-[540px] [&>p]:m-0 [&>p]:text-[#95aa9f] [&>p]:text-meta [&>p]:leading-[1.7]">
          <Eyebrow className="text-[#6de7b8]">
            Kubernetes, without the ceremony
          </Eyebrow>
          <h1>
            Ship from a digest.
            <br />
            Reconcile from Git.
          </h1>
          <p>
            A self-hosted control plane for content-addressed releases, explicit
            routes, and an honest view of what Argo CD is doing.
          </p>
          <div
            className="flex w-max items-center gap-3 mt-8 py-3 px-3 border border-[rgba(255,_255,_255,_0.08)] rounded-[9px] text-[#b6c9bf] bg-[rgba(255,_255,_255,_0.03)] font-mono text-meta [&_svg]:w-[13px] [&_svg]:text-mint"
            aria-label="Kuberploy App delivery flow"
          >
            <span>OCI image</span>
            <Icon name="arrow" />
            <span>Git commit</span>
            <Icon name="arrow" />
            <span>Argo sync</span>
          </div>
        </div>
        <small className="relative text-ink-soft text-meta">
          Your workloads keep running even when the control plane is offline.
        </small>
      </section>

      <section className="grid place-items-center p-12 bg-surface-soft to-820:min-h-screen to-820:p-8">
        <div className="w-[min(390px,_100%)] [&_h2]:mt-2 [&_h2]:mx-0 [&_h2]:mb-2 [&_h2]:text-ink [&_h2]:text-[28px] [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&>[data-slot='button']]:w-full [&>[data-slot='button']]:mt-2">
          <div className="grid w-[45px] h-[45px] place-items-center mb-5 border border-[#c4e8d8] rounded-panel text-mint-dark bg-mint-soft [&_svg]:w-5">
            <Icon name={offline ? "refresh" : "terminal"} />
          </div>
          <Eyebrow>
            {mode === "offline"
              ? "Connection"
              : mode === "loading"
                ? "Installation"
                : mode === "invitation"
                  ? "Team invitation"
                  : mode === "bootstrap"
                    ? "First run"
                    : "Session required"}
          </Eyebrow>
          <h2>
            {mode === "offline"
              ? "Control plane unavailable"
              : mode === "loading"
                ? "Checking installation"
                : mode === "invitation"
                  ? "Join your Kuberploy team"
                  : mode === "bootstrap"
                    ? "Claim this installation"
                    : "Sign in to continue"}
          </h2>
          <p className="mt-0 mx-0 mb-6 text-ink-soft text-[11px] leading-[1.65]">
            {mode === "offline"
              ? "The UI cannot reach the same-origin API. Check the API service and retry without changing your cluster."
              : mode === "loading"
                ? "Checking whether this installation needs its first administrator before showing the correct sign-in flow."
                : mode === "invitation"
                  ? "Choose your own password to join. The one-time token from the invitation link is sent directly to this installation and is never saved by the UI."
                  : mode === "bootstrap"
                    ? "Use the one-time token printed by the installer. The first account becomes platform administrator."
                    : "Authentication is managed by this installation. Restore your session, then retry."}
          </p>

          {mode === "offline" ? (
            <Notice tone="error" role="alert">
              <p>{errorMessage(unavailableError)}</p>
            </Notice>
          ) : mode === "loading" ? (
            <div
              className="flex items-center gap-3 min-h-[43px] mb-3 text-ink-soft text-[11px]"
              role="status"
            >
              <span
                className="inline-block size-3.5 animate-spin rounded-full border-2 border-current border-r-transparent text-mint-dark"
                aria-hidden="true"
              />
              <span>Reading installation state…</span>
            </div>
          ) : mode === "bootstrap" ? (
            <form
              onSubmit={bootstrapForm.handleSubmit((values) =>
                bootstrap.mutate(values),
              )}
              className="flex flex-col gap-4 mb-3 [&_[data-slot='button']]:min-h-[43px] [&_[data-slot='button']]:mt-1"
            >
              <Field
                label="Admin email"
                required
                hint="Used to sign in; your display name is shown separately."
                error={bootstrapForm.formState.errors.email?.message}
              >
                <input
                  type="email"
                  autoComplete="email"
                  placeholder="admin@example.com"
                  {...bootstrapForm.register("email", {
                    required: "Enter an administrator email.",
                  })}
                />
              </Field>
              <Field
                label="Display name"
                required
                hint="Shown in the workspace; sign in with the admin email above."
                error={bootstrapForm.formState.errors.displayName?.message}
              >
                <input
                  autoComplete="name"
                  placeholder="Administrator"
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
                <Notice tone="error" role="alert">
                  {errorMessage(bootstrap.error)}
                </Notice>
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
              className="flex flex-col gap-4 mb-3 [&_[data-slot='button']]:min-h-[43px] [&_[data-slot='button']]:mt-1"
            >
              <Field
                label="Display name"
                required
                hint="Shown in the workspace; the invitation email is the sign-in identity."
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
                  value={invitationInputToken}
                  onChange={(event) => {
                    const token = event.currentTarget.value;
                    setInvitationInputToken(token);
                    invitationForm.setValue("token", token, {
                      shouldDirty: true,
                      shouldTouch: true,
                      shouldValidate: false,
                    });
                  }}
                />
              </Field>
              {acceptInvitation.error ? (
                <Notice tone="error" role="alert">
                  {errorMessage(acceptInvitation.error)}
                </Notice>
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
              className="flex flex-col gap-4 mb-3 [&_[data-slot='button']]:min-h-[43px] [&_[data-slot='button']]:mt-1"
            >
              <Field
                label="Email"
                required
                error={loginForm.formState.errors.email?.message}
              >
                <input
                  type="email"
                  autoComplete="email"
                  {...loginForm.register("email", {
                    required: "Enter your email.",
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
                <Notice tone="error" role="alert">
                  {errorMessage(login.error)}
                </Notice>
              ) : null}
              <Button type="submit" busy={login.isPending}>
                Sign in <Icon name="arrow" />
              </Button>
            </form>
          )}

          {mode !== "offline" && mode !== "loading" ? (
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
          <p className="mt-5 mx-0 mb-0 text-[#98a39e] text-xs text-center">
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
