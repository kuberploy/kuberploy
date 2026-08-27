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
    <ol className="m-0 p-0 list-none">
      {operations.map((operation) => (
        <li
          key={operation.id}
          className="grid grid-cols-[18px_1fr] gap-2 min-h-[73px] [&:last-child_.timeline_rail::after]:hidden"
        >
          <span className="relative flex justify-center [&::after]:absolute [&::after]:top-3.5 [&::after]:bottom-[-4px] [&::after]:w-px [&::after]:content-[''] [&::after]:bg-line [&_span]:relative [&_span]:z-[1] [&_span]:w-2 [&_span]:h-2 [&_span]:mt-1 [&_span]:border-2 [&_span]:border-white [&_span]:rounded-full [&_span]:bg-mint [&_span]:shadow-[0_0_0_1px_#8edbbb]">
            <span />
          </span>
          <div className="pb-4 [&>div]:flex [&>div]:items-center [&>div]:justify-between [&>div]:gap-2 [&_strong]:text-meta [&_p]:my-1 [&_p]:mx-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.45] [&_small]:text-ink-faint [&_small]:text-xs [&_code]:text-[inherit]">
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
