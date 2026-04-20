# Testing Baseten Operator with Kind

This guide walks through testing the operator locally using Kind with a real Baseten API.

## Prerequisites

```bash
# Install kind
brew install kind

# Install kubectl
brew install kubectl

# Verify installations
kind version
kubectl version --client
```

## Step 1: Create Kind Cluster

```bash
kind create cluster --name baseten-dev
kubectl cluster-info --context kind-baseten-dev
```

## Step 2: Build and Load Image

**CRITICAL**: Kind clusters cannot access local Docker images without explicitly loading them.

```bash
make docker-build IMG=baseten-operator:dev
kind load docker-image baseten-operator:dev --name baseten-dev
```

## Step 3: Deploy Operator

`make deploy` installs CRDs + controller + RBAC as a single Helm release:

```bash
export BASETEN_API_KEY=your-api-key
make deploy IMG=baseten-operator:dev

kubectl wait --for=condition=available --timeout=120s \
  -n baseten-operator-system deployment/baseten-operator-controller-manager
```

> `make install` / `make uninstall` are a separate path that installs only the CRD via
> `kubectl apply -f config/crd/bases/`. Don't mix `make install` with `make deploy` —
> Helm refuses to adopt CRDs it didn't create. Use one or the other.

## Step 4: Apply Test Resources

```bash
# Test sourceDeploymentName workflow
kubectl apply -f test/kind/01-test-model-dev.yaml

# Test trussConfig workflow
kubectl apply -f test/kind/truss-config-test-configmap.yaml
kubectl apply -f test/kind/truss-config-test-cr.yaml

# Watch reconciliation
kubectl get bm -w

# Check events
kubectl describe bm <resource-name>
```

## Step 5: Iterative Development

```bash
make docker-build IMG=baseten-operator:dev
kind load docker-image baseten-operator:dev --name baseten-dev
kubectl rollout restart deployment -n baseten-operator-system \
  baseten-operator-controller-manager
```

## Cleanup

```bash
kubectl delete bm --all
make undeploy
kind delete cluster --name baseten-dev
```
