import {
  Link,
  useRouterState,
  type ErrorComponentProps,
} from "@tanstack/react-router";
import { Icon } from "../components/Icon";
import { Button, Eyebrow, Page, buttonVariants } from "../components/ui";

export function NotFoundPage() {
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  });

  return (
    <Page narrow className="min-h-[calc(100vh-68px)] place-content-center">
      <section className="relative overflow-hidden rounded-[20px] border border-line bg-surface px-8 py-12 shadow-[0_24px_80px_rgba(8,20,15,0.08)] to-580:px-5 to-580:py-9">
        <div
          className="pointer-events-none absolute -top-28 -right-24 size-72 rounded-full bg-mint-soft blur-2xl"
          aria-hidden="true"
        />
        <div className="relative grid gap-8 md:grid-cols-[minmax(0,1fr)_170px] md:items-center">
          <div className="min-w-0">
            <Eyebrow>Page not found</Eyebrow>
            <h1 className="mt-3 max-w-[620px] text-[clamp(32px,5vw,54px)] leading-[1.02] font-semibold tracking-[-0.035em] text-ink">
              This route does not lead anywhere.
            </h1>
            <p className="mt-5 max-w-[610px] text-base leading-7 text-ink-soft">
              The page may have moved during an upgrade, or the address may be
              incomplete. Your cluster and Apps have not been changed.
            </p>
            <code className="mt-5 block max-w-full overflow-hidden rounded-lg border border-line bg-surface-soft px-3 py-2 text-xs text-ellipsis whitespace-nowrap text-ink-faint">
              {pathname}
            </code>
            <div className="mt-8 flex flex-wrap gap-3">
              <Link to="/" className={buttonVariants({ variant: "primary" })}>
                Open dashboard <Icon name="arrow" />
              </Link>
              <Link
                to="/projects"
                className={buttonVariants({ variant: "secondary" })}
              >
                View projects
              </Link>
              <Button variant="ghost" onClick={() => window.history.back()}>
                Go back
              </Button>
            </div>
          </div>
          <div
            className="relative mx-auto grid size-40 place-items-center rounded-[32px] border border-mint-line bg-mint-soft text-mint-dark shadow-[inset_0_0_0_12px_rgba(67,215,160,0.05)] [&_svg]:size-16"
            aria-hidden="true"
          >
            <Icon name="route" />
            <span className="absolute -right-2 -bottom-2 grid size-14 place-items-center rounded-2xl border border-line bg-surface text-lg font-bold text-ink shadow-lg">
              404
            </span>
          </div>
        </div>
      </section>
    </Page>
  );
}

export function RouteErrorPage({ reset }: ErrorComponentProps) {
  return (
    <Page narrow className="min-h-[calc(100vh-68px)] place-content-center">
      <section className="relative overflow-hidden rounded-[20px] border border-line bg-surface px-8 py-12 shadow-[0_24px_80px_rgba(8,20,15,0.08)] to-580:px-5 to-580:py-9">
        <div
          className="pointer-events-none absolute -top-28 -right-24 size-72 rounded-full bg-red-soft blur-2xl"
          aria-hidden="true"
        />
        <div className="relative grid gap-8 md:grid-cols-[minmax(0,1fr)_170px] md:items-center">
          <div className="min-w-0">
            <Eyebrow>Page unavailable</Eyebrow>
            <h1 className="mt-3 max-w-[620px] text-[clamp(32px,5vw,54px)] leading-[1.02] font-semibold tracking-[-0.035em] text-ink">
              This page could not finish loading.
            </h1>
            <p className="mt-5 max-w-[610px] text-base leading-7 text-ink-soft">
              Kuberploy kept the current cluster state unchanged. Retry the
              page, or return to a known workspace while the problem is
              investigated.
            </p>
            <div className="mt-8 flex flex-wrap gap-3">
              <Button onClick={reset}>
                Try again <Icon name="refresh" />
              </Button>
              <Link to="/" className={buttonVariants({ variant: "secondary" })}>
                Open dashboard
              </Link>
              <Link
                to="/projects"
                className={buttonVariants({ variant: "ghost" })}
              >
                View projects
              </Link>
            </div>
          </div>
          <div
            className="relative mx-auto grid size-40 place-items-center rounded-[32px] border border-red-line bg-red-soft text-red shadow-[inset_0_0_0_12px_rgba(210,64,64,0.04)] [&_svg]:size-16"
            aria-hidden="true"
          >
            <Icon name="refresh" />
            <span className="absolute -right-2 -bottom-2 grid size-14 place-items-center rounded-2xl border border-line bg-surface text-sm font-bold text-ink shadow-lg">
              Retry
            </span>
          </div>
        </div>
      </section>
    </Page>
  );
}
