import {
  Children,
  Fragment,
  cloneElement,
  isValidElement,
  useId,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ComponentProps,
  type ElementType,
  type HTMLAttributes,
  type KeyboardEvent,
  type PropsWithChildren,
  type ReactNode,
} from "react";
import { Icon, type IconName } from "./Icon";
import { cn } from "@/lib/utils";
import { Button as ButtonPrimitive, buttonVariants } from "./shadcn/button";
import { Card as CardPrimitive } from "./shadcn/card";
import { errorMessage } from "../api/client";
import { operationTone, titleCase } from "../lib/format";
import { useCopyToClipboard } from "../lib/clipboard";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "./shadcn/dialog";
export { Select } from "./shadcn/select";

/*
 * The dialog primitives are the one piece of vendored shadcn this app still
 * uses. Re-exported here so no page imports out of `components/shadcn`
 * directly: the boundary is what lets the primitive be restyled or replaced in
 * one place instead of in every caller.
 */
/**
 * For elements that must not be a <button>: a router <Link> or an <a> that
 * should read as a button. Gives them the button's classes without lying about
 * what the element is.
 */
export { buttonVariants };

export {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
};

/**
 * Roving tabindex for a single-choice control group -- a tablist or a
 * radiogroup. Both patterns require that the group holds exactly one tab stop
 * and that the arrow keys move between its items; a group of plain buttons
 * puts every item in the tab order and answers no arrow key at all, which is
 * why `role="tablist"` without this is a promise the markup does not keep.
 *
 * Activation is manual: an arrow moves focus, Space or Enter commits. Native
 * buttons already fire a click on both, so no key handling is needed for that.
 */
// Editable rows must not be keyed by their position. A key is React's identity
// for a row: keyed by index, deleting row 2 makes row 3 inherit row 2's DOM
// node, so focus, selection and uncontrolled input state land on the wrong row.
// The draft models here are plain YAML-shaped arrays with no ids, so identity
// is minted here and follows the row: callers tell the hook when a row is
// removed or moved rather than letting it guess from a length change.
export function useRowKeys(length: number) {
  const idsRef = useRef<string[]>([]);
  const counterRef = useRef(0);
  if (idsRef.current.length < length) {
    const grown = [...idsRef.current];
    while (grown.length < length) {
      counterRef.current += 1;
      grown.push(`row-${counterRef.current}`);
    }
    idsRef.current = grown;
  } else if (idsRef.current.length > length) {
    idsRef.current = idsRef.current.slice(0, length);
  }
  const ids = idsRef.current;
  return {
    keyAt: (index: number) => ids[index] ?? `row-${index}`,
    removeAt: (index: number) => {
      idsRef.current = ids.filter((_, position) => position !== index);
    },
    moveRow: (from: number, to: number) => {
      const next = [...ids];
      const [moved] = next.splice(from, 1);
      if (moved) next.splice(to, 0, moved);
      idsRef.current = next;
    },
  };
}

export function useRovingFocus(count: number, activeIndex: number) {
  const items = useRef<Array<HTMLElement | null>>([]);
  const [focused, setFocused] = useState<number | null>(null);
  const current = focused ?? (activeIndex >= 0 ? activeIndex : 0);

  const focusAt = (start: number, step: number) => {
    for (let hop = 1; hop <= count; hop += 1) {
      const next = (start + step * hop + count * count) % count;
      const node = items.current[next];
      // A disabled item is not a stop. Where every other item is disabled this
      // walks the whole ring and lands back on the one that has focus.
      if (node && !(node as HTMLButtonElement).disabled) {
        setFocused(next);
        node.focus();
        return;
      }
    }
  };

  return (index: number) => ({
    ref: (node: HTMLElement | null) => {
      items.current[index] = node;
    },
    tabIndex: index === current ? 0 : -1,
    onFocus: () => setFocused(index),
    onKeyDown: (event: KeyboardEvent<HTMLElement>) => {
      const step =
        event.key === "ArrowRight" || event.key === "ArrowDown"
          ? 1
          : event.key === "ArrowLeft" || event.key === "ArrowUp"
            ? -1
            : 0;
      if (step) {
        event.preventDefault();
        focusAt(index, step);
        return;
      }
      if (event.key === "Home" || event.key === "End") {
        event.preventDefault();
        focusAt(
          event.key === "Home" ? -1 : count,
          event.key === "Home" ? 1 : -1,
        );
      }
    },
  });
}

export function Button({
  variant = "primary",
  busy,
  children,
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost" | "danger";
  busy?: boolean;
}) {
  return (
    <ButtonPrimitive
      {...props}
      variant={variant}
      className={className}
      disabled={busy || props.disabled}
    >
      {busy ? (
        <span
          className="inline-block size-3.5 animate-spin rounded-full border-2 border-current border-r-transparent"
          aria-hidden="true"
        />
      ) : null}
      {children}
    </ButtonPrimitive>
  );
}

/**
 * Copy affordance for values an operator has to reproduce exactly: confirmation
 * tokens, image digests, deploy keys, invitation links. Renders as an icon
 * button by default and swaps to a check for two seconds after a successful
 * copy so the feedback is visible without a toast.
 */
export function CopyButton({
  value,
  label = "Copy",
  variant = "icon",
}: {
  value: string;
  label?: string;
  variant?: "icon" | "inline";
}) {
  const { state, copy } = useCopyToClipboard();
  const title =
    state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : label;
  return (
    <button
      type="button"
      className={cn(
        "inline-flex items-center gap-1.5 rounded border border-transparent text-xs",
        "transition-[color,background-color] duration-(--motion-fast) ease-(--ease-standard)",
        "hover:bg-surface-soft hover:text-ink focus-visible:outline-3 focus-visible:outline-offset-1 focus-visible:outline-ring/25",
        variant === "icon"
          ? "size-6 shrink-0 justify-center pointer-coarse:size-8"
          : "min-h-7 border-line-strong bg-surface px-2 pointer-coarse:min-h-8",
        state === "copied"
          ? "text-mint-dark"
          : state === "failed"
            ? "text-red"
            : "text-ink-faint",
        "[&_svg]:size-3.5 [&_svg]:shrink-0",
      )}
      onClick={() => void copy(value)}
      aria-label={`${label}: ${value}`}
      title={title}
    >
      <Icon name={state === "copied" ? "check" : "copy"} />
      {variant === "inline" ? <span>{title}</span> : null}
      <span className="sr-only" role="status">
        {state === "copied"
          ? "Copied to clipboard"
          : state === "failed"
            ? "Copy failed"
            : ""}
      </span>
    </button>
  );
}

/**
 * A value the operator has to read character by character, with its copy
 * affordance attached. Used for confirmation tokens and identifiers.
 */
export function CopyableCode({
  value,
  label,
}: {
  value: string;
  label?: string;
}) {
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      <code className="overflow-hidden text-xs font-semibold text-ellipsis whitespace-nowrap text-ink">
        {value}
      </code>
      <CopyButton value={value} label={label ?? "Copy value"} />
    </span>
  );
}

const pillTones = {
  good: "text-tone-good border-tone-good-line bg-tone-good-surface",
  bad: "text-tone-bad border-tone-bad-line bg-tone-bad-surface",
  warn: "text-tone-warn border-tone-warn-line bg-tone-warn-surface",
  busy: "text-tone-busy border-tone-busy-line bg-tone-busy-surface",
  neutral: "text-ink-soft border-line bg-surface-soft",
} as const;

const pillDots = {
  good: "bg-tone-good-dot",
  bad: "bg-tone-bad-dot",
  warn: "bg-tone-warn-dot",
  busy: "bg-tone-busy-dot animate-pulse",
  neutral: "bg-ink-faint",
} as const;

export function StatusPill({
  value,
  label,
}: {
  value?: string;
  label?: string;
}) {
  const tone = operationTone(value) as keyof typeof pillTones;
  return (
    <span
      data-slot="status-pill"
      className={cn(
        "inline-flex min-h-7 w-max items-center gap-1.5 rounded-full border px-3",
        "text-xs font-medium whitespace-nowrap",
        pillTones[tone] ?? pillTones.neutral,
      )}
    >
      <span
        className={cn(
          "size-[5px] rounded-full",
          pillDots[tone] ?? pillDots.neutral,
        )}
      />
      {label ?? titleCase(value)}
    </span>
  );
}

export function PageHeader({
  eyebrow,
  title,
  description,
  actions,
}: {
  eyebrow?: string;
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="mb-8 flex flex-wrap items-end justify-between gap-x-6 gap-y-3 page-to-580:flex-col page-to-580:flex-nowrap page-to-580:items-stretch">
      {/* An explicit 1fr track: without it the implicit column takes
          `min-width: auto` from its widest child, so one long identifier in the
          eyebrow sizes the column past the header it sits in. */}
      <div className="grid min-w-0 grid-cols-[minmax(0,1fr)] gap-2">
        {eyebrow ? <Eyebrow>{eyebrow}</Eyebrow> : null}
        <h1 className="text-[clamp(24px,2.4vw,30px)] leading-[1.15] font-semibold tracking-[-0.02em] break-words text-ink">
          {title}
        </h1>
        {/* Descriptions carry image references and revision hashes: single
            unbreakable tokens that otherwise overflow the box invisibly and
            drag the page into horizontal scroll. */}
        {description ? (
          <p className="max-w-[68ch] text-sm leading-[1.55] break-words text-ink-soft">
            {description}
          </p>
        ) : null}
      </div>
      {actions ? (
        <div className="flex min-w-0 flex-wrap items-center gap-2 page-to-580:w-full">
          {actions}
        </div>
      ) : null}
    </header>
  );
}

export function EmptyState({
  icon = "layers",
  title,
  description,
  action,
  compact = false,
}: {
  icon?: IconName;
  title: string;
  description: string;
  action?: ReactNode;
  compact?: boolean;
}) {
  return (
    <div
      className={cn(
        // The min-height is the point of an empty state: it holds the space the
        // populated view will occupy, so the page does not jump when data
        // arrives. `compact` reserves less and drops the boundary, because it
        // sits inside a card that already draws one.
        "flex flex-col items-center justify-center p-10 text-center",
        compact
          ? "min-h-[180px]"
          : "min-h-[310px] rounded-panel border border-dashed border-line-strong",
      )}
    >
      <span className="mb-3 grid size-[42px] place-items-center rounded-[11px] border border-line bg-surface-soft text-mint-dark [&_svg]:w-[19px]">
        <Icon name={icon} />
      </span>
      <h3 className="text-lead font-semibold tracking-[-0.015em] text-ink">
        {title}
      </h3>
      <p className="mt-[7px] mb-[17px] max-w-[430px] text-meta leading-[1.55] text-ink-soft">
        {description}
      </p>
      {action}
    </div>
  );
}

const noticeTones = {
  error: "text-tone-bad border-tone-bad-line border-l-red bg-tone-bad-surface",
  warning:
    "text-tone-warn border-tone-warn-line border-l-amber bg-tone-warn-surface",
  success:
    "text-tone-good border-tone-good-line border-l-mint bg-tone-good-surface",
  info: "text-tone-info border-tone-info-line border-l-blue bg-tone-info-surface",
  neutral: "text-ink-soft border-line border-l-line-strong bg-surface-soft",
} as const;

/**
 * A banner tied to the state of the view it sits in. `border-l-3` carries the
 * tone so the colour reads even when the fill is nearly the page background.
 */
export function Notice({
  tone = "neutral",
  children,
  className,
  ...props
}: HTMLAttributes<HTMLDivElement> & {
  tone?: keyof typeof noticeTones;
}) {
  return (
    <div
      data-slot="notice"
      {...props}
      className={cn(
        "my-4 flex flex-wrap items-center gap-x-4 gap-y-2",
        "rounded-lg border border-l-[3px] px-4 py-3 text-meta",
        "animate-[fade-rise_var(--motion-base)_var(--ease-standard)]",
        // A notice may lead with an icon. Without a size it inherits the
        // flex line's height and fills the banner.
        "[&>svg]:size-[18px] [&>svg]:shrink-0 [&>svg]:text-current",
        // The message takes the free width so it sits beside the icon rather
        // than being pushed across the banner; a trailing action still lands
        // hard right because the message grew into the space between them.
        "[&>div:first-of-type]:min-w-0 [&>div:first-of-type]:flex-1",
        "[&_strong]:block [&_strong]:text-meta [&_strong]:font-medium",
        "[&_p]:mt-1 [&_p]:leading-[1.5] [&_p]:break-words",
        noticeTones[tone],
        className,
      )}
    >
      {children}
    </div>
  );
}

export function ErrorPanel({
  error,
  onRetry,
  title = "Could not load this view",
}: {
  error: unknown;
  onRetry?: () => void;
  title?: string;
}) {
  return (
    <Notice tone="error" role="alert">
      <div>
        <strong>{title}</strong>
        <p>{errorMessage(error)}</p>
      </div>
      {onRetry ? (
        <Button variant="secondary" onClick={onRetry}>
          <Icon name="refresh" /> Retry
        </Button>
      ) : null}
    </Notice>
  );
}

export function ConfirmDialog({
  title,
  description,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  icon = "settings",
  tone = "danger",
  busy = false,
  confirmation,
  confirmationLabel = "Confirm deletion",
  error,
  onConfirm,
  onCancel,
}: {
  title: string;
  description: string;
  confirmLabel?: string;
  cancelLabel?: string;
  icon?: IconName;
  tone?: "danger" | "neutral";
  busy?: boolean;
  confirmation?: string;
  confirmationLabel?: string;
  error?: unknown;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const [confirmationValue, setConfirmationValue] = useState("");
  const confirmed =
    confirmation === undefined || confirmationValue === confirmation;
  const mismatched = confirmationValue.length > 0 && !confirmed;
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onCancel();
      }}
    >
      <DialogContent
        // The original rule set a 9px description and a white border on a light
        // surface; both are corrected here to the app's own floor and token.
        className="w-[min(500px,100%)] max-w-none rounded-[14px] border border-line bg-surface p-[27px] shadow-overlay page-to-580:p-[21px] sm:max-w-lg"
        role="alertdialog"
        showCloseButton={false}
      >
        <div className="grid grid-cols-[40px_minmax(0,_1fr)] items-start gap-4 [&_p]:mt-2 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.55] to-460:grid-cols-[minmax(0,_1fr)] to-460:gap-3">
          <span className="grid w-10 h-10 place-items-center border border-mint-line rounded-lg text-mint-dark bg-mint-soft [&_svg]:w-[19px] [&_svg]:h-[19px]">
            <Icon name={icon} />
          </span>
          <div>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </div>
        </div>
        {confirmation !== undefined ? (
          <div className="grid gap-2 p-4 border border-line rounded-lg bg-surface-soft [&_input]:bg-surface [&_input]:font-mono [&_input[aria-invalid='true']]:border-red [&_input[aria-invalid='true']:focus]:shadow-[0_0_0_3px_color-mix(in_srgb_var(--red)_18%_transparent)]">
            <span className="flex items-center flex-wrap gap-1.5 text-ink text-meta font-semibold">
              Type{" "}
              <CopyableCode
                value={confirmation}
                label="Copy the confirmation phrase"
              />{" "}
              to confirm
            </span>
            <input
              autoFocus
              value={confirmationValue}
              aria-label={confirmationLabel}
              aria-invalid={mismatched || undefined}
              autoComplete="off"
              autoCorrect="off"
              autoCapitalize="off"
              spellCheck={false}
              placeholder={confirmation}
              onChange={(event) => setConfirmationValue(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter" && confirmed && !busy) {
                  event.preventDefault();
                  onConfirm();
                }
              }}
            />
            <span
              className="min-h-[1em] text-red text-xs leading-none"
              aria-live="polite"
            >
              {mismatched ? "Does not match yet." : "\u00a0"}
            </span>
          </div>
        ) : null}
        {error ? (
          <Notice tone="error" role="alert">
            {errorMessage(error)}
          </Notice>
        ) : null}
        <div className="to-680:items-stretch to-680:flex-col flex justify-end flex-wrap gap-2 mt-1 to-460:[&_[data-slot='button']]:flex-auto">
          <Button variant="secondary" disabled={busy} onClick={onCancel}>
            {cancelLabel}
          </Button>
          <Button
            variant={tone === "danger" ? "danger" : "primary"}
            busy={busy}
            onClick={onConfirm}
            disabled={busy || !confirmed}
          >
            {confirmLabel}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function Card({
  flush = false,
  children,
  className,
}: PropsWithChildren<{ flush?: boolean; className?: string }>) {
  return (
    <CardPrimitive className={cn(flush && "overflow-hidden p-0", className)}>
      {children}
    </CardPrimitive>
  );
}

export function Skeleton({ lines = 3 }: { lines?: number }) {
  return (
    <div className="flex flex-col gap-3 py-1.5" aria-label="Loading">
      {Array.from({ length: lines }, (_, index) => (
        <span
          key={index}
          className="h-[11px] animate-pulse rounded bg-line"
          style={{ width: `${92 - index * 11}%` }}
        />
      ))}
    </div>
  );
}

type FieldControlProps = {
  id?: string;
  children?: ReactNode;
  "aria-describedby"?: string;
};

const labelableFieldControls = new Set([
  "button",
  "input",
  "select",
  "textarea",
]);

function wireNestedFieldControl(
  node: ReactNode,
  controlId: string,
  descriptionId?: string,
): { node: ReactNode; wired: boolean } {
  if (!isValidElement<FieldControlProps>(node)) return { node, wired: false };

  if (typeof node.type === "string" && labelableFieldControls.has(node.type)) {
    return {
      node: cloneElement(node, {
        id: node.props.id ?? controlId,
        "aria-describedby": node.props["aria-describedby"] ?? descriptionId,
      }),
      wired: true,
    };
  }

  if (typeof node.type !== "string" && node.type !== Fragment) {
    return { node, wired: false };
  }

  let wired = false;
  const nestedChildren = Children.map(node.props.children, (child) => {
    if (wired) return child;
    const result = wireNestedFieldControl(child, controlId, descriptionId);
    wired = result.wired;
    return result.node;
  });
  return {
    node: wired ? cloneElement(node, { children: nestedChildren }) : node,
    wired,
  };
}

export function Field({
  label,
  hint,
  error,
  children,
  required,
}: PropsWithChildren<{
  label: string;
  hint?: string;
  error?: string;
  required?: boolean;
}>) {
  const generatedId = useId();
  const descriptionId = `${generatedId}-description`;
  const child = isValidElement<FieldControlProps>(children) ? children : null;
  const controlId = child?.props.id ?? generatedId;
  const describedBy = error || hint ? descriptionId : undefined;
  let control: ReactNode = children;

  if (child) {
    if (typeof child.type !== "string" && child.type !== Fragment) {
      control = cloneElement(child, {
        id: controlId,
        "aria-describedby": child.props["aria-describedby"] ?? describedBy,
      });
    } else {
      const wired = wireNestedFieldControl(child, controlId, describedBy);
      control = wired.node;
    }
  }

  return (
    <div className="flex min-w-0 flex-col gap-2">
      <label className="text-meta font-medium text-ink" htmlFor={controlId}>
        {label}
        {required ? <span aria-hidden="true"> *</span> : null}
      </label>
      {control}
      {error ? (
        <span
          className="text-xs leading-[1.45] text-tone-bad"
          id={descriptionId}
        >
          {error}
        </span>
      ) : hint ? (
        <span
          className="text-xs leading-[1.45] text-ink-faint"
          id={descriptionId}
        >
          {hint}
        </span>
      ) : null}
    </div>
  );
}

export function PlaceholderBadge({
  children = "Preview state",
}: PropsWithChildren) {
  return (
    <span
      data-slot="placeholder-badge"
      className="inline-flex min-h-[22px] w-max items-center rounded-md border border-line bg-surface-soft px-2 text-xs font-semibold whitespace-nowrap text-ink-soft"
    >
      {children}
    </span>
  );
}

/* ---------------------------------------------------------------------------
   Page chrome. These were the highest-traffic classes in the stylesheet -- 87
   `.eyebrow`, 45 `.page`, 35 `.form-grid`. As components the utilities are
   written once and every caller reads as markup instead of as a class soup.
   --------------------------------------------------------------------------- */

/**
 * Page column. `container-type: inline-size` is the important part: the docked
 * sidebar removes 240px from this column, so anything laying out *inside* a
 * page has to answer to the column's width, not the viewport's. Container
 * queries elsewhere in the app name this container.
 */
export function Page({
  narrow = false,
  children,
  className,
}: PropsWithChildren<{ narrow?: boolean; className?: string }>) {
  return (
    <div
      // A stable hook that is not a styling class: utilities churn as the design
      // changes, so tests and container queries anchor on this instead.
      data-slot="page"
      data-narrow={narrow ? "true" : undefined}
      className={cn(
        "@container/page mx-auto grid w-[min(1320px,100%)] content-start gap-6 px-8 pt-10 pb-12",
        "animate-[fade-rise_var(--motion-base)_var(--ease-standard)_both]",
        "[&>*]:my-0 [&>*]:min-w-0",
        "to-820:px-5 to-820:pt-8 to-700:px-4 to-700:pt-6 to-700:pb-10",
        narrow && "w-[min(920px,100%)]",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function PageStack({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <div
      className={cn(
        "grid content-start gap-6 [&>*]:my-0 [&>*]:min-w-0",
        className,
      )}
    >
      {children}
    </div>
  );
}

/** Small uppercase label above a heading, naming the section it belongs to. */
export function Eyebrow({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <div
      className={cn(
        "text-[11px] font-semibold tracking-[0.08em] break-words text-ink-faint uppercase",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function FormGrid<T extends "div" | "form" | "fieldset" = "div">({
  as,
  columns = 2,
  children,
  className,
  ...props
}: {
  as?: T;
  columns?: 1 | 2 | 3 | "auto";
} & Omit<ComponentProps<T extends undefined ? "div" : T>, "as">) {
  const Tag = (as ?? "div") as ElementType;
  return (
    <Tag
      {...props}
      className={cn(
        "grid gap-4 page-to-760:grid-cols-[minmax(0,1fr)]",
        columns === 1 && "grid-cols-[minmax(0,1fr)]",
        columns === 2 && "grid-cols-[repeat(2,minmax(0,1fr))]",
        // Three columns step down twice: the page column loses 240px to the
        // sidebar, so these are container queries, not viewport ones.
        columns === 3 &&
          "grid-cols-[repeat(3,minmax(0,1fr))] page-to-1000:grid-cols-[repeat(2,minmax(0,1fr))]",
        // Auto-fit: as many 260px columns as the container can hold.
        columns === "auto" &&
          "grid-cols-[repeat(auto-fit,minmax(min(100%,260px),1fr))]",
        className,
      )}
    >
      {children}
    </Tag>
  );
}

/** Validation message under a form; spans the whole grid so it is not squeezed
 *  into one column beside the field it belongs to. */
export function FormError({ children }: PropsWithChildren) {
  return (
    <p className="col-[1/-1] m-0 text-meta text-tone-bad" role="alert">
      {children}
    </p>
  );
}

export function IconButton({
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      type="button"
      {...props}
      className={cn(
        "grid size-8 place-items-center rounded-lg border border-line bg-surface text-ink-soft",
        "transition-[color,border-color,background-color] duration-(--motion-fast) ease-(--ease-standard)",
        "hover:not-disabled:border-line-strong hover:not-disabled:bg-surface-soft hover:not-disabled:text-ink",
        "active:not-disabled:translate-y-px",
        "focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-ring/25",
        "disabled:cursor-not-allowed disabled:opacity-50",
        "pointer-coarse:size-8 [&_svg]:size-3.5",
        className,
      )}
    />
  );
}

/**
 * Heading block at the top of a Card: title, supporting copy, and any actions
 * on the right. `bar` is for a flush card, where the header is a bordered strip
 * with its own padding rather than a block inside the card's padding.
 */
export function CardHeader({
  bar = false,
  children,
  className,
}: PropsWithChildren<{ bar?: boolean; className?: string }>) {
  return (
    <div
      className={cn(
        "flex flex-wrap items-center justify-between gap-x-4 gap-y-2",
        "[&>div:first-child]:min-w-0 [&>div:first-child]:flex-auto",
        "[&_h2]:m-0 [&_h2]:text-section [&_h2]:leading-[1.3] [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_h2]:text-ink",
        "[&_p]:mx-0 [&_p]:mt-1 [&_p]:mb-0 [&_p]:text-meta [&_p]:leading-[1.5] [&_p]:text-ink-soft",
        bar ? "border-b border-line px-6 py-5" : "mb-5",
        className,
      )}
    >
      {children}
    </div>
  );
}

/** The label of a field rendered outside <Field> -- a group of controls that
 *  shares one label, or a label that sits beside a non-input control. */
export function FieldLabel({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <span className={cn("text-meta font-medium text-ink", className)}>
      {children}
    </span>
  );
}

export function FieldHint({ children }: PropsWithChildren) {
  return (
    <p className="mx-0 mt-1 mb-0 text-xs leading-[1.45] text-ink-faint">
      {children}
    </p>
  );
}

/** A bordered form section with its own heading. Distinct from Card: a form
 *  card is a step in a flow, not a panel of data. */
export function FormCard({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <section
      className={cn(
        "mb-4 rounded-panel border border-line bg-surface px-6 py-6 shadow-panel",
        "[&_h2]:m-0 [&_h2]:text-section [&_h2]:leading-[1.3] [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_h2]:text-ink",
        "page-to-580:px-4 page-to-580:py-5",
        className,
      )}
    >
      {children}
    </section>
  );
}

/** Numbered heading for a step inside a FormCard: the badge, the title, and a
 *  line of supporting copy. */
export function FormCardHeading({
  step,
  children,
  className,
}: PropsWithChildren<{ step?: ReactNode; className?: string }>) {
  return (
    <div
      className={cn(
        "mb-5 grid grid-cols-[38px_1fr] items-start gap-3",
        "[&_p]:mx-0 [&_p]:mt-1 [&_p]:mb-0 [&_p]:text-xs [&_p]:leading-[1.5] [&_p]:text-ink-faint",
        className,
      )}
    >
      {step !== undefined ? (
        <span className="grid size-[34px] place-items-center rounded-lg border border-mint-line bg-mint-soft text-xs font-semibold text-mint-dark">
          {step}
        </span>
      ) : null}
      {/* A heading may carry a trailing action. The text block takes the free
          width so the action lands hard right instead of stacking under it. */}
      <div className="flex min-w-0 flex-wrap items-start justify-between gap-3 [&>div]:min-w-0 [&>div:first-child]:flex-1">
        {children}
      </div>
    </div>
  );
}

export function MutedCopy({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <p className={cn("text-xs leading-[1.5] text-ink-faint", className)}>
      {children}
    </p>
  );
}

/** Buttons at the end of a form, right-aligned. */
export function FormActions({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <div className={cn("mt-5 flex items-center justify-end gap-2", className)}>
      {children}
    </div>
  );
}

/** A row of buttons that belongs to the block above it rather than to a form. */
export function ButtonRow({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <div className={cn("mt-4 flex flex-wrap items-center gap-2", className)}>
      {children}
    </div>
  );
}

/** Definition list of read-only facts: label on the left, value on the right,
 *  one rule between rows. Values that are identifiers are allowed to ellipsize
 *  rather than wrap, because a wrapped digest is harder to read, not easier. */
export function DetailList({
  children,
  className,
}: PropsWithChildren<{ className?: string }>) {
  return (
    <dl
      data-slot="detail-list"
      className={cn(
        "m-0",
        "[&>div]:grid [&>div]:min-h-10 [&>div]:grid-cols-[116px_minmax(0,1fr)] [&>div]:items-center [&>div]:gap-3 [&>div]:border-t [&>div]:border-line [&>div]:px-0 [&>div]:py-2",
        "[&>div:first-child]:border-t-0",
        "[&_dt]:text-xs [&_dt]:text-ink-faint",
        "[&_dd]:m-0 [&_dd]:flex [&_dd]:min-w-0 [&_dd]:items-center [&_dd]:gap-1 [&_dd]:text-meta",
        "[&_dd>code]:overflow-hidden [&_dd>code]:text-ellipsis [&_dd>code]:whitespace-nowrap",
        "[&_dd>span]:overflow-hidden [&_dd>span]:text-ellipsis [&_dd>span]:whitespace-nowrap",
        "[&_code]:text-[inherit]",
        className,
      )}
    >
      {children}
    </dl>
  );
}

/** A titled section inside a config panel: an icon, a heading, supporting copy,
 *  and the fields that belong to it. Sections are separated by a rule, not by a
 *  card each, so a long form reads as one document. */
export function ConfigSection({
  icon,
  title,
  description,
  action,
  children,
  className,
}: PropsWithChildren<{
  icon?: IconName;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  className?: string;
}>) {
  return (
    <section
      className={cn(
        "border-b border-line px-0 py-6 last:border-b-0",
        "[&_h3]:m-0 [&_h3]:text-sm [&_h3]:font-semibold [&_h3]:tracking-[-0.01em] [&_h3]:text-ink",
        className,
      )}
    >
      <div
        className={cn(
          "mb-4 grid items-center gap-3",
          icon ? "grid-cols-[32px_minmax(0,1fr)]" : "grid-cols-[minmax(0,1fr)]",
          action &&
            (icon
              ? "grid-cols-[32px_minmax(0,1fr)_auto]"
              : "grid-cols-[minmax(0,1fr)_auto]"),
        )}
      >
        {icon ? (
          <span className="grid size-8 place-items-center rounded-[9px] bg-mint-soft text-mint-dark [&_svg]:w-[15px]">
            <Icon name={icon} />
          </span>
        ) : null}
        <div className="min-w-0">
          <h3>{title}</h3>
          {description ? (
            <p className="mx-0 mt-1 mb-0 text-xs text-ink-faint">
              {description}
            </p>
          ) : null}
        </div>
        {action}
      </div>
      {children}
    </section>
  );
}
