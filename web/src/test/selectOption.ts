import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

/** Select a custom listbox item by its submitted value, as a user would. */
export async function selectOption(
  element: HTMLElement,
  value: string | HTMLElement,
) {
  const user = userEvent.setup();
  await waitFor(() => {
    if ((element as HTMLButtonElement).disabled) {
      throw new Error("Select is still disabled");
    }
  });
  if (element.getAttribute("aria-expanded") !== "true") {
    await user.click(element);
  }
  await waitFor(() => {
    if (element.getAttribute("aria-expanded") !== "true") {
      throw new Error("Select did not open");
    }
  });
  const requestedValue =
    typeof value === "string" ? value : value.getAttribute("data-value");
  let option: HTMLElement | undefined;
  await waitFor(() => {
    option = screen
      .getAllByRole("option")
      .find((item) => item.getAttribute("data-value") === requestedValue);
    if (!option)
      throw new Error(
        `Select option value ${JSON.stringify(requestedValue)} was not found`,
      );
  });
  if (!option) throw new Error("Select option disappeared before selection");
  await user.click(option);
}

/** Open a custom Select so its bounded catalog can be asserted. */
export async function openSelect(element: HTMLElement) {
  await waitFor(() => {
    if ((element as HTMLButtonElement).disabled) {
      throw new Error("Select is still disabled");
    }
  });
  if (element.getAttribute("aria-expanded") !== "true") {
    await userEvent.setup().click(element);
  }
  await waitFor(() => {
    if (element.getAttribute("aria-expanded") !== "true") {
      throw new Error("Select did not open");
    }
  });
}
