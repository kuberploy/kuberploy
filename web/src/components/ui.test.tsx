import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ConfirmDialog } from "./ui";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("ConfirmDialog", () => {
  it("uses the accessible shadcn dialog boundary for keyboard dismissal", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    const onConfirm = vi.fn();

    render(
      <ConfirmDialog
        title="Delete deployment"
        description="This cannot be undone."
        onCancel={onCancel}
        onConfirm={onConfirm}
      />,
    );

    expect(screen.getByRole("alertdialog")).toHaveAccessibleName(
      "Delete deployment",
    );
    expect(screen.getByText("This cannot be undone.")).toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(onCancel).toHaveBeenCalledOnce();

    await user.click(screen.getByRole("button", { name: "Confirm" }));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("does not close while a confirmation is busy", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();

    render(
      <ConfirmDialog
        title="Delete deployment"
        description="Working."
        busy
        onCancel={onCancel}
        onConfirm={vi.fn()}
      />,
    );

    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).not.toHaveBeenCalled();
  });

  it("requires an exact typed confirmation when configured", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <ConfirmDialog
        title="Delete App"
        description="Only unused Apps can be deleted."
        confirmation="API"
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />,
    );
    const confirm = screen.getByRole("button", { name: "Confirm" });
    expect(confirm).toBeDisabled();
    await user.type(
      screen.getByRole("textbox", { name: "Confirm deletion" }),
      "API",
    );
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    expect(onConfirm).toHaveBeenCalledOnce();
  });
});
