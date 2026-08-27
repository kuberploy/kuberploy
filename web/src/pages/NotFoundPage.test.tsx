import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NotFoundPage, RouteErrorPage } from "./NotFoundPage";

const routerState = vi.fn();

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    className,
  }: PropsWithChildren<{ to: string; className?: string }>) => (
    <a href={to} className={className}>
      {children}
    </a>
  ),
  useRouterState: (options: {
    select: (state: { location: { pathname: string } }) => string;
  }) => options.select(routerState()),
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("NotFoundPage", () => {
  it("renders a safe recovery page for an unknown authenticated route", () => {
    routerState.mockReturnValue({ location: { pathname: "/missing/place" } });

    render(<NotFoundPage />);

    expect(
      screen.getByRole("heading", { name: /does not lead anywhere/i }),
    ).toBeInTheDocument();
    expect(screen.getByText("/missing/place")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: /open dashboard/i }),
    ).toHaveAttribute("href", "/");
    expect(
      screen.getByRole("link", { name: /view projects/i }),
    ).toHaveAttribute("href", "/projects");
    expect(screen.queryByText("NOT_FOUND")).not.toBeInTheDocument();
  });

  it("offers a working browser back action", async () => {
    routerState.mockReturnValue({ location: { pathname: "/missing/place" } });
    const back = vi
      .spyOn(window.history, "back")
      .mockImplementation(() => undefined);

    render(<NotFoundPage />);
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: /go back/i }));

    expect(back).toHaveBeenCalledOnce();
  });
});

describe("RouteErrorPage", () => {
  it("shows a safe recovery action without exposing raw failure details", async () => {
    const reset = vi.fn();
    render(
      <RouteErrorPage
        error={new Error("private backend diagnostic")}
        reset={reset}
      />,
    );

    expect(
      screen.getByRole("heading", { name: /could not finish loading/i }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/private backend diagnostic/i),
    ).not.toBeInTheDocument();
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: /try again/i }));
    expect(reset).toHaveBeenCalledOnce();
  });
});
