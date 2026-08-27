import { Button as ButtonPrimitive } from "@base-ui/react/button";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

// Sizes and colours here are DESIGN.md's, not stock shadcn's: 40px tall, 13px
// label, 8px radius, and the darker mint step for the filled variant because
// white on --mint is only 3.77:1. Every colour resolves through a token that
// swaps on [data-theme="dark"], so there are no `dark:` variants to keep in
// sync.
const buttonStyles = cva(
  [
    "inline-flex shrink-0 items-center justify-center gap-1.5",
    "rounded-lg border whitespace-nowrap select-none",
    "text-meta leading-none font-medium",
    "transition-[background-color,border-color,color,box-shadow] duration-(--motion-fast) ease-(--ease-standard)",
    "outline-none focus-visible:ring-3 focus-visible:ring-ring/40 focus-visible:border-ring",
    "active:translate-y-px",
    "disabled:pointer-events-none disabled:opacity-50",
    "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  ],
  {
    variants: {
      variant: {
        primary:
          "border-accent-surface bg-accent-surface text-accent-ink hover:bg-accent-surface-hover hover:border-accent-surface-hover",
        secondary:
          "border-line-strong bg-surface text-ink shadow-panel hover:bg-surface-soft",
        ghost:
          "border-transparent text-ink-soft hover:bg-surface-soft hover:text-ink",
        danger:
          "border-danger-surface bg-danger-surface text-danger-ink hover:opacity-90",
        link: "border-transparent text-mint-dark underline-offset-4 hover:underline",
      },
      size: {
        default: "min-h-10 px-4",
        sm: "min-h-8 px-2.5 text-xs",
        icon: "size-8 min-h-0 px-0",
      },
    },
    defaultVariants: { variant: "primary", size: "default" },
  },
);

// Call sites style links as buttons through `buttonVariants(...)` directly, so
// the merge has to happen here rather than in the component: without it a base
// utility and a variant's override both survive and CSS order picks the winner.
const buttonVariants = (
  props?: Parameters<typeof buttonStyles>[0] & { className?: string },
) => cn(buttonStyles(props));

function Button({
  className,
  variant,
  size,
  ...props
}: ButtonPrimitive.Props & VariantProps<typeof buttonVariants>) {
  return (
    <ButtonPrimitive
      data-slot="button"
      className={(state) =>
        buttonVariants({
          variant,
          size,
          className:
            typeof className === "function" ? className(state) : className,
        })
      }
      {...props}
    />
  );
}

export { Button, buttonVariants };
