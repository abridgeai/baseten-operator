# Local Testing on Kind

Hands-on testing of the operator against the real Baseten API, in a kind cluster you control.

The fast path is `make kind-dev-up`. The manual steps below are equivalent and useful when you want to deviate.

## Prerequisites

- `kind` and `kubectl` installed (`brew install kind kubectl`)
- A Baseten API key: `export BASETEN_API_KEY=<your-key>` ([generate one](https://app.baseten.co/settings/api_keys))
- A Baseten model to test against. The sample CRs reference `my-llm-model` — edit them to match a model you control. **For destructive tests (`deletionPolicy: Delete`), use a throwaway model you're willing to recreate.**

## Quick start

```bash
export BASETEN_API_KEY=<your-key>
make kind-dev-up
```

`kind-dev-up` creates the cluster, builds and loads the operator image, deploys the operator pointed at `api.baseten.co`, and waits for the rollout. Re-running it on an existing cluster is a no-op for the cluster step and reapplies image + Helm release.

Apply a test resource:

```bash
# Edit modelName in the YAML to reference one of your Baseten models first.
kubectl apply -f test/kind/01-test-model-dev.yaml
kubectl get bm -w
kubectl describe bm example-model-dev
kubectl logs -n baseten-operator-system deployment/baseten-operator-controller-manager -f
```

Iterate on the operator after a code change:

```bash
make kind-dev-restart   # rebuild image, reload into kind, rollout restart
```

Tear it down:

```bash
make kind-dev-down
```

## Test resources

| File | Purpose |
| --- | --- |
| `01-test-model-dev.yaml` | Promote `sourceDeploymentName` to a `dev` environment with autoscaling |
| `02-test-model-staging.yaml` | Same model promoted to `staging` |
| `03-test-model-prod.yaml` | Same model promoted to `prod` |
| `04-test-promotion-settings.yaml` | Exercises `promotionSettings` (rollingDeploy, ramp-up) |
| `05-test-delete-retain.yaml` | `deletionPolicy: Retain` — `kubectl delete bm` leaves the Baseten model alone |
| `06-test-delete-cascade.yaml` | `deletionPolicy: Delete` — `kubectl delete bm` cascades to `DELETE /v1/models/{id}` and removes the model |
| `truss-config-test-configmap.yaml` + `truss-config-test-cr.yaml` | Operator-created deployment via inline `trussConfig` |

## Walkthrough: testing the delete feature

### Retain (default, safe)

```bash
# Edit modelName in 05-test-delete-retain.yaml to reference one of your existing models.
kubectl apply -f test/kind/05-test-delete-retain.yaml

# Wait until status.modelID is populated (operator has resolved the model)
kubectl get bm example-delete-retain -o jsonpath='{.status.modelID}'

# Inspect the finalizer the operator added
kubectl get bm example-delete-retain -o jsonpath='{.metadata.finalizers}'
# expect: ["models.baseten.com/finalizer"]

# Delete the CR
kubectl delete bm example-delete-retain

# CR vanishes within seconds; confirm in Baseten that the model is still there.
```

### Delete (cascade — irreversible)

> Use a throwaway model. This deletes everything in Baseten under that model: deployments, environments, promotion history.

```bash
# Edit modelName in 06-test-delete-cascade.yaml to a throwaway model.
kubectl apply -f test/kind/06-test-delete-cascade.yaml

# Wait for status.modelID
kubectl get bm example-delete-cascade -o jsonpath='{.status.modelID}'

kubectl delete bm example-delete-cascade

# Watch the operator log the cascade and emit ModelDeleted
kubectl logs -n baseten-operator-system deployment/baseten-operator-controller-manager -f
kubectl get events --field-selector reason=ModelDeleted

# CR vanishes once the API call succeeds. Confirm the model is gone in Baseten.
```

### Stuck in Terminating

If `DeleteModel` fails, the CR sits in `Terminating` with `Status.deploymentStatus=DELETE_FAILED` and a `ModelDeleteFailed` event. The operator retries forever (30s backoff). Manual escape:

```bash
kubectl patch bm <name> -p '{"metadata":{"finalizers":[]}}' --type=merge
```

## Manual setup (bypassing `make kind-dev-up`)

```bash
kind create cluster --name baseten-operator-dev
make docker-build IMG=baseten-operator:dev
kind load docker-image baseten-operator:dev --name baseten-operator-dev

kubectl create ns baseten-operator-system
kubectl create secret generic baseten-operator-api-key \
  -n baseten-operator-system \
  --from-literal=api-key="$BASETEN_API_KEY"

make deploy IMG=baseten-operator:dev
kubectl rollout status deployment/baseten-operator-controller-manager \
  -n baseten-operator-system --timeout=120s
```

`make install` / `make uninstall` apply CRDs only (via `kubectl apply -f config/crd/bases/`). Don't combine with `make deploy` — Helm refuses to adopt CRDs it didn't create. Use one path or the other.
