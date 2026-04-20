# baseten-operator

A Helm chart for [baseten-operator](https://github.com/abridgeai/baseten-operator), a Kubernetes
operator that manages Baseten model deployments via the `BasetenModel` CRD.

## TL;DR

```sh
kubectl create namespace baseten-operator-system
kubectl -n baseten-operator-system create secret generic baseten-operator-api-key \
  --from-literal=api-key='<YOUR_BASETEN_API_KEY>'

helm install baseten-operator oci://ghcr.io/abridgeai/charts/baseten-operator \
  --namespace baseten-operator-system \
  --version 0.3.1
```

## Prerequisites

- Kubernetes ≥ 1.28
- A `Secret` containing your Baseten API key (see `api.secretName` / `api.secretKey`)

## Install

The chart is published as an OCI artifact in GitHub Container Registry:

```sh
helm install baseten-operator oci://ghcr.io/abridgeai/charts/baseten-operator \
  --namespace baseten-operator-system --create-namespace \
  --version 0.3.1
```

Use `helm show values oci://ghcr.io/abridgeai/charts/baseten-operator --version 0.3.1` to
inspect the defaults.

## Upgrading

The chart manages the `BasetenModel` CRD as a regular template, so `helm upgrade` will apply
CRD schema changes. Set `crds.install=false` if CRDs are managed out-of-band (e.g., by a separate
GitOps pipeline).

## Uninstalling

```sh
helm uninstall baseten-operator --namespace baseten-operator-system
```

> ⚠️ Because the CRD is part of the chart, `helm uninstall` removes the CRD and all `BasetenModel`
> resources along with it. Back up your CRs first, or install with `crds.install=false` to
> keep CRD lifecycle separate.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `replicaCount` | int | `1` | Controller replicas. Leader election ensures only one reconciles. |
| `image.repository` | string | `ghcr.io/abridgeai/baseten-operator` | Image repository. |
| `image.tag` | string | `""` | Image tag. Defaults to chart `appVersion`. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | list | `[]` | Image pull secrets. |
| `nameOverride` | string | `""` | Override the chart name (component of resource names). |
| `fullnameOverride` | string | `""` | Override the full release name (prefix of resource names). |
| `namespace.create` | bool | `false` | Render a `Namespace` manifest for `namespace.name`. Keep `false` when the namespace is managed externally. |
| `namespace.name` | string | `baseten-operator-system` | Namespace the operator runs in. |
| `crds.install` | bool | `true` | Install the `BasetenModel` CRD. Set to `false` if CRDs are managed separately. |
| `serviceAccount.create` | bool | `true` | Create the controller `ServiceAccount`. |
| `serviceAccount.name` | string | `""` | ServiceAccount name. Defaults to `<fullname>-controller-manager`. |
| `serviceAccount.annotations` | object | `{}` | ServiceAccount annotations (e.g., GKE Workload Identity binding). |
| `rbac.create` | bool | `true` | Create the controller `ClusterRoles`, `RoleBindings`, and `ClusterRoleBindings`. |
| `rbac.aggregatedRoles` | bool | `true` | Install the `basetenmodel-{admin,editor,viewer}` helper `ClusterRoles`. |
| `metrics.enabled` | bool | `true` | Expose the controller metrics service on HTTPS. |
| `metrics.port` | int | `8443` | Metrics service port. |
| `extraArgs` | list | `[]` | Extra args for the controller binary. |
| `api.secretName` | string | `baseten-operator-api-key` | Secret holding the Baseten API key. |
| `api.secretKey` | string | `api-key` | Key within the Secret. |
| `podAnnotations` | object | `{}` | Pod annotations (e.g., Datadog autodiscovery). |
| `podLabels` | object | `{}` | Extra pod labels. |
| `nodeSelector` | object | `{}` | Node selector. |
| `tolerations` | list | `[]` | Tolerations. |
| `affinity` | object | `{}` | Affinity rules. |
| `priorityClassName` | string | `""` | Priority class. |
| `terminationGracePeriodSeconds` | int | `10` | Grace period for Pod termination. |
| `resources` | object | `100m / 128Mi` requests, `1 / 512Mi` limits | Controller resources. |
| `podSecurityContext` | object | `runAsNonRoot: true`, `seccompProfile.type: RuntimeDefault` | Pod-level security context. |
| `securityContext` | object | drop ALL, read-only root FS | Container-level security context. |
| `livenessProbe` | object | `/healthz` on `:8081` | Liveness probe. |
| `readinessProbe` | object | `/readyz` on `:8081` | Readiness probe. |

## GitOps examples

### Argo CD

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: baseten-operator
  namespace: argocd
spec:
  project: default
  source:
    repoURL: ghcr.io/abridgeai/charts
    chart: baseten-operator
    targetRevision: 0.3.1
    helm:
      values: |
        serviceAccount:
          annotations:
            iam.gke.io/gcp-service-account: baseten-operator@my-project.iam.gserviceaccount.com
  destination:
    server: https://kubernetes.default.svc
    namespace: baseten-operator-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

### Config Sync (RootSync)

```yaml
apiVersion: configsync.gke.io/v1beta1
kind: RootSync
metadata:
  name: baseten-operator
  namespace: config-management-system
spec:
  sourceFormat: unstructured
  sourceType: helm
  helm:
    auth: none                     # or `gcenode` for GAR-hosted copies
    chart: baseten-operator
    includeCRDs: true              # no-op when CRDs are in templates/
    namespace: baseten-operator-system
    releaseName: baseten-operator  # keeps `baseten-operator-*` resource prefix
    repo: oci://ghcr.io/abridgeai/charts
    version: 0.3.1
    values:
      image:
        repository: my-registry/baseten-operator   # override to a mirror if desired
```

## License

Apache 2.0 — see [LICENSE](../../LICENSE).
