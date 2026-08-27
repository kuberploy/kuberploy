import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api/client";
import {
  Button,
  Card,
  CopyButton,
  EmptyState,
  ErrorPanel,
  MutedCopy,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
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
    <Page>
      <PageHeader
        title="Audit timeline"
        description="Safe actor, action, resource, outcome, and request metadata. Secret and provider detail is never exposed."
      />
      <Card>
        <div className="grid grid-cols-[1fr_1fr_auto] items-end gap-3 to-580:grid-cols-[1fr]">
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
        <MutedCopy>
          Scoped users must provide an exact target type and ID. Platform
          administrators may leave both blank.
        </MutedCopy>
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
          action={
            action ? (
              // The action filter is the one input that can silently exclude
              // everything; clearing it is the way back to a result.
              <Button variant="secondary" onClick={() => setAction("")}>
                Clear action filter
              </Button>
            ) : undefined
          }
        />
      ) : (
        <Card flush>
          <div className="flex flex-col" role="list">
            {timeline.data.items.map((event) => (
              <div
                className="grid min-h-16 grid-cols-[minmax(0,_1.6fr)_auto_minmax(0,_1fr)_auto] items-center gap-y-3 gap-x-5 py-3 px-6 border-b border-b-line last:border-b-0 to-900:grid-cols-[minmax(0,_1fr)_auto] to-900:p-4"
                role="listitem"
                key={event.id}
              >
                <div className="min-w-0 [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-ink [&_strong]:text-sm [&_strong]:font-medium [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_small]:flex [&_small]:min-w-0 [&_small]:items-center [&_small]:gap-1 [&_small]:mt-0.5 [&_small]:text-ink-faint [&_small]:text-xs [&_small_code]:overflow-hidden [&_small_code]:text-ellipsis [&_small_code]:whitespace-nowrap">
                  <strong>{event.action}</strong>
                  <small>
                    {event.targetType} · <code>{event.targetId}</code>
                    <CopyButton value={event.targetId} label="Copy target ID" />
                  </small>
                </div>
                <StatusPill value={event.outcome} />
                <div className="grid min-w-0 gap-0.5 text-ink-soft text-xs [&_span]:overflow-hidden [&_span]:text-ellipsis [&_span]:whitespace-nowrap [&_small]:overflow-hidden [&_small]:text-ellipsis [&_small]:whitespace-nowrap [&_small]:text-ink-faint to-900:col-[1_/_-1] to-900:text-left">
                  <span>actor {event.actorId}</span>
                  {event.requestId ? (
                    <small>
                      request <code>{event.requestId}</code>
                    </small>
                  ) : null}
                </div>
                <time
                  className="text-ink-faint text-xs tabular-nums text-right whitespace-nowrap to-900:col-[1_/_-1] to-900:text-left"
                  dateTime={event.createdAt}
                >
                  {formatDate(event.createdAt)}
                </time>
              </div>
            ))}
          </div>
        </Card>
      )}
    </Page>
  );
}
