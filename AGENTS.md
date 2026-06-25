# AGENTS.md

Guidance for AI coding agents (and humans) working in this repo. `CLAUDE.md` is a symlink to this file.

## What this is

The baseten-operator is a Kubernetes operator (Kubebuilder) that manages Baseten model
deployments declaratively. It defines one CRD, `BasetenModel` (`models.baseten.com/v1alpha1`),
that promotes a Baseten deployment to an environment (dev/staging/production) and keeps its
autoscaling/promotion settings reconciled.

Two mutually exclusive ways to specify the deployment (CEL-validated, exactly one required):
- **`spec.sourceDeploymentName`** — promote a deployment CI/CD already created via `truss push`
  (naming: `img-{ver}-wgt-{ver}-p-{ver}`).
- **`spec.trussConfig`** — the operator creates the deployment itself: it hashes the config to a
  deterministic name `depl-{imageName}-{imageTag}-{hash}`, and if it doesn't exist, pushes it via
  the truss-go SDK in a background goroutine (non-blocking reconcile).

Both converge on environment reconciliation + promotion.

## Source of truth (read these, don't duplicate them)

- **CRD spec/status fields** → `api/v1alpha1/basetenmodel_types.go` (kubebuilder markers are the
  authoritative validation; CRD YAML is generated from here).
- **Reconciliation logic, status constants, conditions, orphan cleanup, retry** →
  `internal/controller/basetenmodel_controller.go`.
- **Baseten REST client + `ClientInterface` + utility helpers** → `internal/baseten/client.go`
  (mock: `mock_client.go`).
- **Truss config generation / hashing / push** → `internal/truss/config.go`, `pusher.go`.
- **Kubernetes events** → grep `r.Recorder.Event` in the controller for the current set.
- Original design docs are local-only under `docs/design/` (gitignored), not in the repo.

## Key behaviors worth knowing

- **Reconcile is environment-polling + prefix matching.** `GET environments/{name}` returns
  `current_deployment` and `candidate_deployment` and is the single source of truth. Baseten adds
  timestamp suffixes during promotion, so names are matched with `DeploymentNameMatchesPrefix`.
- **Tri-state health:** every status update sets both `Ready` and `Progressing` conditions
  (Ready=True → Argo Healthy; Ready=False+Progressing=True → Progressing; both False → Degraded).
- **Automatic retry:** retryable failures (FAILED, DEPLOY_FAILED, BUILD_FAILED) are retried via the
  Baseten retry API with exponential backoff (2m base, 30m cap, jitter) for up to 2h, then
  `TerminalError`. BUILD_STOPPED is intentional and not retried. Always-on, no spec config.
- **Orphan cleanup** is opt-in (`spec.orphanDeploymentCleanup`) and runs only in the steady-state
  happy path. All thresholds must be set explicitly — nil means skip.
- **`spec.paused: true`** stops all reconciliation/API calls and preserves last status.

## Conventions

- After editing `api/v1alpha1/basetenmodel_types.go`, run `make manifests generate` (also syncs the
  CRD into `charts/baseten-operator/files/`). CI fails if generated files are out of date.
- Status writes go through `updateStatus()`, which skips the API write when nothing meaningful
  changed (avoids self-triggered reconciles and extra Baseten API calls). Don't bypass it.
- The controller depends on `baseten.ClientInterface` and `truss.PusherInterface` for testability —
  use the interfaces, not concrete types.
- All Baseten client methods take `ctx` first and return `*APIError` on HTTP errors. CRD camelCase
  maps to API snake_case inside client methods.
- Keep comments minimal — only for non-obvious "why".
- PR titles must be Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`,
  `ci:`, `perf:`); commits need a DCO sign-off (`git commit -s`).

## Commands

```bash
make build                 # build the binary
make test                  # unit tests (excludes e2e)
make test-e2e              # e2e tests (spins up a Kind cluster)
make lint                  # golangci-lint (lint-fix to autofix)
make fmt vet               # format / go vet
make manifests generate    # regenerate CRDs + deepcopy after API changes

make run                   # run controller locally (needs kubeconfig + BASETEN_API_KEY)
make deploy IMG=<img>      # Helm-install CRDs + controller + RBAC into the cluster
make undeploy

go test ./internal/controller/... -v   # single package
```

Local Kind testing: after `make docker-build IMG=baseten-operator:dev`, you MUST
`kind load docker-image baseten-operator:dev --name <cluster>` before `make deploy` — Kind can't
pull local images.

## Layout

```
api/v1alpha1/            CRD type definitions (+kubebuilder markers)
internal/baseten/        Baseten REST client (+ mock)
internal/controller/     reconciliation logic (+ tests)
internal/truss/          truss config generation + push (+ mock pusher)
cmd/main.go              entrypoint
charts/baseten-operator/ Helm chart (user-facing install)
config/                  kubebuilder-generated CRD + RBAC source (drives the chart)
test/e2e/                Ginkgo e2e tests
```

Requirements: `BASETEN_API_KEY` env var; the model must already exist in Baseten.
