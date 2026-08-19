.PHONY: help fmt test web-build helm-lint check secret-scan prisma-migration-test platform-chart-test installer-chart-test builder-chart-test registry-chart-test monitoring-chart-test edge-chart-test argocd-chart-test postgresql-chart-test valkey-chart-test secret-controller-chart-test registry-cache-smoke registry-kubernetes-smoke kubernetes-harness-test kubernetes-preflight kubernetes-smoke kubernetes-cleanup

PNPM ?= npx --yes --package=pnpm@11.20.0 pnpm

help:
	@echo "Kuberploy development targets"
	@echo "  make fmt        Format backend and frontend sources"
	@echo "  make test       Run backend and frontend tests"
	@echo "  make web-build  Build the web UI"
	@echo "  make helm-lint  Lint and render Helm charts"
	@echo "  make check      Run all local verification"
	@echo "  make secret-scan Scan only tracked files for committed credentials"
	@echo "  make prisma-migration-test Test the real migration image against PostgreSQL"
	@echo "  make platform-chart-test  Test the control-plane and runtime charts"
	@echo "  make installer-chart-test Test the single-invocation Argo bootstrap installer"
	@echo "  make builder-chart-test   Test the isolated builder boundary chart"
	@echo "  make registry-chart-test  Test the managed registry chart"
	@echo "  make monitoring-chart-test  Test the independent managed monitoring chart"
	@echo "  make edge-chart-test      Test independent Traefik, cert-manager, and external-dns charts"
	@echo "  make argocd-chart-test    Test the managed/adopted Argo CD foundation chart"
	@echo "  make postgresql-chart-test Test the managed/adopted PostgreSQL chart"
	@echo "  make valkey-chart-test    Test the independent managed/adopted Valkey chart"
	@echo "  make secret-controller-chart-test Test External Secrets and Sealed Secrets charts"
	@echo "  make registry-cache-smoke Opt-in Docker registry/cache test"
	@echo "  make registry-kubernetes-smoke  Opt-in authenticated cluster test"
	@echo "  make kubernetes-harness-test  Test cluster-target safety locally"
	@echo "  make kubernetes-preflight     Read-only explicit-cluster preflight"
	@echo "  make kubernetes-smoke         Run and clean a scoped cluster smoke test"
	@echo "  make kubernetes-cleanup       Delete only the owned smoke namespace"

fmt:
	@if [ -f go.mod ]; then gofmt -w $$(find cmd internal migrations -name '*.go' -type f 2>/dev/null); fi
	@if [ -f web/package.json ]; then $(PNPM) --dir web format; fi

test:
	@if [ -f go.mod ]; then go test ./...; fi
	@if [ -f web/package.json ]; then $(PNPM) --dir web test --run; fi

web-build:
	@if [ -f web/package.json ]; then $(PNPM) --dir web build; fi

helm-lint: platform-chart-test installer-chart-test monitoring-chart-test edge-chart-test argocd-chart-test postgresql-chart-test valkey-chart-test secret-controller-chart-test
	@for chart in charts/kuberploy charts/kuberploy-runtime charts/kuberploy-registry charts/kuberploy-builder; do \
		if [ -f "$$chart/Chart.yaml" ]; then helm lint "$$chart"; helm template test "$$chart" >/dev/null; fi; \
	done

check: secret-scan test web-build helm-lint builder-chart-test registry-chart-test kubernetes-harness-test

secret-scan:
	@./scripts/security/scan-tracked-secrets.sh
	@./scripts/security/test-scan-tracked-secrets.sh

prisma-migration-test:
	@./test/e2e/test-prisma-migrations.sh

platform-chart-test:
	@./test/e2e/render-charts.sh

installer-chart-test:
	@./test/e2e/render-installer-chart.sh

builder-chart-test:
	@./test/e2e/render-builder-chart.sh

registry-chart-test:
	@./test/e2e/render-registry-chart.sh

monitoring-chart-test:
	@./test/e2e/render-monitoring-chart.sh

edge-chart-test:
	@./test/e2e/render-edge-chart.sh

argocd-chart-test:
	@./test/e2e/render-argocd-chart.sh

postgresql-chart-test:
	@./test/e2e/render-postgresql-chart.sh

valkey-chart-test:
	@./test/e2e/render-valkey-chart.sh

secret-controller-chart-test:
	@./test/e2e/render-secret-controller-charts.sh

registry-cache-smoke:
	@./scripts/docker/registry-cache-smoke.sh

registry-kubernetes-smoke:
	@./test/e2e/smoke-registry-chart.sh

kubernetes-harness-test:
	@./test/e2e/test-kubernetes-harness.sh
	@./test/e2e/test-public-provider-workflow.sh
	@./test/e2e/test-outbox-relay-job.sh
	@./test/e2e/test-kubernetes-qualification.sh

kubernetes-preflight:
	@./scripts/kubernetes/preflight.sh

kubernetes-smoke:
	@./scripts/kubernetes/smoke.sh

kubernetes-cleanup:
	@./scripts/kubernetes/cleanup-run.sh
