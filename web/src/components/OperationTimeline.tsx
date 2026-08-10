import type { Operation } from "../api/types";
import { formatDate, titleCase } from "../lib/format";
import { EmptyState, StatusPill } from "./ui";

export function OperationTimeline({
  operations,
  empty,
}: {
  operations: Operation[];
  empty?: string;
}) {
  if (!operations.length) {
    return (
      <EmptyState
        icon="git"
        title="No operations"
        description={empty ?? "The release timeline will appear here."}
        compact
      />
    );
  }
  return (
    <ol className="timeline">
      {operations.map((operation) => (
        <li key={operation.id} className="timeline__item">
          <span className="timeline__rail">
            <span />
          </span>
          <div className="timeline__content">
            <div>
              <strong>{titleCase(operation.kind)}</strong>
              <StatusPill value={operation.state} />
            </div>
            <p>
              {operation.problem?.detail ??
                operation.steps?.at(-1)?.message ??
                operation.target?.name ??
                operation.targetRef?.name ??
                "Operation accepted by the control plane."}
            </p>
            <small>
              {formatDate(operation.updatedAt ?? operation.createdAt)} ·{" "}
              <code>{operation.id.slice(0, 8)}</code>
            </small>
          </div>
        </li>
      ))}
    </ol>
  );
}
