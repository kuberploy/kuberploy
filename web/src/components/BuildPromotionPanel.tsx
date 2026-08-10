import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { ApiError, api } from "../api/client";
import type {
  BuildAttempt,
  DeploymentRouteInput,
  Operation,
  SchedulingProfileRef,
} from "../api/types";
import { Button, EmptyState, ErrorPanel, Field } from "./ui";
import { Icon } from "./Icon";
import { SchedulingProfilePicker } from "./SchedulingProfilePicker";

type PromotionCommand = {
  idempotencyKey: string;
  environmentId: string;
  replicas: number;
  port: number;
  routeMode: "internal" | "manual" | "sslip";
  hostname: string;
  schedulingProfile?: SchedulingProfileRef;
};

export function BuildPromotionPanel({
  attempt,
  humanSession,
  gitOpsReady,
}: {
  attempt: BuildAttempt;
  humanSession: boolean;
  gitOpsReady: boolean;
}) {
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled: attempt.state === "succeeded" && humanSession,
  });
  const [environmentId, setEnvironmentId] = useState("");
  const [replicas, setReplicas] = useState(1);
  const [port, setPort] = useState(8080);
  const [routeMode, setRouteMode] =
    useState<PromotionCommand["routeMode"]>("internal");
  const [hostname, setHostname] = useState("");
  const [schedulingProfile, setSchedulingProfile] =
    useState<SchedulingProfileRef>();
  const [accepted, setAccepted] = useState<Operation>();
  const projectEnvironments =
    environments.data?.items.filter(
      (item) => item.projectId === attempt.projectId,
    ) ?? [];
  const mutation = useMutation({
    mutationFn: (command: PromotionCommand) => {
      let route: DeploymentRouteInput | undefined;
      if (command.routeMode === "manual") {
        route = {
          hostname: command.hostname.trim().toLowerCase(),
          dnsMode: "manual",
          pathPrefix: "/",
          tlsMode: "httpOnly",
        };
      } else if (command.routeMode === "sslip") {
        route = { dnsMode: "sslip", pathPrefix: "/", tlsMode: "httpOnly" };
      }
      return api.promoteBuildAttempt(
        attempt.id,
        {
          environmentId: command.environmentId,
          runtime: {
            replicas: command.replicas,
            ports: [
              { name: "http", containerPort: command.port, protocol: "TCP" },
            ],
            resources: { requests: { cpu: "50m", memory: "100Mi" } },
            ...(command.schedulingProfile
              ? { schedulingProfile: command.schedulingProfile }
              : {}),
          },
          route,
        },
        command.idempotencyKey,
      );
    },
    retry: (failures, error) =>
      error instanceof ApiError && error.status === 0 && failures < 1,
    onSuccess: setAccepted,
  });

  if (attempt.state !== "succeeded" || !attempt.image) {
    return (
      <EmptyState
        icon="deploy"
        title="Promotion waits for a verified release"
        description="This command appears only after the exact build succeeds and its immutable registry result is available."
        compact
      />
    );
  }
  if (!humanSession) {
    return (
      <EmptyState
        icon="deploy"
        title="Human session required"
        description="MVP promotion requires a cookie session plus exact builds.read and resources.write permissions; automation scopes stay separated."
        compact
      />
    );
  }
  if (!gitOpsReady) {
    return (
      <EmptyState
        icon="deploy"
        title="Protected rollout is not ready"
        description="Promotion stays disabled until both protected Git projection and Argo desired-state readiness are fresh."
        compact
      />
    );
  }
  if (accepted) {
    return (
      <div className="notice notice--success" role="status">
        <div>
          <strong>Promotion accepted</strong>
          <p>
            The server derived the application and immutable image from this
            exact build.
          </p>
        </div>
        <Link
          className="button button--secondary"
          to="/operations/$operationId"
          params={{ operationId: accepted.id }}
        >
          Open operation <Icon name="arrow" />
        </Link>
      </div>
    );
  }
  return (
    <div className="form-grid">
      <Field label="Environment" required>
        <select
          value={environmentId}
          onChange={(event) => setEnvironmentId(event.target.value)}
        >
          <option value="">Select a same-project environment</option>
          {projectEnvironments.map((environment) => (
            <option key={environment.id} value={environment.id}>
              {environment.name}
            </option>
          ))}
        </select>
      </Field>
      <Field label="Replicas" required>
        <input
          type="number"
          min={1}
          max={100}
          value={replicas}
          onChange={(event) => setReplicas(Number(event.target.value))}
        />
      </Field>
      <Field label="Container port" required>
        <input
          type="number"
          min={1}
          max={65535}
          value={port}
          onChange={(event) => setPort(Number(event.target.value))}
        />
      </Field>
      <Field label="HTTP route">
        <select
          value={routeMode}
          onChange={(event) =>
            setRouteMode(event.target.value as PromotionCommand["routeMode"])
          }
        >
          <option value="internal">Internal only</option>
          <option value="sslip">Free sslip.io hostname</option>
          <option value="manual">My hostname</option>
        </select>
      </Field>
      <SchedulingProfilePicker
        environmentId={environmentId}
        value={schedulingProfile}
        onChange={setSchedulingProfile}
      />
      {routeMode === "manual" ? (
        <Field label="Hostname" required>
          <input
            value={hostname}
            placeholder="api.example.com"
            onChange={(event) => setHostname(event.target.value)}
          />
        </Field>
      ) : null}
      {environments.error ? <ErrorPanel error={environments.error} /> : null}
      {mutation.error ? (
        <ErrorPanel error={mutation.error} title="Promotion was not accepted" />
      ) : null}
      <div>
        <Button
          busy={mutation.isPending}
          disabled={
            !environmentId ||
            replicas < 1 ||
            replicas > 100 ||
            port < 1 ||
            port > 65535 ||
            (routeMode === "manual" && !hostname.trim())
          }
          onClick={() =>
            mutation.mutate({
              idempotencyKey: crypto.randomUUID(),
              environmentId,
              replicas,
              port,
              routeMode,
              hostname,
              schedulingProfile,
            })
          }
        >
          <Icon name="deploy" /> Promote verified build
        </Button>
      </div>
    </div>
  );
}
