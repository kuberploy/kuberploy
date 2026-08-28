"use client";

import { Select as SelectPrimitive } from "@base-ui/react/select";
import {
  Children,
  forwardRef,
  isValidElement,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type FocusEvent,
  type HTMLAttributes,
  type OptionHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
} from "react";

import { Icon } from "@/components/Icon";
import { cn } from "@/lib/utils";

type SelectOption = {
  disabled: boolean;
  label: ReactNode;
  textLabel: string;
  value: string;
};

type SelectItemValue = { key: string };

function textFromNode(node: ReactNode): string {
  return Children.toArray(node)
    .map((child) => {
      if (typeof child === "string" || typeof child === "number") {
        return String(child);
      }
      if (isValidElement<{ children?: ReactNode }>(child)) {
        return textFromNode(child.props.children);
      }
      return "";
    })
    .join("")
    .trim();
}

function optionsFromChildren(children: ReactNode): SelectOption[] {
  const options: SelectOption[] = [];

  const visit = (nodes: ReactNode) => {
    Children.forEach(nodes, (child) => {
      if (!isValidElement(child)) return;
      if (child.type === "option") {
        const props = child.props as OptionHTMLAttributes<HTMLOptionElement>;
        const textLabel = textFromNode(props.children);
        options.push({
          disabled: Boolean(props.disabled),
          label: props.children,
          textLabel,
          value: String(props.value ?? textLabel),
        });
        return;
      }
      const props = child.props as { children?: ReactNode };
      if (props.children) visit(props.children);
    });
  };

  visit(children);
  return options;
}

type SelectProps = Omit<
  SelectHTMLAttributes<HTMLSelectElement>,
  "multiple" | "size"
>;

/**
 * Shadcn-style, application-wide Select. It intentionally accepts the native
 * single-select API so existing forms retain their event and registration
 * contracts while the visible control is an accessible Base UI listbox.
 */
const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  {
    children,
    className,
    defaultValue,
    disabled,
    form,
    id,
    name,
    onBlur,
    onChange,
    required,
    value,
    ...props
  },
  forwardedRef,
) {
  const inputRef = useRef<HTMLInputElement>(null);
  const userInteractionRef = useRef(false);
  const options = useMemo(() => optionsFromChildren(children), [children]);
  const items = useMemo<Array<{ label: ReactNode; value: SelectItemValue }>>(
    () =>
      options.map((option) => ({
        label: option.label,
        value: { key: option.value },
      })),
    [options],
  );
  const initialValue = String(defaultValue ?? options[0]?.value ?? "");
  const currentValue = value === undefined ? undefined : String(value);
  const [uncontrolledValue, setUncontrolledValue] = useState(initialValue);
  const selectedValue = currentValue ?? uncontrolledValue;
  const selectedItem =
    items.find((item) => item.value.key === selectedValue)?.value ?? null;

  // React Hook Form registers this compatibility component as a select. Base
  // UI owns an equivalent hidden form input, so exposing that input preserves
  // registration, reset, validation, and submission without a native popup.
  useImperativeHandle(
    forwardedRef,
    () => inputRef.current as unknown as HTMLSelectElement,
    [],
  );

  const nativeEvent = (nextValue: string) => {
    const target = {
      name: name ?? "",
      type: "select-one",
      value: nextValue,
    } as unknown as HTMLSelectElement;
    return { currentTarget: target, target } as ChangeEvent<HTMLSelectElement>;
  };

  const blurEvent = () => {
    const target = {
      name: name ?? "",
      type: "select-one",
      value: selectedValue,
    } as unknown as HTMLSelectElement;
    return { currentTarget: target, target } as FocusEvent<HTMLSelectElement>;
  };

  const ariaProps = props as HTMLAttributes<HTMLButtonElement>;
  const passthroughProps = Object.fromEntries(
    Object.entries(props).filter(
      ([key]) => key.startsWith("aria-") || key.startsWith("data-"),
    ),
  ) as HTMLAttributes<HTMLButtonElement>;

  return (
    <>
      <SelectPrimitive.Root
        disabled={disabled}
        id={id}
        isItemEqualToValue={(item, selected) => item.key === selected.key}
        itemToStringLabel={(item) =>
          options.find((option) => option.value === item.key)?.textLabel ?? ""
        }
        itemToStringValue={(item) => item.key}
        items={items}
        onValueChange={(nextValue, details) => {
          // Base UI resolves its first nonempty item while reconciling a catalog
          // that contains an empty placeholder. That internal `none` transition
          // must not become a user choice or mutate a registered form field.
          if (details.reason === "none" || !userInteractionRef.current) return;
          const next = nextValue?.key ?? "";
          userInteractionRef.current = false;
          if (inputRef.current) inputRef.current.value = next;
          if (currentValue === undefined) setUncontrolledValue(next);
          onChange?.(nativeEvent(next));
        }}
        value={selectedItem}
      >
        <SelectPrimitive.Trigger
          {...passthroughProps}
          aria-describedby={ariaProps["aria-describedby"]}
          aria-invalid={ariaProps["aria-invalid"]}
          aria-label={ariaProps["aria-label"]}
          aria-labelledby={ariaProps["aria-labelledby"]}
          aria-required={required}
          className={cn(
            "flex min-h-11 w-full min-w-0 items-center justify-between gap-3 rounded-[9px] border border-line-strong bg-surface px-3 py-0 text-left text-sm text-ink outline-none",
            "transition-[border-color,box-shadow,background-color] duration-(--motion-fast) ease-(--ease-standard)",
            "hover:not-disabled:border-ink-faint focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/15",
            "disabled:cursor-not-allowed disabled:opacity-50",
            "[&_[data-slot='select-value']]:min-w-0 [&_[data-slot='select-value']]:truncate",
            className,
          )}
          onBlur={() => onBlur?.(blurEvent())}
          onKeyDownCapture={() => {
            userInteractionRef.current = true;
          }}
          onPointerDownCapture={() => {
            userInteractionRef.current = true;
          }}
          title={props.title}
          value={selectedValue}
        >
          <SelectPrimitive.Value data-slot="select-value" />
          <SelectPrimitive.Icon className="shrink-0 rotate-90 text-ink-faint transition-transform data-popup-open:-rotate-90 [&_svg]:size-4">
            <Icon name="chevron" />
          </SelectPrimitive.Icon>
        </SelectPrimitive.Trigger>
        <SelectPrimitive.Portal>
          <SelectPrimitive.Positioner
            align="start"
            alignItemWithTrigger={false}
            className="z-60 outline-none"
            disableAnchorTracking
            sideOffset={6}
          >
            <SelectPrimitive.Popup
              className={cn(
                "max-h-[min(320px,var(--available-height))] min-w-(--anchor-width) origin-(--transform-origin) overflow-y-auto rounded-[10px] border border-line-strong bg-surface p-1 shadow-xl outline-none",
                "data-starting-style:scale-95 data-starting-style:opacity-0 data-ending-style:scale-95 data-ending-style:opacity-0",
                "transition-[transform,opacity] duration-(--motion-fast) ease-(--ease-standard)",
              )}
            >
              {options.map((option, index) => (
                <SelectPrimitive.Item
                  key={`${option.value}-${index}`}
                  className={cn(
                    "relative flex min-h-10 cursor-default items-center rounded-md py-2 pr-9 pl-3 text-sm text-ink outline-none select-none",
                    "data-highlighted:bg-surface-soft data-highlighted:text-ink data-selected:font-medium",
                    "data-disabled:pointer-events-none data-disabled:opacity-45",
                  )}
                  disabled={option.disabled}
                  data-value={option.value}
                  label={option.textLabel}
                  value={items[index]?.value}
                >
                  <SelectPrimitive.ItemText>
                    {option.label}
                  </SelectPrimitive.ItemText>
                  <SelectPrimitive.ItemIndicator className="absolute right-3 grid size-4 place-items-center text-mint-dark [&_svg]:size-4">
                    <Icon name="check" />
                  </SelectPrimitive.ItemIndicator>
                </SelectPrimitive.Item>
              ))}
            </SelectPrimitive.Popup>
          </SelectPrimitive.Positioner>
        </SelectPrimitive.Portal>
      </SelectPrimitive.Root>
      <input
        ref={inputRef}
        autoComplete={props.autoComplete}
        disabled={disabled}
        form={form}
        name={name}
        readOnly
        type="hidden"
        value={selectedValue}
      />
    </>
  );
});

Select.displayName = "Select";

export { Select };
export type { SelectProps };
