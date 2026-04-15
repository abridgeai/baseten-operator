# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Important Documentation

**Original Design Document**: See `docs/design.md` for the comprehensive design document that outlines the original design vision. Note that the current implementation has evolved from that design.

**TrussConfig Support**: The operator now supports creating deployments directly via inline truss configuration. See `docs/truss-config-design.md` for the original design document. Implementation is complete as of v1alpha1.

## Project Overview

The baseten-operator is a Kubernetes operator built with Kubebuilder that manages Baseten model deployments. It provides a declarative way to promote existing Baseten deployments to environments using Kubernetes Custom Resources.

**Key functionality:**
- Defines a `BasetenModel` CRD for declarative environment promotion
- Reconciles desired state by promoting existing deployments to environments
- Creates deployments directly via inline truss configuration (`spec.trussConfig`)
- Manages environments (development, staging, production)
- Polls deployment status until active
- Automatically retries failed deployments (FAILED, DEPLOY_FAILED, BUILD_FAILED) for up to 2 hours with exponential backoff

## Architecture

### Custom Resource Definition (CRD)

The operator defines a single CRD: `BasetenModel` (API group: `models.baseten.com/v1alpha1`)

**Spec fields:**
- `modelName` - Name of the model in Baseten
- `sourceDeploymentName` - Name of the deployment to promote (created by CI/CD via `truss push`). Mutually exclusive with `trussConfig`. Format: `img-{version}-wgt-{version}-p-{version}`
- `trussConfig` - Inline truss configuration for the operator to create a deployment via truss push. Mutually exclusive with `sourceDeploymentName`. Contains:
  - `pythonVersion` - Python version (e.g., `py311`, `py312`) (optional)
  - `resources` - Compute resources (required):
    - `accelerator` - GPU type and count (e.g., `H100:2`, `A100:4`, `L4`) (required)
    - `useGpu` - Enable GPU support (bool pointer, optional)
  - `baseImage` - Docker base image (required):
    - `image` - Docker image URI (required)
    - `dockerAuth` - Registry authentication (optional):
      - `authMethod` - Auth method (e.g., `GCP_SERVICE_ACCOUNT_JSON`) (required)
      - `secretName` - Baseten secret name containing credentials (required)
      - `registry` - Docker registry hostname (e.g., `us-docker.pkg.dev`) (required)
  - `dockerServer` - Custom server configuration (optional):
    - `noBuild` - Skip image build step when base image already contains everything needed (bool pointer, optional)
    - `startCommand` - Server start command
    - `readinessEndpoint` - HTTP readiness endpoint path
    - `livenessEndpoint` - HTTP liveness endpoint path
    - `predictEndpoint` - HTTP prediction endpoint path
    - `serverPort` - Server port (note: 8080 is reserved by Baseten)
  - `runtime` - Runtime behavior (optional):
    - `predictConcurrency` - Maximum concurrent prediction requests (int32 pointer)
  - `modelMetadata` - Model metadata (optional):
    - `tags` - Labels for the model (e.g., `["openai-compatible"]`)
  - `secrets` - Secret keys available to deployment; values should be empty strings (optional)
  - `environmentVariables` - Non-secret environment variables (optional)
  - `setupScript` - Setup/startup script (optional):
    - `configMapRef` - Reference to ConfigMap containing the script:
      - `name` - ConfigMap name
      - `key` - Key in ConfigMap's data
    - `inline` - Script content directly (for small scripts only)
- `environment` - Environment configuration including:
  - `name` - Environment name (lowercase alphanumeric only, e.g., dev, staging, production)
  - `autoscaling` - Autoscaling configuration (optional):
    - `minReplicas` - Minimum number of replicas (int32 pointer)
    - `maxReplicas` - Maximum number of replicas (int32 pointer)
    - `concurrencyTarget` - Target concurrency per replica (int32 pointer)
    - `autoscalingWindow` - Autoscaling window in seconds (int32 pointer)
    - `scaleDownDelay` - Scale down delay in seconds (int32 pointer)
    - `targetUtilizationPercentage` - Target utilization percentage (int32 pointer)
  - `promotionSettings` - Promotion behavior configuration (optional)
- `orphanDeploymentCleanup` - Automatic cleanup of orphan deployments (optional):
  - `scaleToZero` - Set min_replica=0 on orphan deployments (bool pointer)
  - `delete` - Delete orphan deployments older than deleteAfterDays (bool pointer)
  - `deleteAfterDays` - Minimum age in days before deletion (int32 pointer, required when delete is true)
  - `minToKeep` - Minimum number of orphan deployments to preserve (int32 pointer, required when delete is true)
  - `intervalMinutes` - How often cleanup runs in minutes (int32 pointer, required — cleanup skipped if nil)
- `paused` - Stops the controller from reconciling this resource (bool, optional, default false). When true, no API calls are made and the last known status is preserved. Useful during emergencies, incident response, or when setting up a new model via click-ops in the Baseten UI.

**Status fields:**
- `modelID` - Baseten model ID (populated after reconciliation)
- `sourceDeploymentID` - Resolved Baseten deployment ID for the source deployment (cached after lookup)
- `sourceDeploymentName` - Source deployment name that was resolved (confirms what controller resolved)
- `activeDeploymentName` - Name of the currently live deployment in the environment (from `current_deployment.name`)
- `candidateDeploymentID` - ID of the deployment being promoted (double-promote guard, from `Promote()` response)
- `candidateDeploymentName` - Name of the deployment being promoted (from `candidate_deployment.name`)
- `deploymentStatus` - Most relevant deployment status (uses controller-level constants like `PROMOTING`, `PROMOTING_DEPLOYMENT`, `PENDING`, or Baseten API statuses like `ACTIVE`, `BUILDING`, `FAILED`)
- `activeReplicaCount` - Number of active replicas
- `message` - Additional status information
- `trussConfigHash` - SHA256 hash (8 hex chars) of trussConfig + setup script content, used for change detection; a new hash triggers a new truss push
- `pushStatus` - Internal tracking of the truss push operation: `TRUSS_PUSHING` (async push launched), `TRUSS_PUSH_DONE` (deployment created in Baseten and found on next reconcile). Used internally by the controller to detect in-flight pushes across reconcile cycles. Note: the user-facing `deploymentStatus` uses `DEPLOYING` (not `TRUSS_PUSHING`) while a push is in progress.
- `trussPushTime` - Timestamp of the last truss push launch. Used with `trussPushStaleTimeout` (5 minutes) to detect goroutines that died before writing back their result. When elapsed > timeout: clears push state and emits `TrussPushFailed` warning, then retries push on next reconcile.
- `lastCleanupTime` - Timestamp of last orphan deployment cleanup execution (metav1.Time pointer)
- `firstDeploymentFailureTime` - Timestamp when the current deployment first entered a retryable failure state (FAILED, DEPLOY_FAILED, BUILD_FAILED). Used to enforce the 2-hour retry deadline. Automatically cleared when the deployment becomes ACTIVE or SCALED_TO_ZERO.
- `deploymentRetryCount` - Number of retry attempts for the current failure. Used to compute exponential backoff intervals. Cleared on ACTIVE or SCALED_TO_ZERO.
- `nextRetryTime` - Earliest time the next retry attempt is allowed. Computed as exponential backoff (2m base, doubling, capped at 30m) with 0-50% jitter. Cleared on ACTIVE or SCALED_TO_ZERO.
- `conditions` - Standard Kubernetes conditions with two types:
  - **`Ready`** - True when deployment is ACTIVE or SCALED_TO_ZERO (deployment is serving traffic)
  - **`Progressing`** - True when work is in progress, False for terminal/stable states (enables Argo CD tri-state health)

### CI/CD Integration

The operator supports **two mutually exclusive workflows** for specifying the deployment to promote:

**Option A: CI/CD-created deployment (`sourceDeploymentName`)**
1. GitHub Actions runs `truss push` when creating a PR
2. This creates a new deployment with naming convention: `img-{container-img-version}-wgt-{model-weight-version}-p-{platform-config-version}`
3. The operator promotes this existing deployment to the target environment

**Option B: Operator-created deployment (`trussConfig`)**
1. User specifies inline truss configuration in the CRD (`spec.trussConfig`)
2. The operator computes a deterministic hash of the config → deployment name `depl-{imageName}-{imageTag}-{hash}` (e.g., `depl-vllm-0.11.2.1-9a1d1cbc`)
3. The operator checks if the deployment already exists (idempotent); if not, generates `config.yaml` and launches an **async goroutine** to push via the truss-go SDK (reconcile loop is not blocked)
4. On the next reconcile (after 10s), the controller re-checks `FindDeploymentIDByName`; once the deployment exists, the existing promotion flow takes over

Both approaches converge at Step 2 (environment reconciliation). The `sourceDeploymentName` approach remains fully supported for backward compatibility.

### Controller Reconciliation Flow

The `BasetenModelReconciler` in `internal/controller/basetenmodel_controller.go` uses **environment-based polling** with **prefix matching** on deployment names. The environment API (`GET /models/{id}/environments/{name}`) returns both `current_deployment` and `candidate_deployment`, serving as the single source of truth.

**Step 1: Model Lookup**
- Use cached `status.ModelID` if available (persisted in API server across reconciles)
- Otherwise resolve model ID from model name using `FindModelIDByName()` (lists all models, filters by name)
- Fail if model doesn't exist

**Step 1.5: Resolve Source Deployment (`resolveSourceDeployment()`)**
- If `spec.trussConfig` is set → call `reconcileTrussDeployment()`:
  - Read setup script from ConfigMap if `setupScript.configMapRef` is set (or use `inline` value)
  - On ConfigMap error: emit `SetupScriptNotFound` warning event, set `deploymentStatus=FAILED`, message `"{activePrefix}truss push failed: {err}"`, stop reconciliation
  - Compute deterministic hash: `HashTrussConfig(tc, setupScript)` → 8 hex chars
  - Generate deployment name: `depl-{imageName}-{imageTag}-{hash}` (e.g., `depl-vllm-0.11.2.1-9a1d1cbc`), derived from `parseImage(baseImage.image)` + hash
  - Check if deployment already exists via `FindDeploymentIDByName()` (idempotent — handles re-runs and shared configs)
  - If exists: update `status.trussConfigHash`, `status.sourceDeploymentName`, `status.pushStatus=TRUSS_PUSH_DONE`; return deployment name
  - If `pushStatus=TRUSS_PUSHING` and deployment name matches: push already in flight, but first check for stale state:
    - If `trussPushTime` age > `trussPushStaleTimeout` (5 min): goroutine likely died — emit `TrussPushFailed` warning, clear `pushStatus`/`trussPushTime`, fall through to launch new push
    - Otherwise: set `deploymentStatus=DEPLOYING`, message `"{activePrefix}truss push in progress for X"`, requeue 10s
  - If not exists (and not already pushing): generate `config.yaml` via `GenerateConfigYAML()`:
    - On generation error: emit `TrussPushFailed` warning event, set `deploymentStatus=FAILED`, message `"{activePrefix}truss push failed for X: {err}"`, stop reconciliation
    - On success: set `pushStatus=TRUSS_PUSHING`, set `trussPushTime=now` in status, emit `TrussPushStarted` event, launch **async goroutine** (`asyncPush`) with a 5-minute context — reconcile loop returns immediately; set `deploymentStatus=DEPLOYING`, message `"{activePrefix}truss push started for X"`, requeue 10s
  - Async goroutine (`asyncPush`): calls `PushFromConfig()`, then writes result back to CR status via `updatePushStatus()`:
    - On success: sets `pushStatus=TRUSS_PUSH_DONE`, clears `trussPushTime` — next reconcile finds the deployment and proceeds
    - On failure: clears `pushStatus` (empty string) and clears `trussPushTime` — next reconcile retries the push automatically
- If `spec.sourceDeploymentName` is set → return it directly (no API call)

**Step 2: Environment Reconciliation**
- `reconcileEnvironment()` returns the `*baseten.Environment` on success
- Get or create environment using `GetEnvironment()` / `CreateEnvironment()`
- Check for autoscaling drift AND promotion settings drift between spec and actual settings
- If drift detected: call `UpdateEnvironmentSettings()` with only the configs that have drift (nil for no-drift configs), sending both in a single PATCH call
- Emit `AutoscalingUpdated` event if autoscaling drifted, `PromotionSettingsUpdated` event if promotion settings drifted
- On update failure: emit `AutoscalingUpdateFailed` if autoscaling drifted, `PromotionSettingsUpdateFailed` if promotion settings drifted (both emitted when both drift)
- Requeue for 10s after create/update to verify

**Step 3: Check Current Deployment (prefix match) - HAPPY PATH**
- If `env.CurrentDeployment.Name` matches the resolved source deployment name by prefix → deployment is live
- Set `status.activeDeploymentName`, clear candidate fields, report status
- Set `deploymentStatus` to ACTIVE or SCALED_TO_ZERO
- Set Ready=True, Progressing=False (stable state)
- **Orphan Cleanup (if enabled):** Run `reconcileOrphanCleanup()` only in this happy path steady state
- Requeue 5 min for periodic verification

**Step 4: Check Candidate Deployment (prefix match)**
- If `env.CandidateDeployment.Name` matches by prefix → promotion in progress
- Report candidate's status, update `status.candidateDeploymentName`
- Set `deploymentStatus` to `PROMOTING` (controller constant, not from API)
- Set Ready=False, Progressing=True (work in progress)
- If terminal failure (FAILED, DEPLOY_FAILED, BUILD_FAILED, BUILD_STOPPED) → call `handleCandidateFailure()`:
  - If **retryable** (FAILED, DEPLOY_FAILED, BUILD_FAILED): call `tryRetryDeployment()`:
    - Record `status.firstDeploymentFailureTime` on first detection
    - Check backoff: if `status.nextRetryTime` hasn't elapsed, requeue until that time (skips retry call)
    - Concurrency guard: re-check deployment status via `FindDeploymentIDByName`; skip if no longer in retryable state (another CR may have retried it)
    - If within 2h of first failure: call `RetryDeployment()` API, emit `DeploymentRetried` event, call `scheduleNextRetry()` (increments `deploymentRetryCount`, sets `nextRetryTime` with exponential backoff), set `deploymentStatus=BUILDING`, requeue 30s
    - If `RetryDeployment()` call fails: emit `DeploymentRetryFailed` warning, call `scheduleNextRetry()`, requeue 30s
    - If retry API declines (retried=false): emit `DeploymentRetryFailed` warning, call `scheduleNextRetry()`, requeue 30s
    - If 2h deadline exceeded: emit `DeploymentRetryExhausted` warning, return `TerminalError` (stops requeueing)
  - If **non-retryable** (BUILD_STOPPED): emit `PromotionFailed` warning, report error (not a TerminalError — requeues on next spec change)
- Requeue 30s

**Step 5: Promote**
- Neither current nor candidate matches → need to promote
- **Double-promote guard**: if `status.candidateDeploymentID` is set OR env has a candidate → don't re-promote (sets `deploymentStatus: PROMOTING`, message `"{activePrefix}waiting for existing promotion to complete..."` or `"{promotionMessage} — waiting for completion before promoting {sourceDeploymentName}"`)
- Clear stale `candidateDeploymentID` if source deployment name changed
- Look up source deployment ID via `FindDeploymentIDByName()`, validate status:
  - Not found: emit `SourceDeploymentNotFound` warning, `deploymentStatus=FAILED`, message `"{activePrefix}unable to promote: source deployment {name} not found"`
  - FAILED, DEPLOY_FAILED, BUILD_FAILED (retryable): call `tryRetryDeployment()` — same 2h retry logic with exponential backoff as Step 4; emits `DeploymentRetried`, `DeploymentRetryFailed`, or `DeploymentRetryExhausted`; on retry: `deploymentStatus=BUILDING`; on exhaustion: emit `SourceDeploymentFailed` warning, return `TerminalError`
  - BUILD_STOPPED and other unrecognized statuses: fall into default handler — `deploymentStatus={sourceStatus}`, message `"{activePrefix}waiting to promote: source deployment {name} has status {STATUS}"`, requeue 30s
  - Building/Deploying/other known progressing: `deploymentStatus={sourceStatus}`, message `"{activePrefix}waiting to promote: source deployment {name} is {STATUS}"`
- Set `deploymentStatus: PROMOTING_DEPLOYMENT`, message `"{activePrefix}promoting {source} to {env}"` before API call
- Call `Promote()` → store returned deployment ID as `status.candidateDeploymentID`
- Set `deploymentStatus: PROMOTING`, message `"{activePrefix}promoted {source} to {env} ({candidateStatus})"` after promotion
- On `Promote()` error: `deploymentStatus=FAILED`, message `"{activePrefix}promotion of '{source}' to {env} failed: {err}"`
- Set Ready=False, Progressing=True
- Emit `DeploymentPromoted` event, requeue 30s

**Prefix Matching:**
- Baseten adds timestamp suffixes during promotion (e.g., `img-1.0-wgt-1.0-p-1.2.1768269232`)
- `baseten.DeploymentNameMatchesPrefix(name, prefix)` handles exact match and prefix+dot match

**Orphan Deployment Cleanup:**
Cleanup runs only in Step 3 (happy path when deployment is live and stable):
1. **Check interval**: Skip entirely if `intervalMinutes` is nil. Skip if `lastCleanupTime + intervalMinutes` hasn't elapsed.
2. **Find orphans**: Deployments that are ALL of:
   - NOT `current_deployment` of any environment
   - NOT `candidate_deployment` of any environment
   - NOT prefix-matching `spec.sourceDeploymentName`
   - NOT `is_production` or `is_development` flagged
3. **Scale to zero** (if `scaleToZero: true`): Set `min_replica=0` on orphans where `min_replica > 0`. Skips deployments already at zero.
4. **Delete stale orphans** (if `delete: true`): Requires both `deleteAfterDays` and `minToKeep` to be set (skips if either is nil).
   - Sort orphans by `created_at` descending (newest first)
   - Keep first `minToKeep` orphans
   - **Only delete INACTIVE or terminal failure deployments** (FAILED, DEPLOY_FAILED, BUILD_FAILED, BUILD_STOPPED) — ACTIVE, SCALED_TO_ZERO, BUILDING, DEPLOYING, etc. are never deleted
   - Delete remaining eligible orphans older than `deleteAfterDays` days
5. **Emit events**: `OrphanDeploymentsScaledIn` (Normal) and `OrphanDeploymentsDeleted` (Normal)
6. **Update status**: Set `lastCleanupTime` to current timestamp

**No opinionated defaults**: All cleanup parameters (`intervalMinutes`, `deleteAfterDays`, `minToKeep`) must be explicitly set. If any required parameter is nil, that operation is skipped. Recommended defensive values:
```yaml
orphanDeploymentCleanup:
  scaleToZero: true
  delete: true
  deleteAfterDays: 30        # 30 days — generous window for rollback
  minToKeep: 10              # always keep 10 newest orphans regardless of age
  intervalMinutes: 10080     # run once per week (7 * 24 * 60)
```

**Important**:
- With `sourceDeploymentName`: the deployment MUST already exist (created by `truss push` in CI/CD)
- With `trussConfig`: the operator creates the deployment if it doesn't exist, then promotes it
- No deployment IDs are cached in status except `candidateDeploymentID` (promote guard)
- Source deployment ID is only looked up when promotion is needed (step 5)
- The operator emits Kubernetes Events for important operations
- Orphan cleanup is opt-in and only runs during steady state (not during promotions or errors)

### Condition System and Argo CD Health

The controller sets **two conditions** on every status update for tri-state health reporting:

**Ready Condition:**
- **Type**: `Ready`
- **True**: Deployment is ACTIVE or SCALED_TO_ZERO in the environment (serving traffic)
- **False**: Deployment is not yet active, is building, or has failed
- **Reason**: Set to the current `deploymentStatus` value (e.g., `PROMOTING`, `ACTIVE`, `FAILED`)

**Progressing Condition:**
- **Type**: `Progressing`
- **True**: Work is in progress (building, promoting, activating, etc.)
- **False**: Terminal or stable state (failed, or ready)
- **Reason**: Set to the current `deploymentStatus` value

**Controller-Level Status Constants** (used in `deploymentStatus` and condition Reason):
- `PROMOTING` - Promotion initiated or in progress (double-promote guard active)
- `PROMOTING_DEPLOYMENT` - About to call Promote API
- `PENDING` - Waiting for precondition (environment creation, status check)
- `PAUSED` - Reconciliation paused via `spec.paused: true` (no API calls, no requeue)

**Baseten API Status Constants** (also used in `deploymentStatus`):
- `ACTIVE` - Deployment is live and serving traffic
- `SCALED_TO_ZERO` - Deployment is live but scaled to zero replicas
- `BUILDING`, `DEPLOYING`, `ACTIVATING`, `WAKING_UP`, `UPDATING`, `LOADING_MODEL` - Progressing states
- `FAILED`, `DEPLOY_FAILED`, `BUILD_FAILED`, `BUILD_STOPPED` - Terminal failure states

**Argo CD Health Mapping:**
- **Ready=True** → Healthy (green icon)
- **Ready=False + Progressing=True** → Progressing (yellow icon, indicates work in progress)
- **Ready=False + Progressing=False** → Degraded (red icon, indicates failure or stuck)

**Implementation:**
The `isProgressingStatus()` helper determines Progressing condition status:
```go
// Returns true for: PROMOTING, PROMOTING_DEPLOYMENT, PENDING, ACTIVATING, BUILDING, DEPLOYING, WAKING_UP, UPDATING, LOADING_MODEL
```

**Important Bug Fix:**
The double-promote guard now always sets `deploymentStatus: Promoting` instead of passing through the environment's `candidate_deployment.status`. This prevents incorrect Ready=True when an unrelated deployment's candidate happens to be ACTIVE.

### Baseten Client

The `internal/baseten/client.go` package provides a REST API client for Baseten. The controller depends on the `baseten.ClientInterface` interface to enable testing with mock implementations.

**Client Interface:**
The controller uses `baseten.ClientInterface`, not the concrete `*baseten.Client` directly. This enables dependency injection and testing with mocks.

**Model Operations:**
- `FindModelIDByName(ctx, modelName)` - List models and find by name

**Deployment Operations:**
- `FindDeploymentIDByName(ctx, modelID, deploymentName)` - Find deployment by name; returns `(id, status, error)` — the returned status eliminates the need for a separate `GetDeploymentStatus` call
- `ActivateDeployment(ctx, modelID, deploymentID)` - Activate a deployment
- `ListDeployments(ctx, modelID)` - List all deployments with full details (returns `[]DeploymentDetail`)
- `UpdateDeploymentAutoscaling(ctx, modelID, deploymentID, minReplica)` - Update deployment autoscaling (set min_replica)
- `DeleteDeployment(ctx, modelID, deploymentID)` - Delete a deployment
- `RetryDeployment(ctx, modelID, deploymentID)` - Retry a failed deployment; returns `*RetryResponse` with `Retried bool`, `Reason string`, and `Deployment *Deployment`

**Environment Operations:**
- `GetEnvironment(ctx, modelID, envName)` - Get specific environment with autoscaling settings
- `ListEnvironments(ctx, modelID)` - List all environments with full Environment objects (returns `[]Environment`)
- `CreateEnvironment(ctx, modelID, envConfig)` - Create new environment with autoscaling settings
- `UpdateEnvironmentSettings(ctx, modelID, envName, autoscalingConfig, promotionConfig)` - Update environment autoscaling and/or promotion settings in a single PATCH call (either config can be nil to skip that update)
- `Promote(ctx, modelID, deploymentID, targetEnv, settings)` - Promote deployment to environment

**Utility Functions:**
- `IsNotFoundError(err)` - Check if error is 404 not found (uses `errors.As()` with `*APIError`)
- `HasAutoscalingDrift(spec, env)` - Compare spec autoscaling vs environment, returns (bool, []string changes)
- `HasPromotionSettingsDrift(spec, env)` - Compare spec promotion settings vs environment, returns (bool, []string changes); mirrors `HasAutoscalingDrift` pattern
- `DeploymentNameMatchesPrefix(name, prefix)` - Check if deployment name matches prefix (handles timestamp suffix)
- `IsTerminalFailure(status)` - Check if status is a terminal failure (FAILED, DEPLOY_FAILED, BUILD_FAILED, BUILD_STOPPED)
- `IsRetryableFailure(status)` - Check if status is a retryable failure (FAILED, DEPLOY_FAILED, BUILD_FAILED); BUILD_STOPPED is excluded as it represents an intentional user action

**Error Handling:**
- All API errors are returned as `*APIError` with `StatusCode` and `Message` fields
- `IsNotFoundError(err)` uses `errors.As()` to check for 404 errors
- Controller receives structured errors with HTTP status codes

**Field Mapping:**
- CRD uses camelCase (e.g., `minReplicas`, `maxReplicas`)
- API uses snake_case with singular (e.g., `min_replica`, `max_replica`)
- Conversion happens automatically in client methods

**Context Support:**
All client methods accept `ctx context.Context` as the first parameter for cancellation, timeout, and tracing support.

**Authentication:** Requires `BASETEN_API_KEY` environment variable.

### Kubernetes Events

The operator emits Kubernetes Events for important operations, visible in `kubectl describe`:

**Normal Events:**
- `EnvironmentCreated` (Normal) - Environment created with autoscaling settings
- `AutoscalingUpdated` (Normal) - Autoscaling settings updated due to drift
- `PromotionSettingsUpdated` (Normal) - Promotion settings updated due to drift (when only promotion settings have drift, or alongside `AutoscalingUpdated` when both drift)
- `DeploymentPromoted` (Normal) - Deployment promoted to environment
- `DeploymentActive` (Normal) - Deployment is now active after promotion completes
- `TrussPushStarted` (Normal) - Truss push launched to create a deployment
- `TrussPushCompleted` (Normal) - Deployment created successfully via truss push (emitted on TRUSS_PUSHING → TRUSS_PUSH_DONE transition)
- `ReconciliationPaused` (Normal) - Reconciliation paused via `spec.paused: true` (emitted once on transition to paused)
- `OrphanDeploymentsScaledIn` (Normal) - Orphan deployments scaled in (min_replica=0) to release GPUs
- `OrphanDeploymentsDeleted` (Normal) - Stale orphan deployments deleted after exceeding age threshold
- `DeploymentRetried` (Normal) - Deployment retry initiated via Baseten retry API (emitted on each successful retry call)

**Warning Events:**
- `ModelNotFound` (Warning) - Model lookup failed or returned empty (emitted on FindModelIDByName failure or empty result)
- `EnvironmentCreateFailed` (Warning) - Environment creation failed
- `AutoscalingUpdateFailed` (Warning) - Autoscaling settings update failed (emitted when autoscaling drifted and the PATCH fails)
- `PromotionSettingsUpdateFailed` (Warning) - Promotion settings update failed (emitted when promotion settings drifted and the PATCH fails; both events emitted when both drift)
- `SetupScriptNotFound` (Warning) - ConfigMap referenced by `trussConfig.setupScript.configMapRef` not found (sets `deploymentStatus=FAILED`)
- `TrussPushFailed` (Warning) - Failed to prepare truss push (config generation error before launch, OR stale push timed out and is being retried)
- `SourceDeploymentNotFound` (Warning) - Source deployment lookup failed or returned empty
- `SourceDeploymentFailed` (Warning) - Source deployment is in terminal failure state (FAILED, etc.)
- `PromotionFailed` (Warning) - Promotion API call failed or candidate deployment in terminal failure state
- `PromotionBlocked` (Warning) - Double-promote guard prevents promotion (existing candidate in progress)
- `DeploymentRetryFailed` (Warning) - Retry API call failed or retry was declined by Baseten
- `DeploymentRetryExhausted` (Warning) - Deployment has been failing for over 2h; operator stops retrying (TerminalError returned)

**Note on async push:** The truss push itself runs in a background goroutine (`asyncPush`), but most events are emitted from the reconcile loop: `TrussPushStarted` when launching, `TrussPushCompleted` when the next reconcile finds the deployment, and `TrussPushFailed` if config generation fails before launch or if a stale push times out. The goroutine writes its result back to the CR status via `updatePushStatus()`: on success it sets `pushStatus=TRUSS_PUSH_DONE`; on failure it clears `pushStatus` so the next reconcile retries. `trussPushTime` is set when the goroutine launches and cleared by `updatePushStatus()` in all cases.

**RBAC Requirements:**
The controller requires permissions to create and patch events and to read ConfigMaps. This is configured via kubebuilder markers:
```go
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
```

**Important**: The ClusterRoleBinding must reference the correct ServiceAccount:
- ServiceAccount name: `baseten-operator-controller-manager`
- Namespace: `baseten-operator-system`

Events are created using the EventRecorder from controller-runtime, initialized in `cmd/main.go`:
```go
Recorder: mgr.GetEventRecorderFor("basetenmodel-controller")
```

## Common Commands

### Build and Test

```bash
# Build the operator binary
make build

# Run tests (unit tests, excluding e2e)
make test

# Run end-to-end tests (creates Kind cluster)
make test-e2e

# Clean up e2e test cluster
make cleanup-test-e2e

# Run linter
make lint

# Run linter with auto-fix
make lint-fix

# Format code
make fmt

# Run go vet
make vet

# Generate CRD manifests and deepcopy code
make manifests generate
```

### Running the Operator

```bash
# Run controller locally (requires kubeconfig)
make run

# Build and push Docker image
make docker-build docker-push IMG=<registry>/baseten-operator:tag

# Install CRDs into cluster
make install

# Deploy operator to cluster
make deploy IMG=<registry>/baseten-operator:tag

# Apply sample CR
kubectl apply -k config/samples/

# Uninstall CRDs
make uninstall

# Remove operator from cluster
make undeploy
```

### Development Workflow

```bash
# After modifying API types in api/v1alpha1/basetenmodel_types.go
make manifests generate

# Run locally for testing
export BASETEN_API_KEY=your-api-key
make run

# In another terminal, apply a test resource
kubectl apply -f config/samples/models_v1alpha1_basetenmodel.yaml

# Check resource status
kubectl get basetenmodels
kubectl get bm  # shortname

# View detailed status
kubectl describe bm basetenmodel-sample

# Check operator logs
kubectl logs -n baseten-operator-system deployment/baseten-operator-controller-manager
```

### Testing with Kind Cluster

For local testing with real Baseten API:

```bash
# Create Kind cluster
kind create cluster --name baseten-dev

# Build Docker image
make docker-build IMG=baseten-operator:dev

# IMPORTANT: Load image into Kind cluster
# Kind clusters cannot access local Docker images without explicitly loading them
kind load docker-image baseten-operator:dev --name baseten-dev

# Install CRDs
make install

# Deploy operator
make deploy IMG=baseten-operator:dev

# Verify operator is running
kubectl get pods -n baseten-operator-system

# Apply test resources
kubectl apply -f test/kind/test-valid-env.yaml

# Watch reconciliation
kubectl get bm -w

# Check events
kubectl describe bm <resource-name>

# View controller logs
kubectl logs -n baseten-operator-system deployment/baseten-operator-controller-manager -f

# Cleanup
kind delete cluster --name baseten-dev
```

**Important**: Always use `kind load docker-image` after building images. Without this, Kind will try to pull from a registry and fail to find your local image.

## Project Structure

```
api/v1alpha1/              # CRD type definitions
  basetenmodel_types.go
internal/
  baseten/                 # Baseten API client
    client.go              # REST client implementation
    mock_client.go         # Mock client for testing
    client_test.go         # Client unit tests
  controller/              # Reconciliation logic
    basetenmodel_controller.go
    basetenmodel_controller_test.go  # Controller unit tests
  truss/                   # Truss config generation and deployment push
    config.go              # Config YAML generation, hashing, WriteTrussDirectory
    config_test.go         # Config unit tests
    pusher.go              # truss-go SDK wrapper (Pusher, PusherInterface, MockPusher)
cmd/main.go                # Operator entrypoint
config/                    # Kustomize manifests
  crd/bases/               # Generated CRD YAML
  samples/                 # Example CRs
  rbac/                    # RBAC resources
  manager/                 # Operator deployment
test/
  e2e/                     # End-to-end tests using Ginkgo
```

## Development Notes

### Modifying the API

When adding or modifying fields in `BasetenModelSpec` or `BasetenModelStatus`:

1. Edit `api/v1alpha1/basetenmodel_types.go`
2. Add kubebuilder validation markers (e.g., `+kubebuilder:validation:MinLength=1`)
3. Run `make manifests generate` to update CRDs and generated code
4. If running in a cluster, run `make install` to update CRDs

### Controller Development

The controller uses controller-runtime's reconciliation loop. Key patterns:

- **Idempotency:** Controller may be called multiple times for same resource
- **Status updates:** All status writes go through `updateStatus()`, which takes a `statusUpdate` value, applies it to the in-memory object, then compares via `statusSnapshot` equality and `conditionsChanged()` before calling `r.Status().Update()`. The API server write is **skipped entirely** if nothing meaningful changed — this prevents triggering a watch event that would cause an extra reconcile loop (and extra Baseten API calls). In steady state, this reduces Baseten API calls from ~3 per cycle to 1.
- **Non-critical status updates:** Use `r.logUpdateStatus()` wrapper that logs errors but doesn't fail reconciliation
- **Error handling:** Returning an error triggers requeue with exponential backoff
- **Logging:** Use `log.FromContext(ctx)` for structured logging
- **Requeuing:** Use `ctrl.Result{RequeueAfter: duration}` for polling
- **Events:** Use `r.Recorder.Event()` or `r.Recorder.Eventf()` to emit Kubernetes Events
- **Environment reconciliation:** Happens early (Step 2) to ensure correct configuration before deployment operations
- **Drift detection:** Automatically detects and reconciles autoscaling and promotion settings changes; uses `HasAutoscalingDrift()` and `HasPromotionSettingsDrift()` helpers, then calls `UpdateEnvironmentSettings()` with only the drifted configs
- **Validation:** Environment names must be lowercase alphanumeric only (enforced at CRD level)
- **Dependency injection:** Controller uses `baseten.ClientInterface` and `truss.PusherInterface` for testability with mock implementations (`baseten.MockClient`, `truss.MockPusher`)
- **Condition handling:** Always set both Ready and Progressing conditions together using `setCondition()` helper. Per Kubernetes convention, `LastTransitionTime` is only updated when the condition's `.status` field actually changes (e.g., True→False); it is preserved unchanged when only `Reason` or `Message` changes. Whether conditions changed is checked via `conditionsChanged()` before deciding to write to the API server.
- **Status constants:** Use controller-level constants (`PROMOTING`, `PROMOTING_DEPLOYMENT`, `PENDING`) for states managed by the reconciler; terminal failures use `baseten.DeploymentStatusFailed` (`"FAILED"`) consistently for all error paths
- **Progressing classification:** Use `isProgressingStatus()` to determine if a status represents work in progress
- **Automatic retry:** When a candidate or source deployment enters a retryable failure state (FAILED, DEPLOY_FAILED, BUILD_FAILED), the controller calls `handleCandidateFailure()` → `tryRetryDeployment()`. Uses exponential backoff (2m base, doubling, 30m cap, 0-50% jitter) tracked via `status.deploymentRetryCount` and `status.nextRetryTime`. A concurrency guard re-checks live deployment status before each retry call (handles multiple CRs on the same model). The 2-hour retry window is tracked via `status.firstDeploymentFailureTime`. After 2h, `reconcile.TerminalError()` is returned so the resource stops requeueing until the spec changes. BUILD_STOPPED is intentional and not retried. Always-on — no spec config needed.

### Testing

Tests use Ginkgo/Gomega framework with extensive coverage:

**Controller Unit Tests:**
- Location: `internal/controller/basetenmodel_controller_test.go`
- Coverage: 24 test cases covering all reconciliation scenarios
- Mock client: Uses `baseten.MockClient` for isolated testing
- Tests include: model lookup, environment creation/drift, promotion logic, error handling

**Client Unit Tests:**
- Location: `internal/baseten/client_test.go`
- Uses `httptest` for API response mocking
- Tests all client methods and utility functions
- Includes error handling and edge case coverage

**Mock Client:**
- Location: `internal/baseten/mock_client.go`
- Implements `baseten.ClientInterface`
- Supports error injection for testing failure scenarios

**Truss Package Tests:**
- Location: `internal/truss/config_test.go`
- Tests: `GenerateConfigYAML`, `HashTrussConfig`, `DeploymentName`, `WriteTrussDirectory`
- Uses standard `testing` package (not Ginkgo)
- `truss.MockPusher` in `pusher.go` implements `PusherInterface` for controller testing

**E2E Tests:**
- Location: `test/e2e/e2e_test.go`
- Requires Kind cluster with real Baseten API

Run specific test suites:
```bash
# Run all unit tests
go test ./... -v

# Run controller tests only
go test ./internal/controller/... -v

# Run client tests only
go test ./internal/baseten/... -v

# Run truss package tests only
go test ./internal/truss/... -v

# Run e2e tests with verbose output
make test-e2e
```

### Baseten API Integration

The operator requires:
1. `BASETEN_API_KEY` environment variable set (used by both Baseten REST client and truss-go SDK pusher)
2. Models must already exist in Baseten (created via UI or `truss push`)
3. Deployments: either pre-existing (created by CI/CD via `truss push`) when using `sourceDeploymentName`, or created by the operator when using `trussConfig`

**Key API Endpoints Used:**
- `GET /v1/models` - List models
- `GET /v1/models/{model_id}/deployments` - List deployments (full details including created_at, autoscaling_settings)
- `GET /v1/models/{model_id}/environments` - List all environments (full Environment objects)
- `GET /v1/models/{model_id}/environments/{env_name}` - Get specific environment with autoscaling settings
- `POST /v1/models/{model_id}/environments` - Create environment
- `PATCH /v1/models/{model_id}/environments/{env_name}` - Update environment autoscaling and/or promotion settings
- `PATCH /v1/models/{model_id}/deployments/{deployment_id}/autoscaling_settings` - Update deployment autoscaling
- `DELETE /v1/models/{model_id}/deployments/{deployment_id}` - Delete deployment
- `POST /v1/models/{model_id}/deployments/{deployment_id}/retry` - Retry a failed deployment
- `POST /v1/models/{model_id}/environments/{env_name}/promote` - Promote deployment

When extending the Baseten client:
- **All methods must accept `ctx context.Context`** as the first parameter
- Return `*APIError` for HTTP errors (include status code and message)
- Use `newRequest()` and `doRequest()` internal helpers for consistency
- All methods use JSON API with `Api-Key` header
- Base URL: `https://api.baseten.co/v1`
- Convert between CRD camelCase and API snake_case field names
- All autoscaling fields are optional (use pointers in Go structs)
- Only pass non-nil fields to API (Baseten handles defaults)
- **Add new methods to `ClientInterface`** if they're needed by the controller
- Write unit tests using `httptest` for API mocking

## Kubebuilder Scaffolding

This project was scaffolded with Kubebuilder v4.10.1. Standard kubebuilder patterns apply:

- API types in `api/v<version>/`
- Controllers in `internal/controller/`
- Kubebuilder markers for RBAC, validation, printing columns
- Kustomize-based deployment in `config/`

To add new resources:
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

## Current Design vs Original Design

The current implementation differs from `docs/design.md`:

**Original design:**
- Operator would deploy models using truss
- Inline truss configuration in CRD
- Complex vLLM configuration fields
- Single CRD named `ModelDeployment`
- API group: `baseten.com/v1alpha1`

**Current implementation (v1alpha1):**
- Operator only promotes existing deployments (simpler scope)
- CI/CD handles `truss push` to create deployments
- CRD named `BasetenModel` (API group: `models.baseten.com/v1alpha1`)
- Focused on environment promotion and configuration management
- Deployment naming convention: `img-{ver}-wgt-{ver}-p-{ver}`
- **Autoscaling support**: Full autoscaling configuration per environment
- **Environment reconciliation**: Early reconciliation with drift detection
- **Kubernetes Events**: Emits events for important operations
- **Validation**: CRD-level validation for environment names

**Key Features Added:**
1. **Autoscaling Configuration**: Complete support for min/max replicas, concurrency target, scaling windows
2. **Drift Detection**: Automatically detects and reconciles autoscaling and promotion setting changes
3. **Environment Reconciliation**: Happens early (Step 2) before deployment operations
4. **Kubernetes Events**: Provides visibility into operator actions via standard K8s events
5. **Field Validation**: Environment names restricted to lowercase alphanumeric at CRD admission
6. **Tri-State Health Conditions**: Ready + Progressing conditions enable Argo CD to distinguish healthy/progressing/degraded states
7. **Orphan Deployment Cleanup**: Automatic cleanup of unused deployments to reclaim leaked GPU resources (opt-in)
8. **TrussConfig Support**: Operator can create deployments directly via inline truss configuration (`spec.trussConfig`), eliminating the need for CI/CD to run `truss push`. Deployment names are deterministic from base image + config hash (e.g., `depl-vllm-0.11.2.1-9a1d1cbc`), enabling idempotent pushes across reconcile cycles and multiple CRs sharing the same config. Push runs asynchronously (non-blocking reconcile loop), with `deploymentStatus=DEPLOYING` shown while the push is in progress.
9. **Automatic Deployment Retry**: When a candidate or source deployment enters a retryable failure state (FAILED, DEPLOY_FAILED, BUILD_FAILED), the operator automatically calls the Baseten retry API with exponential backoff (2m base, doubling, 30m cap, 0-50% jitter). Retries continue for 2 hours from the first failure (`status.firstDeploymentFailureTime`); retry state is tracked in `status.deploymentRetryCount` and `status.nextRetryTime`. A concurrency guard re-checks live deployment status before each retry call. After 2h, a `TerminalError` is returned. BUILD_STOPPED is not retried (intentional user action). Always-on with no spec configuration required.

The operator supports two mutually exclusive source workflows: CI/CD-created deployments (`sourceDeploymentName`) and operator-created deployments (`trussConfig`).

## TrussConfig Support

**Status:** Implemented in v1alpha1
**Design Document:** See `docs/truss-config-design.md` for the original design

The operator supports **creating deployments directly** via inline truss configuration, eliminating the need for CI/CD to run `truss push`.

**Workflow A: CI/CD-created deployment (`sourceDeploymentName`)**
```
CI/CD: truss push → deployment created (naming: img-{ver}-wgt-{ver}-p-{ver})
User: Create BasetenModel CR with sourceDeploymentName
Operator: Promote deployment to environment
```

**Workflow B: Operator-created deployment (`trussConfig`)**
```
User: Create BasetenModel CR with trussConfig
Operator: Hash config → deployment name depl-{imageName}-{imageTag}-{hash}
Operator: Check if deployment exists (idempotent)
If not: Generate config.yaml, launch async goroutine to push (non-blocking), requeue 10s
On next reconcile: deployment found → promote to environment
```

### Example CR with trussConfig

```yaml
spec:
  modelName: "my-llm-model"
  trussConfig:
    pythonVersion: "py312"
    resources:
      accelerator: "H100:1"
    baseImage:
      image: "us-docker.pkg.dev/my-project/my-repo/vllm:0.16.0"
      dockerAuth:
        authMethod: "GCP_SERVICE_ACCOUNT_JSON"
        secretName: "docker-registry-secret"
        registry: "us-docker.pkg.dev"
    setupScript:
      configMapRef:
        name: my-model-setup
        key: setup.sh
  environment:
    name: "dev"
```

### Key Design Decisions

1. **Inline vs Separate CRD:** Truss config is **inlined** in BasetenModel, not a separate CRD — one CR per deployment, clear ownership
2. **Setup Script Handling:** Large scripts stored in **ConfigMaps** and referenced via `setupScript.configMapRef` — keeps CRs readable
3. **Mutual Exclusivity:** CEL validation enforces that exactly one of `sourceDeploymentName` or `trussConfig` is specified
4. **Deterministic Naming:** Deployment name `depl-{imageName}-{imageTag}-{hash}` is derived from the base image URI and config hash (e.g., `depl-vllm-0.11.2.1-9a1d1cbc`) — same config across multiple CRs or reconcile cycles = only one push
5. **Idempotency:** `FindDeploymentIDByName()` check before push ensures no duplicate pushes even if the controller restarts mid-push. In-flight pushes are tracked via `status.pushStatus=TRUSS_PUSHING` so the reconcile loop avoids launching duplicate goroutines.
6. **Non-blocking push:** The truss push runs in a background goroutine (`asyncPush`). The reconcile loop sets `deploymentStatus=DEPLOYING`, updates `pushStatus=TRUSS_PUSHING` + `trussPushTime=now`, then returns immediately with a 10-second requeue. The goroutine writes its outcome back to the CR via `updatePushStatus()`. On the next reconcile, `FindDeploymentIDByName()` re-checks whether the deployment appeared.
7. **Stale push recovery:** If the goroutine dies before writing back (e.g., pod restart), `trussPushTime` age is checked on each reconcile. After `trussPushStaleTimeout` (5 minutes), the stale state is cleared, `TrussPushFailed` warning is emitted, and the push is retried automatically.

### Packages

- `internal/truss/config.go` — `GenerateConfigYAML()`, `HashTrussConfig()`, `DeploymentName()`, `WriteTrussDirectory()`
- `internal/truss/pusher.go` — `Pusher` (wraps truss-go SDK), `PusherInterface`, `MockPusher`

### Dependencies

- [`github.com/basetenlabs/truss-go`](https://github.com/basetenlabs/truss-go) — programmatic truss push SDK
- Controller reads ConfigMaps (RBAC: `get;list;watch` on core/configmaps)

### Benefits

- **Eliminates CI/CD deployment creation workflows** — only image build CI/CD step needed
- **Enables fully declarative GitOps** — entire deployment lifecycle in Git
- **Config hash = deployment identity** — rollback by reverting Git config, no manual deployment tracking

