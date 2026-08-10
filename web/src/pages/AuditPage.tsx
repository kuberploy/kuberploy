import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api/client";
import {
  Card,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Skeleton,
} from "../components/ui";
import { formatDate } from "../lib/format";

export function AuditPage() {
  const [targetType, setTargetType] = useState("");
  const [targetId, setTargetId] = useState("");
  const [action, setAction] = useState("");
  const me = useQuery({ queryKey: ["me"], queryFn: api.me, retry: false });
  const hasExactTarget = targetType.length > 0 && targetId.length > 0;
  const canQuery = me.data?.role === "platform-admin" || hasExactTarget;
  const timeline = useQuery({
    queryKey: ["audit-events", targetType, targetId, action],
    queryFn: () =>
      api.auditEvents({
        targetType: targetType || undefined,
        targetId: targetId || undefined,
        action: action || undefined,
        limit: 50,
      }),
    retry: false,
    enabled: canQuery,
  });
  return (
    <div className="page-stack">
      <PageHeader
        title="Audit timeline"
        description="Safe actor, action, resource, outcome, and request metadata. Secret and provider detail is never exposed."
      />
      <Card>
        <div className="inline-form">
          <label>
            Target type
            <input
              value={targetType}
              onChange={(event) => setTargetType(event.target.value)}
              placeholder="deployment"
            />
          </label>
          <label>
            Target ID
            <input
              value={targetId}
              onChange={(event) => setTargetId(event.target.value)}
              placeholder="UUID"
            />
          </label>
          <label>
            Exact action
            <input
              value={action}
              onChange={(event) => setAction(event.target.value)}
              placeholder="deployment.config.accepted"
            />
          </label>
        </div>
        <p className="muted-copy">
          Scoped users must provide an exact target type and ID. Platform
          administrators may leave both blank.
        </p>
      </Card>
      {me.isPending || (canQuery && timeline.isPending) ? (
        <Skeleton lines={5} />
      ) : !canQuery ? (
        <EmptyState
          title="Choose one exact resource"
          description="Enter both target type and target ID to load the audit timeline for a resource you can read."
        />
      ) : timeline.error ? (
        <ErrorPanel
          error={timeline.error}
          onRetry={() => void timeline.refetch()}
        />
      ) : !timeline.data?.items.length ? (
        <EmptyState
          title="No audit events"
          description="No authorized events match the exact filters."
        />
      ) : (
        <Card>
          <div className="member-list" role="list">
            {timeline.data.items.map((event) => (
              <div className="member-row" role="listitem" key={event.id}>
                <div>
                  <strong>{event.action}</strong>
                  <small>
                    {event.targetType} · {event.targetId}
                  </small>
                </div>
                <div>
                  <strong>{event.outcome}</strong>
                  <small>
                    {formatDate(event.createdAt)} · actor {event.actorId}
                  </small>
                </div>
              </div>
            ))}
          </div>
        </Card>
      )}
    </div>
  );
}
