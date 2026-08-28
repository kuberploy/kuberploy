import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ApiError, api } from "../api/client";
import type {
  BuildAttempt,
  DeploymentRouteInput,
  Operation,
} from "../api/types";
import {
  Select,
  Button,
  EmptyState,
  ErrorPanel,
  Field,
  FormGrid,
  Notice,
  buttonVariants,
} from "./ui";
import { Icon } from "./Icon";

type PromotionCommand = {
  attemptId: string;
  idempotencyKey: string;
  draftScope: string;
  environmentId: string;
  replicas: number;
  port: number;
  routeMode: "internal" | "manual" | "sslip";
  hostname: string;
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
  // The picked environment is a preference; the effective one is derived from
  // the environments this project exposes in this render.
  const [environmentChoice, setEnvironmentId] = useState("");
  const environmentId =
    environmentChoice &&
    (!environments.data ||
      environments.data.items.some(
        (environment) =>
          environment.projectId === attempt.projectId &&
          environment.id === environmentChoice,
      ))
      ? environmentChoice
      : "";
  const [replicas, setReplicas] = useState(1);
  const [port, setPort] = useState(8080);
  const [routeMode, setRouteMode] =
    useState<PromotionCommand["routeMode"]>("internal");
  const [hostname, setHostname] = useState("");
  const [accepted, setAccepted] = useState<Operation>();
  const draftScope = JSON.stringify({
    attemptId: attempt.id,
    environmentId,
    replicas,
    port,
    routeMode,
    hostname: hostname.trim().toLowerCase(),
  });
  const draftScopeRef = useRef(draftScope);
  draftScopeRef.current = draftScope;
  const promotionAttempt = useRef<{ signature: string; key: string } | null>(
    null,
  );
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
        command.attemptId,
        {
          environmentId: command.environmentId,
          runtime: {
            replicas: command.replicas,
            ports: [
              { name: "http", containerPort: command.port, protocol: "TCP" },
            ],
            resources: { requests: { cpu: "50m", memory: "100Mi" } },
          },
          route,
        },
        command.idempotencyKey,
      );
    },
    retry: (failures, error) =>
      error instanceof ApiError && error.status === 0 && failures < 1,
    onSuccess: (operation, command) => {
      if (
        command.attemptId !== attempt.id ||
        command.draftScope !== draftScopeRef.current
      )
        return;
      if (promotionAttempt.current?.key === command.idempotencyKey) {
        promotionAttempt.current = null;
      }
      setAccepted(operation);
    },
  });

  const promote = () => {
    const signature = JSON.stringify({
      attemptId: attempt.id,
      environmentId,
      replicas,
      port,
      routeMode,
      hostname: hostname.trim().toLowerCase(),
    });
    const idempotencyKey =
      promotionAttempt.current?.signature === signature
        ? promotionAttempt.current.key
        : crypto.randomUUID();
    promotionAttempt.current = { signature, key: idempotencyKey };
    mutation.mutate({
      attemptId: attempt.id,
      idempotencyKey,
      draftScope,
      environmentId,
      replicas,
      port,
      routeMode,
      hostname,
    });
  };

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
      <Notice tone="success" role="status">
        <div>
          <strong>Promotion accepted</strong>
          <p>
            The server derived the application and immutable image from this
            exact build.
          </p>
        </div>
        <Link
          className={buttonVariants({ variant: "secondary" })}
          to="/operations/$operationId"
          params={{ operationId: accepted.id }}
        >
          Open operation <Icon name="arrow" />
        </Link>
      </Notice>
    );
  }
  return (
    <FormGrid>
      <Field label="Environment" required>
        <Select
          value={environmentId}
          onChange={(event) => setEnvironmentId(event.target.value)}
        >
          <option value="">Select a same-project environment</option>
          {projectEnvironments.map((environment) => (
            <option key={environment.id} value={environment.id}>
              {environment.name}
            </option>
          ))}
        </Select>
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
        <Select
          value={routeMode}
          onChange={(event) =>
            setRouteMode(event.target.value as PromotionCommand["routeMode"])
          }
        >
          <option value="internal">Internal only</option>
          <option value="sslip">Free sslip.io hostname</option>
          <option value="manual">My hostname</option>
        </Select>
      </Field>
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
          onClick={promote}
        >
          <Icon name="deploy" /> Promote verified build
        </Button>
      </div>
    </FormGrid>
  );
}
