import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { ConfirmDialog, CopyButton, Select, useRowKeys } from "./ui";
import { openSelect, selectOption } from "../test/selectOption";

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

describe("CopyButton", () => {
  it("writes the value to the clipboard and confirms the copy", async () => {
    // userEvent.setup() installs its own clipboard stub, so the spy has to be
    // applied after it.
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.spyOn(navigator, "clipboard", "get").mockReturnValue({
      writeText,
    } as unknown as Clipboard);

    render(<CopyButton value="payments-api" label="Copy the name" />);
    const button = screen.getByRole("button", {
      name: "Copy the name: payments-api",
    });
    await user.click(button);

    expect(writeText).toHaveBeenCalledWith("payments-api");
    expect(await screen.findByText("Copied to clipboard")).toBeInTheDocument();
  });

  it("falls back to a selection copy when the async clipboard is missing", async () => {
    const user = userEvent.setup();
    vi.spyOn(navigator, "clipboard", "get").mockReturnValue(
      undefined as unknown as Clipboard,
    );
    const execCommand = vi.fn().mockReturnValue(true);
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: execCommand,
    });

    render(<CopyButton value="DISCONNECT" />);
    await user.click(screen.getByRole("button", { name: "Copy: DISCONNECT" }));

    expect(execCommand).toHaveBeenCalledWith("copy");
    expect(await screen.findByText("Copied to clipboard")).toBeInTheDocument();
  });
});

describe("Select", () => {
  it("does not emit a change when opening the current selection", async () => {
    const onChange = vi.fn();

    render(
      <Select
        aria-label="Project"
        defaultValue="project-a"
        onChange={onChange}
      >
        <option value="project-a">Payments</option>
        <option value="project-b">Storefront</option>
      </Select>,
    );

    await openSelect(screen.getByRole("combobox", { name: "Project" }));

    expect(onChange).not.toHaveBeenCalled();
  });

  it("uses a styled listbox and emits the native select change contract", async () => {
    const onChange = vi.fn();

    render(
      <Select
        aria-label="Repository"
        name="repositoryId"
        defaultValue="repo-a"
        onChange={onChange}
      >
        <option value="repo-a">kuberploy/kuberploy</option>
        <option value="repo-gitops">kuberploy/kuberploy-gitops-test</option>
      </Select>,
    );

    const trigger = screen.getByRole("combobox", { name: "Repository" });
    expect(trigger).toHaveTextContent("kuberploy/kuberploy");
    expect(document.querySelector("select")).toBeNull();

    await selectOption(trigger, "repo-gitops");

    expect(trigger).toHaveTextContent("kuberploy/kuberploy-gitops-test");
    expect(onChange).toHaveBeenCalledOnce();
    expect(onChange.mock.calls[0]?.[0].target).toMatchObject({
      name: "repositoryId",
      value: "repo-gitops",
    });
  });

  it("supports keyboard selection and skips disabled options", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <Select aria-label="Environment" defaultValue="dev" onChange={onChange}>
        <option value="dev">Development</option>
        <option value="blocked" disabled>
          Blocked
        </option>
        <option value="prod">Production</option>
      </Select>,
    );

    const trigger = screen.getByRole("combobox", { name: "Environment" });
    await openSelect(trigger);
    screen.getByRole("option", { name: "Development" }).focus();
    await user.keyboard("{End}{Enter}");

    expect(trigger).toHaveTextContent("Production");
    expect(onChange.mock.calls[0]?.[0].target.value).toBe("prod");
  });
});

describe("ConfirmDialog confirmation phrase", () => {
  it("offers the phrase as a copyable value and flags a mismatch", async () => {
    const user = userEvent.setup();
    render(
      <ConfirmDialog
        title="Delete App"
        description="Only unused Apps can be deleted."
        confirmation="payments-api"
        onCancel={vi.fn()}
        onConfirm={vi.fn()}
      />,
    );

    expect(
      screen.getByRole("button", {
        name: "Copy the confirmation phrase: payments-api",
      }),
    ).toBeInTheDocument();

    const input = screen.getByRole("textbox", { name: "Confirm deletion" });
    await user.type(input, "payments");
    expect(screen.getByText("Does not match yet.")).toBeInTheDocument();
    expect(input).toHaveAttribute("aria-invalid", "true");

    await user.type(input, "-api");
    expect(screen.queryByText("Does not match yet.")).not.toBeInTheDocument();
  });

  it("confirms on Enter once the phrase matches", async () => {
    const onConfirm = vi.fn();
    const user = userEvent.setup();
    render(
      <ConfirmDialog
        title="Disconnect source"
        description="This removes the source connection."
        confirmation="DISCONNECT"
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />,
    );

    const input = screen.getByRole("textbox", { name: "Confirm deletion" });
    await user.type(input, "DISCONNEC{Enter}");
    expect(onConfirm).not.toHaveBeenCalled();

    await user.type(input, "T{Enter}");
    expect(onConfirm).toHaveBeenCalledOnce();
  });
});

describe("useRowKeys", () => {
  function RowList() {
    const [rows, setRows] = useState(["alpha", "bravo", "charlie"]);
    const keys = useRowKeys(rows.length);
    return (
      <ul>
        {rows.map((row, index) => (
          <li key={keys.keyAt(index)}>
            <input aria-label={`${row} note`} defaultValue={row} />
            <button
              type="button"
              aria-label={`Remove ${row}`}
              onClick={() => {
                keys.removeAt(index);
                setRows(rows.filter((_, position) => position !== index));
              }}
            >
              Remove
            </button>
          </li>
        ))}
      </ul>
    );
  }

  it("keeps a row's own DOM node when an earlier row is removed", async () => {
    const user = userEvent.setup();
    render(<RowList />);

    const charlie = screen.getByRole("textbox", { name: "charlie note" });
    await user.clear(charlie);
    await user.type(charlie, "kept");
    await user.click(screen.getByRole("button", { name: "Remove alpha" }));

    // Keyed by index, charlie would have inherited bravo's node and shown
    // "bravo" here: the uncontrolled value belongs to the node, not the row.
    expect(screen.getByRole("textbox", { name: "charlie note" })).toBe(charlie);
    expect(charlie).toHaveValue("kept");
    expect(
      screen.queryByRole("textbox", { name: "alpha note" }),
    ).not.toBeInTheDocument();
  });
});
