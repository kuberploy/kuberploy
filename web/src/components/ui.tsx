import type { ButtonHTMLAttributes, PropsWithChildren, ReactNode } from "react";
import { Icon, type IconName } from "./Icon";
import { errorMessage } from "../api/client";
import { operationTone, titleCase } from "../lib/format";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "./shadcn/dialog";

export function Button({
  variant = "primary",
  busy,
  children,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost" | "danger";
  busy?: boolean;
}) {
  return (
    <button
      {...props}
      className={`button button--${variant}`}
      disabled={busy || props.disabled}
    >
      {busy ? <span className="spinner" aria-hidden="true" /> : null}
      {children}
    </button>
  );
}

export function StatusPill({
  value,
  label,
}: {
  value?: string;
  label?: string;
}) {
  const tone = operationTone(value);
  return (
    <span className={`status-pill status-pill--${tone}`}>
      <span className="status-pill__dot" />
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
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <header className="page-header">
      <div>
        {eyebrow ? <div className="eyebrow">{eyebrow}</div> : null}
        <h1>{title}</h1>
        {description ? <p>{description}</p> : null}
      </div>
      {actions ? <div className="page-header__actions">{actions}</div> : null}
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
  icon?: Parameters<typeof Icon>[0]["name"];
  title: string;
  description: string;
  action?: ReactNode;
  compact?: boolean;
}) {
  return (
    <div className={`empty-state ${compact ? "empty-state--compact" : ""}`}>
      <span className="empty-state__icon">
        <Icon name={icon} />
      </span>
      <h3>{title}</h3>
      <p>{description}</p>
      {action}
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
    <div className="notice notice--error" role="alert">
      <div>
        <strong>{title}</strong>
        <p>{errorMessage(error)}</p>
      </div>
      {onRetry ? (
        <Button variant="secondary" onClick={onRetry}>
          <Icon name="refresh" /> Retry
        </Button>
      ) : null}
    </div>
  );
}

export function ConfirmDialog({
  title,
  description,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  icon = "settings",
  busy = false,
  onConfirm,
  onCancel,
}: {
  title: string;
  description: string;
  confirmLabel?: string;
  cancelLabel?: string;
  icon?: IconName;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onCancel();
      }}
    >
      <DialogContent
        className="confirmation-dialog max-w-none"
        role="alertdialog"
        showCloseButton={false}
      >
        <span className="confirmation-dialog__icon">
          <Icon name={icon} />
        </span>
        <span className="eyebrow">Confirm action</span>
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>{description}</DialogDescription>
        <div className="confirmation-dialog__actions">
          <Button
            variant="secondary"
            disabled={busy}
            onClick={onCancel}
            autoFocus
          >
            {cancelLabel}
          </Button>
          <Button
            variant="danger"
            busy={busy}
            onClick={onConfirm}
            disabled={busy}
          >
            {confirmLabel}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function Card({
  children,
  className = "",
}: PropsWithChildren<{ className?: string }>) {
  return <section className={`card ${className}`}>{children}</section>;
}

export function Skeleton({ lines = 3 }: { lines?: number }) {
  return (
    <div className="skeleton" aria-label="Loading">
      {Array.from({ length: lines }, (_, index) => (
        <span key={index} style={{ width: `${92 - index * 11}%` }} />
      ))}
    </div>
  );
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
  return (
    <label className="field">
      <span className="field__label">
        {label}
        {required ? <span aria-hidden="true"> *</span> : null}
      </span>
      {children}
      {error ? (
        <span className="field__error">{error}</span>
      ) : hint ? (
        <span className="field__hint">{hint}</span>
      ) : null}
    </label>
  );
}

export function PlaceholderBadge({
  children = "Preview state",
}: PropsWithChildren) {
  return <span className="placeholder-badge">{children}</span>;
}
