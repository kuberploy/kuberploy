import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { AuditPage } from "./AuditPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("audit page layout", () => {
  it("uses the shared responsive page gutter", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Platform admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "auditEvents").mockResolvedValue({ items: [] });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    const { container } = render(<AuditPage />, { wrapper: Wrapper });

    expect(await screen.findByText("No audit events")).toBeVisible();
    expect(container.firstElementChild).toHaveAttribute("data-slot", "page");
  });
});
