/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	"github.com/abridgeai/baseten-operator/internal/truss"
)

const (
	statusPromoting           = "PROMOTING"
	statusPromotingDeployment = "PROMOTING_DEPLOYMENT"
	statusPending             = "PENDING"
	statusTrussPushing        = "TRUSS_PUSHING"
	statusTrussPushDone       = "TRUSS_PUSH_DONE"
	statusPaused              = "PAUSED"

	conditionReady       = "Ready"
	conditionProgressing = "Progressing"

	// trussPushStaleTimeout is the safety net for clearing stale TRUSS_PUSHING state
	// if the goroutine is killed before it can write back (e.g., pod restart).
	trussPushStaleTimeout = 5 * time.Minute

	// deploymentRetryDeadline is how long the operator retries failed deployments
	// before giving up with a TerminalError. After this, the resource stops requeueing
	// until the spec changes.
	deploymentRetryDeadline = 2 * time.Hour

	// retryBaseInterval is the initial backoff interval for deployment retries.
	retryBaseInterval = 2 * time.Minute

	// retryMaxInterval is the maximum backoff interval for deployment retries.
	retryMaxInterval = 30 * time.Minute

	// retryJitterFraction is the maximum fraction of jitter added to retry intervals (0-50%).
	retryJitterFraction = 0.5
)

// BasetenModelReconciler reconciles a BasetenModel object
type BasetenModelReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	BasetenClient           baseten.ClientInterface
	TrussPusher             truss.PusherInterface
	Recorder                record.EventRecorder
	MaxConcurrentReconciles int           // concurrent reconcile workers (default 1)
	PushSemaphore           chan struct{} // bounds concurrent async truss push goroutines
}

type statusUpdate struct {
	deploymentStatus           string
	message                    string
	modelID                    string
	sourceDeploymentID         string
	sourceDeploymentName       string
	activeDeploymentName       string
	candidateDeploymentID      string
	candidateDeploymentName    string
	clearCandidate             bool
	replicaCount               int32
	firstDeploymentFailureTime *metav1.Time
}

// +kubebuilder:rbac:groups=models.baseten.com,resources=basetenmodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=models.baseten.com,resources=basetenmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=models.baseten.com,resources=basetenmodels/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *BasetenModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling BasetenModel", "name", req.Name, "namespace", req.Namespace)

	model := &modelsv1alpha1.BasetenModel{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Pause check: skip all reconciliation when paused
	if model.Spec.Paused {
		return r.reconcilePaused(ctx, model)
	}

	// Step 1: Resolve model ID (cached in status after first lookup)
	modelID, result, err := r.resolveModelID(ctx, model)
	if err != nil || result != nil {
		return *result, err
	}

	envName := model.Spec.Environment.Name

	// Step 1.5: Determine source deployment (either from spec or via truss push)
	sourceDeploymentName, depResult, err := r.resolveSourceDeployment(ctx, model, modelID)
	if err != nil || depResult != nil {
		if r.invalidateModelID(ctx, model, err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return *depResult, err
	}

	// Model not yet created — push launched or in flight, wait for it
	if modelID == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Step 2: Reconcile environment (create if needed, fix autoscaling drift)
	env, envResult, err := r.reconcileEnvironment(ctx, model, modelID, envName)
	if err != nil || envResult != nil {
		return *envResult, err
	}

	logger.Info("Environment reconciled", "environment", envName)

	if env.CurrentDeployment != nil && baseten.DeploymentNameMatchesPrefix(env.CurrentDeployment.Name, sourceDeploymentName) {
		logger.V(1).Info("Current deployment matches source prefix",
			"currentDeployment", env.CurrentDeployment.Name,
			"sourceDeploymentName", sourceDeploymentName,
			"status", env.CurrentDeployment.Status)

		su := statusUpdate{
			deploymentStatus:     env.CurrentDeployment.Status,
			message:              steadyStateMessage(env.CurrentDeployment, envName, env.AutoscalingSettings),
			modelID:              modelID,
			activeDeploymentName: env.CurrentDeployment.Name,
			replicaCount:         env.CurrentDeployment.ActiveReplicaCount,
		}

		if model.Status.CandidateDeploymentID != "" || model.Status.CandidateDeploymentName != "" {
			logger.Info("Promotion complete, clearing candidate fields")
			su.clearCandidate = true
			r.Recorder.Eventf(model, corev1.EventTypeNormal, EventDeploymentActive, "Deployment %s is now active in %s", env.CurrentDeployment.Name, envName)
		}

		if err := r.updateStatus(ctx, model, su); err != nil {
			logger.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}

		// Orphan cleanup: only when opted in and in steady state
		if model.Spec.OrphanDeploymentCleanup != nil {
			r.reconcileOrphanCleanup(ctx, model, modelID)
		}

		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	if env.CandidateDeployment != nil && baseten.DeploymentNameMatchesPrefix(env.CandidateDeployment.Name, sourceDeploymentName) {
		logger.Info("Candidate deployment matches source prefix, promotion in progress",
			"candidateDeployment", env.CandidateDeployment.Name,
			"candidateStatus", env.CandidateDeployment.Status)

		su := statusUpdate{
			deploymentStatus:        statusPromoting,
			message:                 promotionMessage(env.CurrentDeployment, env.CandidateDeployment),
			modelID:                 modelID,
			candidateDeploymentName: env.CandidateDeployment.Name,
			replicaCount:            env.CandidateDeployment.ActiveReplicaCount,
		}
		if env.CurrentDeployment != nil {
			su.activeDeploymentName = env.CurrentDeployment.Name
		}
		if model.Status.CandidateDeploymentID != "" {
			su.candidateDeploymentID = model.Status.CandidateDeploymentID
		}

		if baseten.IsTerminalFailure(env.CandidateDeployment.Status) {
			return r.handleCandidateFailure(ctx, model, modelID, env, su)
		}

		r.logUpdateStatus(ctx, model, su)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("No matching deployment in environment, checking promotion eligibility",
		"sourceDeploymentName", sourceDeploymentName)

	if result := r.checkDoublePromoteGuard(ctx, model, env, modelID, sourceDeploymentName); result != nil {
		return *result, nil
	}

	sourceDeploymentID, result, err := r.validateSourceDeployment(ctx, model, modelID, sourceDeploymentName, env.CurrentDeployment)
	if err != nil || result != nil {
		if r.invalidateModelID(ctx, model, err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return *result, err
	}

	logger.Info("Promoting source deployment to target environment",
		"sourceDeploymentID", sourceDeploymentID,
		"sourceDeploymentName", sourceDeploymentName,
		"targetEnvironment", envName)

	r.logUpdateStatus(ctx, model, statusUpdate{
		deploymentStatus:     statusPromotingDeployment,
		message:              fmt.Sprintf("%spromoting %s to %s", activePrefix(env.CurrentDeployment), sourceDeploymentName, envName),
		modelID:              modelID,
		sourceDeploymentID:   sourceDeploymentID,
		sourceDeploymentName: sourceDeploymentName,
	})

	targetDep, err := r.BasetenClient.Promote(ctx, modelID, sourceDeploymentID, envName, model.Spec.Environment.PromotionSettings)
	if err != nil {
		logger.Error(err, "Failed to promote source deployment")
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventPromotionFailed, "Failed to promote %s to %s: %v", sourceDeploymentName, envName, err)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus:     baseten.DeploymentStatusFailed,
			message:              fmt.Sprintf("%spromotion of '%s' to %s failed: %v", activePrefix(env.CurrentDeployment), sourceDeploymentName, envName, err),
			modelID:              modelID,
			sourceDeploymentID:   sourceDeploymentID,
			sourceDeploymentName: sourceDeploymentName,
		})
		return ctrl.Result{}, err
	}

	logger.Info("Source deployment promoted successfully",
		"sourceDeploymentID", sourceDeploymentID,
		"sourceDeploymentName", sourceDeploymentName,
		"targetDeploymentID", targetDep.ID,
		"targetDeploymentName", targetDep.Name,
		"targetStatus", targetDep.Status)

	r.Recorder.Eventf(model, corev1.EventTypeNormal, EventDeploymentPromoted, "Promoted %s to %s environment", sourceDeploymentName, envName)

	su := statusUpdate{
		deploymentStatus:        statusPromoting,
		message:                 fmt.Sprintf("%spromoted %s to %s (%s)", activePrefix(env.CurrentDeployment), sourceDeploymentName, envName, targetDep.Status),
		modelID:                 modelID,
		sourceDeploymentID:      sourceDeploymentID,
		sourceDeploymentName:    sourceDeploymentName,
		candidateDeploymentID:   targetDep.ID,
		candidateDeploymentName: targetDep.Name,
		replicaCount:            targetDep.ActiveReplicaCount,
	}
	if env.CurrentDeployment != nil {
		su.activeDeploymentName = env.CurrentDeployment.Name
	}

	if err := r.updateStatus(ctx, model, su); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// checkDoublePromoteGuard prevents re-promoting when a promotion is already in progress.
// Returns a non-nil result if the guard is active (caller should return immediately).
func (r *BasetenModelReconciler) checkDoublePromoteGuard(ctx context.Context, model *modelsv1alpha1.BasetenModel, env *baseten.Environment, modelID, sourceDeploymentName string) *ctrl.Result {
	logger := log.FromContext(ctx)

	// Clear stale candidate if source changed or promotion completed
	if model.Status.CandidateDeploymentID != "" {
		if env.CandidateDeployment == nil || !baseten.DeploymentNameMatchesPrefix(env.CandidateDeployment.Name, sourceDeploymentName) {
			logger.Info("Clearing stale candidateDeploymentID (source changed or promotion completed)",
				"staleCandidateDeploymentID", model.Status.CandidateDeploymentID)
			model.Status.CandidateDeploymentID = ""
			model.Status.CandidateDeploymentName = ""
		}
	}

	if model.Status.CandidateDeploymentID == "" && env.CandidateDeployment == nil {
		return nil
	}

	candidateName := ""
	candidateStatus := ""
	if env.CandidateDeployment != nil {
		candidateName = env.CandidateDeployment.Name
		candidateStatus = env.CandidateDeployment.Status
	}
	logger.Info("Another promotion already in progress, waiting",
		"candidateDeploymentID", model.Status.CandidateDeploymentID,
		"envCandidateName", candidateName)

	r.Recorder.Eventf(model, corev1.EventTypeWarning, EventPromotionBlocked, "Waiting for existing promotion to complete (candidate: %s)", candidateName)

	msg := fmt.Sprintf("%swaiting for existing promotion to complete (candidate: %s, status: %s)", activePrefix(env.CurrentDeployment), candidateName, candidateStatus)
	if env.CandidateDeployment != nil {
		msg = promotionMessage(env.CurrentDeployment, env.CandidateDeployment) + " — waiting for completion before promoting " + sourceDeploymentName
	}

	su := statusUpdate{
		deploymentStatus:        statusPromoting,
		message:                 msg,
		modelID:                 modelID,
		candidateDeploymentID:   model.Status.CandidateDeploymentID,
		candidateDeploymentName: candidateName,
	}
	if env.CurrentDeployment != nil {
		su.activeDeploymentName = env.CurrentDeployment.Name
	}

	r.logUpdateStatus(ctx, model, su)
	result := ctrl.Result{RequeueAfter: 30 * time.Second}
	return &result
}

func (r *BasetenModelReconciler) reconcileOrphanCleanup(ctx context.Context, model *modelsv1alpha1.BasetenModel, modelID string) {
	logger := log.FromContext(ctx)
	cleanup := model.Spec.OrphanDeploymentCleanup

	// Skip cleanup if interval not configured (feature disabled)
	if cleanup.IntervalMinutes == nil {
		return
	}
	// Skip cleanup if interval hasn't elapsed (rate limiting to avoid API churn)
	if model.Status.LastCleanupTime != nil && time.Since(model.Status.LastCleanupTime.Time) < time.Duration(*cleanup.IntervalMinutes)*time.Minute {
		return
	}

	scaleIn := cleanup.ScaleToZero != nil && *cleanup.ScaleToZero
	deleteStale := cleanup.Delete != nil && *cleanup.Delete
	// Skip cleanup if neither action is enabled (nothing to do)
	if !scaleIn && !deleteStale {
		return
	}

	orphans, err := r.findOrphanDeployments(ctx, model, modelID)
	if err != nil {
		logger.Error(err, "Failed to find orphan deployments")
		return
	}

	logger.Info("Orphan cleanup: found orphan deployments", "count", len(orphans))

	scaledIn := 0
	var scaledNames []string
	if scaleIn {
		scaledIn, scaledNames = r.scaleInOrphans(ctx, modelID, orphans)
	}

	deleted := 0
	var deletedNames []string
	if deleteStale {
		deleted, deletedNames = r.deleteStaleOrphans(ctx, modelID, orphans, cleanup)
	}

	if scaledIn > 0 {
		r.Recorder.Eventf(model, corev1.EventTypeNormal, EventOrphanDeploymentsScaledIn, "Scaled in %d orphan deployments: %s", scaledIn, strings.Join(scaledNames, ", "))
	}
	if deleted > 0 {
		r.Recorder.Eventf(model, corev1.EventTypeNormal, EventOrphanDeploymentsDeleted, "Deleted %d stale orphan deployments: %s", deleted, strings.Join(deletedNames, ", "))
	}

	now := metav1.Now()
	model.Status.LastCleanupTime = &now
	if err := r.Status().Update(ctx, model); err != nil {
		logger.Error(err, "Failed to update lastCleanupTime")
	}
}

func (r *BasetenModelReconciler) findOrphanDeployments(ctx context.Context, model *modelsv1alpha1.BasetenModel, modelID string) ([]baseten.DeploymentDetail, error) {
	deployments, err := r.BasetenClient.ListDeployments(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}

	environments, err := r.BasetenClient.ListEnvironments(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("listing environments: %w", err)
	}

	protected := make(map[string]bool)
	for _, env := range environments {
		if env.CurrentDeployment != nil {
			protected[env.CurrentDeployment.ID] = true
		}
		if env.CandidateDeployment != nil {
			protected[env.CandidateDeployment.ID] = true
		}
	}

	orphans := make([]baseten.DeploymentDetail, 0, len(deployments))
	for _, dep := range deployments {
		// Skip deployments actively serving traffic in any environment
		if protected[dep.ID] {
			continue
		}
		// Skip production and development deployments (always protected regardless of environment status)
		if dep.IsProduction || dep.IsDevelopment {
			continue
		}
		// Skip deployments matching the source deployment prefix (current desired version)
		sourceDepName := model.Spec.SourceDeploymentName
		if sourceDepName == "" {
			sourceDepName = model.Status.SourceDeploymentName
		}
		if sourceDepName != "" && baseten.DeploymentNameMatchesPrefix(dep.Name, sourceDepName) {
			continue
		}
		orphans = append(orphans, dep)
	}
	return orphans, nil
}

func (r *BasetenModelReconciler) scaleInOrphans(ctx context.Context, modelID string, orphans []baseten.DeploymentDetail) (int, []string) {
	logger := log.FromContext(ctx)
	scaledIn := 0
	names := make([]string, 0, len(orphans))
	for _, dep := range orphans {
		// Skip if already scaled to zero (no API call needed)
		if dep.AutoscalingSettings != nil && dep.AutoscalingSettings.MinReplica == 0 {
			continue
		}
		if err := r.BasetenClient.UpdateDeploymentAutoscaling(ctx, modelID, dep.ID, 0); err != nil {
			logger.Error(err, "Failed to scale in orphan deployment", "deploymentID", dep.ID, "deploymentName", dep.Name)
			continue
		}
		logger.Info("Scaled in orphan deployment", "deploymentID", dep.ID, "deploymentName", dep.Name)
		scaledIn++
		names = append(names, dep.Name)
	}
	return scaledIn, names
}

func (r *BasetenModelReconciler) deleteStaleOrphans(ctx context.Context, modelID string, orphans []baseten.DeploymentDetail, cleanup *modelsv1alpha1.OrphanDeploymentCleanupConfig) (int, []string) {
	logger := log.FromContext(ctx)

	// Skip deletion if safety parameters not configured (prevents accidental mass deletion)
	if cleanup.DeleteAfterDays == nil || cleanup.MinToKeep == nil {
		logger.Info("Skipping stale orphan deletion: deleteAfterDays and minToKeep must be set")
		return 0, nil
	}
	staleDays := *cleanup.DeleteAfterDays
	minKeep := *cleanup.MinToKeep

	sort.Slice(orphans, func(i, j int) bool {
		return orphans[i].CreatedAt > orphans[j].CreatedAt
	})

	cutoff := time.Now().AddDate(0, 0, -int(staleDays))
	deleted := 0
	names := make([]string, 0, len(orphans))
	for i, dep := range orphans {
		// Skip the N newest orphans (safety buffer to avoid deleting recent deployments)
		if int32(i) < minKeep {
			continue
		}
		// Only delete INACTIVE or terminal failure deployments — SCALED_TO_ZERO can still wake up and serve traffic
		if dep.Status != baseten.DeploymentStatusInactive && !baseten.IsTerminalFailure(dep.Status) {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339, dep.CreatedAt)
		if err != nil {
			logger.Error(err, "Failed to parse deployment created_at", "deploymentID", dep.ID, "createdAt", dep.CreatedAt)
			continue
		}
		// Skip deployments newer than the staleness cutoff (not old enough to delete)
		if createdAt.After(cutoff) {
			continue
		}
		if err := r.BasetenClient.DeleteDeployment(ctx, modelID, dep.ID); err != nil {
			logger.Error(err, "Failed to delete stale orphan deployment", "deploymentID", dep.ID, "deploymentName", dep.Name)
			continue
		}
		logger.Info("Deleted stale orphan deployment", "deploymentID", dep.ID, "deploymentName", dep.Name)
		deleted++
		names = append(names, dep.Name)
	}
	return deleted, names
}

func (r *BasetenModelReconciler) reconcilePaused(ctx context.Context, model *modelsv1alpha1.BasetenModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciliation paused", "name", model.Name)

	msg := "reconciliation paused"
	if model.Status.Message != "" && !strings.HasPrefix(model.Status.Message, "reconciliation paused") {
		msg = fmt.Sprintf("reconciliation paused | last status: %s", model.Status.Message)
	} else if model.Status.Message != "" {
		msg = model.Status.Message
	}

	if model.Status.DeploymentStatus != statusPaused {
		r.Recorder.Eventf(model, corev1.EventTypeNormal, EventReconciliationPaused, "Reconciliation paused for %s", model.Spec.ModelName)
	}

	r.logUpdateStatus(ctx, model, statusUpdate{
		deploymentStatus: statusPaused,
		message:          msg,
	})
	return ctrl.Result{}, nil
}

// handleCandidateFailure handles a candidate deployment in terminal failure state.
// For retryable failures (FAILED, DEPLOY_FAILED, BUILD_FAILED), it attempts retry via the Baseten API.
// For non-retryable failures (BUILD_STOPPED), it reports the error directly.
func (r *BasetenModelReconciler) handleCandidateFailure(
	ctx context.Context,
	model *modelsv1alpha1.BasetenModel,
	modelID string,
	env *baseten.Environment,
	su statusUpdate,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	candidate := env.CandidateDeployment

	if baseten.IsRetryableFailure(candidate.Status) {
		candidateID := candidate.ID
		if candidateID == "" {
			candidateID = model.Status.CandidateDeploymentID
		}
		if candidateID != "" {
			if result, shouldReturn := r.tryRetryDeployment(ctx, model, modelID, candidateID, candidate.Name, candidate.Status); shouldReturn {
				return *result, nil
			}
		}
		// Past deadline — terminal error
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventPromotionFailed, "Candidate deployment %s failed with status %s (retries exhausted)", candidate.Name, candidate.Status)
		r.logUpdateStatus(ctx, model, su)
		return ctrl.Result{}, reconcile.TerminalError(fmt.Errorf("candidate deployment %s failed with status %s (retries exhausted after %s)", candidate.Name, candidate.Status, deploymentRetryDeadline))
	}

	// Non-retryable terminal failure (BUILD_STOPPED)
	su.deploymentStatus = candidate.Status
	su.message = candidateFailedMessage(env.CurrentDeployment, candidate)
	logger.Info("Candidate deployment in terminal failure state", "status", candidate.Status)
	r.Recorder.Eventf(model, corev1.EventTypeWarning, EventPromotionFailed, "Candidate deployment %s failed with status %s", candidate.Name, candidate.Status)
	r.logUpdateStatus(ctx, model, su)
	return ctrl.Result{}, fmt.Errorf("candidate deployment %s failed with status %s", candidate.Name, candidate.Status)
}

// tryRetryDeployment attempts to retry a failed deployment via the Baseten retry API.
// Uses exponential backoff (2m base, doubling, capped at 30m) with 0-50% jitter.
// Returns (result, true) if the caller should return the result (retry scheduled or in progress).
// Returns (nil, false) if retries are exhausted (past 2h deadline) — caller should handle as permanent failure.
func (r *BasetenModelReconciler) tryRetryDeployment(
	ctx context.Context,
	model *modelsv1alpha1.BasetenModel,
	modelID, deploymentID, deploymentName, failedStatus string,
) (*ctrl.Result, bool) {
	logger := log.FromContext(ctx)

	// Set firstDeploymentFailureTime on first detection
	firstFailure := model.Status.FirstDeploymentFailureTime
	if firstFailure == nil {
		now := metav1.Now()
		firstFailure = &now
		model.Status.FirstDeploymentFailureTime = firstFailure
		if err := r.Status().Update(ctx, model); err != nil {
			logger.Error(err, "Failed to set firstDeploymentFailureTime")
		}
	}

	// Check 2h deadline
	if time.Since(firstFailure.Time) > deploymentRetryDeadline {
		logger.Info("Deployment retry deadline exceeded", "deploymentName", deploymentName, "firstFailure", firstFailure.Time)
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventDeploymentRetryExhausted,
			"Deployment %s has been failing for over %s, giving up retries", deploymentName, deploymentRetryDeadline)
		return nil, false
	}

	// Check backoff: skip retry if nextRetryTime hasn't elapsed
	if model.Status.NextRetryTime != nil && time.Now().Before(model.Status.NextRetryTime.Time) {
		remaining := time.Until(model.Status.NextRetryTime.Time)
		logger.Info("Retry backoff not elapsed, waiting", "deploymentName", deploymentName,
			"nextRetryTime", model.Status.NextRetryTime.Time, "remaining", remaining,
			"retryCount", model.Status.DeploymentRetryCount)
		result := ctrl.Result{RequeueAfter: remaining}
		return &result, true
	}

	// Concurrency guard: re-check deployment status before retrying.
	// Another CR managing the same model may have already retried this deployment.
	currentStatus := failedStatus
	_, freshStatus, err := r.BasetenClient.FindDeploymentIDByName(ctx, modelID, deploymentName)
	if err != nil {
		logger.Error(err, "Failed to re-check deployment status before retry", "deploymentName", deploymentName)
		// Proceed with the status we have — the retry API is idempotent
	} else if freshStatus != "" {
		currentStatus = freshStatus
	}
	if !baseten.IsRetryableFailure(currentStatus) {
		logger.Info("Deployment no longer in retryable state, skipping retry",
			"deploymentName", deploymentName, "currentStatus", currentStatus)
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, true
	}

	// Call retry API
	retryResp, err := r.BasetenClient.RetryDeployment(ctx, modelID, deploymentID)
	if err != nil {
		logger.Error(err, "Failed to call retry API", "deploymentID", deploymentID)
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventDeploymentRetryFailed,
			"Failed to retry deployment %s: %v", deploymentName, err)
		r.scheduleNextRetry(ctx, model)
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, true
	}

	if !retryResp.Retried {
		logger.Info("Retry API declined", "deploymentName", deploymentName, "reason", retryResp.Reason)
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventDeploymentRetryFailed,
			"Retry declined for deployment %s: %s", deploymentName, retryResp.Reason)
		r.scheduleNextRetry(ctx, model)
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &result, true
	}

	logger.Info("Deployment retry initiated", "deploymentName", deploymentName, "deploymentID", deploymentID,
		"retryCount", model.Status.DeploymentRetryCount+1)
	r.Recorder.Eventf(model, corev1.EventTypeNormal, EventDeploymentRetried,
		"Retried deployment %s (was %s, attempt %d)", deploymentName, failedStatus, model.Status.DeploymentRetryCount+1)

	r.scheduleNextRetry(ctx, model)

	r.logUpdateStatus(ctx, model, statusUpdate{
		deploymentStatus: baseten.DeploymentStatusBuilding,
		message:          fmt.Sprintf("retrying deployment %s (was %s, attempt %d)", deploymentName, failedStatus, model.Status.DeploymentRetryCount),
		modelID:          modelID,
	})

	result := ctrl.Result{RequeueAfter: 30 * time.Second}
	return &result, true
}

// scheduleNextRetry increments the retry count and computes the next retry time
// using exponential backoff (2m base, doubling, capped at 30m) with 0-50% jitter.
func (r *BasetenModelReconciler) scheduleNextRetry(ctx context.Context, model *modelsv1alpha1.BasetenModel) {
	model.Status.DeploymentRetryCount++
	backoff := retryBackoff(model.Status.DeploymentRetryCount)
	nextRetry := metav1.NewTime(time.Now().Add(backoff))
	model.Status.NextRetryTime = &nextRetry
	if err := r.Status().Update(ctx, model); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update retry status")
	}
}

// retryBackoff computes the backoff duration for a given retry count.
// Formula: min(2m * 2^(count-1), 30m) + 0-50% jitter
func retryBackoff(retryCount int32) time.Duration {
	base := retryBaseInterval
	for i := int32(1); i < retryCount; i++ {
		base *= 2
		if base > retryMaxInterval {
			base = retryMaxInterval
			break
		}
	}
	jitter := time.Duration(float64(base) * retryJitterFraction * rand.Float64())
	return base + jitter
}

func (r *BasetenModelReconciler) resolveModelID(ctx context.Context, model *modelsv1alpha1.BasetenModel) (string, *ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Use cached model ID from status if available (persisted in API server across reconciles)
	if model.Status.ModelID != "" {
		logger.V(1).Info("Using cached model ID", "modelID", model.Status.ModelID)
		return model.Status.ModelID, nil, nil
	}

	modelID, err := r.BasetenClient.FindModelIDByName(ctx, model.Spec.ModelName)
	if err != nil {
		logger.Error(err, "Failed to lookup model in Baseten")
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventModelNotFound, "Failed to lookup model %q: %v", model.Spec.ModelName, err)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          fmt.Sprintf("Failed to lookup model '%s': %v", model.Spec.ModelName, err),
		})
		return "", &ctrl.Result{}, err
	}

	if modelID == "" {
		// trussConfig workflow: model will be created by truss push
		if model.Spec.TrussConfig != nil {
			logger.Info("Model not found, will be created by truss push", "modelName", model.Spec.ModelName)
			return "", nil, nil
		}
		// sourceDeploymentName workflow: model must already exist
		msg := fmt.Sprintf("model '%s' not found", model.Spec.ModelName)
		logger.Info(msg)
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventModelNotFound, "Model %q not found", model.Spec.ModelName)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          msg,
		})
		return "", &ctrl.Result{}, fmt.Errorf("model '%s' not found", model.Spec.ModelName)
	}

	logger.Info("Resolved model ID from API", "modelID", modelID)
	return modelID, nil, nil
}

func (r *BasetenModelReconciler) resolveSourceDeployment(ctx context.Context, model *modelsv1alpha1.BasetenModel, modelID string) (string, *ctrl.Result, error) {
	if model.Spec.TrussConfig != nil {
		return r.reconcileTrussDeployment(ctx, model, modelID)
	}
	return model.Spec.SourceDeploymentName, nil, nil
}

func (r *BasetenModelReconciler) reconcileTrussDeployment(ctx context.Context, model *modelsv1alpha1.BasetenModel, modelID string) (string, *ctrl.Result, error) {
	logger := log.FromContext(ctx)
	tc := model.Spec.TrussConfig

	// Use cached active deployment name from previous reconcile (env not yet available at Step 1.5)
	ap := ""
	if model.Status.ActiveDeploymentName != "" {
		ap = fmt.Sprintf("active: %s | ", model.Status.ActiveDeploymentName)
	}

	// Read setup script from ConfigMap if referenced
	setupScript, err := r.readSetupScript(ctx, tc, model.Namespace)
	if err != nil {
		logger.Error(err, "Failed to read setup script")
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventSetupScriptNotFound, "%v", err)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          fmt.Sprintf("%struss push failed: %v", ap, err),
			modelID:          modelID,
		})
		return "", &ctrl.Result{}, err
	}

	// Compute hash and deployment name
	configHash := truss.HashTrussConfig(tc, setupScript)
	deploymentName := truss.DeploymentName(configHash, tc.BaseImage.Image)

	// Only check for existing deployment if we have a modelID.
	// When modelID is empty (model doesn't exist yet), skip straight to push logic —
	// the truss-go SDK will create both the model and deployment.
	if modelID != "" {
		// Fast path: if hash unchanged and push was previously completed, trust the cache.
		// The downstream environment/promotion checks (Steps 3-5) will catch any issues
		// if the deployment was deleted from Baseten.
		if model.Status.TrussConfigHash == configHash && model.Status.TrussPushStatus == statusTrussPushDone {
			logger.V(1).Info("TrussConfig unchanged, using cached deployment", "deploymentName", model.Status.SourceDeploymentName)
			return model.Status.SourceDeploymentName, nil, nil
		}

		// Hash changed or first reconcile — verify deployment exists in Baseten
		existingID, _, err := r.BasetenClient.FindDeploymentIDByName(ctx, modelID, deploymentName)
		if err != nil {
			logger.Error(err, "Failed to check for existing deployment")
			r.logUpdateStatus(ctx, model, statusUpdate{
				deploymentStatus: statusPending,
				message:          fmt.Sprintf("%sunable to check deployment %s: %v", ap, deploymentName, err),
				modelID:          modelID,
			})
			result := ctrl.Result{RequeueAfter: 30 * time.Second}
			return "", &result, nil
		}

		if existingID != "" {
			logger.Info("Deployment exists in Baseten", "deploymentName", deploymentName)
			if model.Status.TrussPushStatus == statusTrussPushing {
				r.Recorder.Eventf(model, corev1.EventTypeNormal, EventTrussPushCompleted, "Deployment '%s' created successfully via truss push", deploymentName)
			}
			model.Status.TrussConfigHash = configHash
			model.Status.SourceDeploymentName = deploymentName
			model.Status.TrussPushStatus = statusTrussPushDone
			if err := r.Status().Update(ctx, model); err != nil {
				logger.Error(err, "Failed to update status after finding existing deployment")
			}
			return deploymentName, nil, nil
		}
	}

	// Deployment doesn't exist — need to create it.
	// If already creating (async push in flight), wait unless the push is stale.
	if model.Status.TrussPushStatus == statusTrussPushing && model.Status.SourceDeploymentName == deploymentName {
		// Check if the push has been running too long (goroutine likely died)
		if model.Status.TrussPushTime != nil && time.Since(model.Status.TrussPushTime.Time) > trussPushStaleTimeout {
			elapsed := time.Since(model.Status.TrussPushTime.Time)
			logger.Info("Stale truss push detected, clearing state to retry",
				"deploymentName", deploymentName,
				"lastPushTime", model.Status.TrussPushTime.Time,
				"elapsed", elapsed)
			r.Recorder.Eventf(model, corev1.EventTypeWarning, EventTrussPushFailed, "Truss push for '%s' timed out after %s, retrying", deploymentName, elapsed.Truncate(time.Second))
			model.Status.TrussPushStatus = ""
			model.Status.TrussPushTime = nil
			if err := r.Status().Update(ctx, model); err != nil {
				logger.Error(err, "Failed to clear stale push status")
			}
			// Fall through to launch a new push below
		} else {
			logger.Info("Deployment creation in progress, waiting", "deploymentName", deploymentName)
			r.logUpdateStatus(ctx, model, statusUpdate{
				deploymentStatus: baseten.DeploymentStatusDeploying,
				message:          fmt.Sprintf("%struss push in progress for %s", ap, deploymentName),
				modelID:          modelID,
			})
			result := ctrl.Result{RequeueAfter: 10 * time.Second}
			return "", &result, nil
		}
	}

	// Generate config.yaml
	configYAML, err := truss.GenerateConfigYAML(tc, model.Spec.ModelName)
	if err != nil {
		logger.Error(err, "Failed to generate truss config")
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventTrussPushFailed, "Failed to prepare truss push for '%s': %v", deploymentName, err)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          fmt.Sprintf("%struss push failed for %s: %v", ap, deploymentName, err),
			modelID:          modelID,
		})
		return "", &ctrl.Result{}, err
	}

	// Set status before launching async push
	logger.Info("Creating deployment via truss push", "deploymentName", deploymentName, "modelName", model.Spec.ModelName)
	now := metav1.Now()
	model.Status.TrussConfigHash = configHash
	model.Status.SourceDeploymentName = deploymentName
	model.Status.TrussPushStatus = statusTrussPushing
	model.Status.TrussPushTime = &now
	if err := r.Status().Update(ctx, model); err != nil {
		logger.Error(err, "Failed to update status before push")
	}

	// Try to acquire push semaphore — if full, requeue instead of spawning unbounded goroutines
	if r.PushSemaphore != nil {
		select {
		case r.PushSemaphore <- struct{}{}:
			// Acquired — will be released in asyncPush
		default:
			logger.Info("Push semaphore full, requeuing", "deploymentName", deploymentName)
			model.Status.TrussPushStatus = ""
			model.Status.SourceDeploymentName = ""
			if err := r.Status().Update(ctx, model); err != nil {
				logger.Error(err, "Failed to reset status after semaphore full")
			}
			r.logUpdateStatus(ctx, model, statusUpdate{
				deploymentStatus: statusPending,
				message:          fmt.Sprintf("%swaiting for push capacity for %s", ap, deploymentName),
				modelID:          modelID,
			})
			result := ctrl.Result{RequeueAfter: 10 * time.Second}
			return "", &result, nil
		}
	}

	// Launch push in background goroutine — does NOT block the reconcile loop
	if modelID == "" {
		r.Recorder.Eventf(model, corev1.EventTypeNormal, EventTrussPushStarted, "Creating model '%s' and deployment '%s' via truss push", model.Spec.ModelName, deploymentName)
	} else {
		r.Recorder.Eventf(model, corev1.EventTypeNormal, EventTrussPushStarted, "Creating deployment '%s' via truss push", deploymentName)
	}
	go r.asyncPush(model.Name, model.Namespace, configYAML, []byte(setupScript), model.Spec.ModelName, deploymentName)

	r.logUpdateStatus(ctx, model, statusUpdate{
		deploymentStatus: baseten.DeploymentStatusDeploying,
		message:          fmt.Sprintf("%struss push started for %s", ap, deploymentName),
		modelID:          modelID,
	})
	result := ctrl.Result{RequeueAfter: 10 * time.Second}
	return "", &result, nil
}

// asyncPush runs truss push in the background and writes result back to CR status.
func (r *BasetenModelReconciler) asyncPush(modelName, namespace string, configYAML, setupScript []byte, basetenModelName, deploymentName string) {
	if r.PushSemaphore != nil {
		defer func() { <-r.PushSemaphore }()
	}

	logger := log.FromContext(context.Background()).WithValues("deploymentName", deploymentName, "model", modelName)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := r.TrussPusher.PushFromConfig(ctx, configYAML, setupScript, basetenModelName, deploymentName)
	if err != nil {
		logger.Error(err, "Create deployment failed", "deploymentName", deploymentName)
		r.updatePushStatus(modelName, namespace, "", "")
		return
	}

	logger.Info("Deployment created successfully", "deploymentName", deploymentName, "deploymentID", result.DeploymentID, "modelID", result.ModelID)
	r.updatePushStatus(modelName, namespace, statusTrussPushDone, result.ModelID)
}

// updatePushStatus writes push outcome back to CR status. Empty pushStatus = failure (triggers retry).
// modelID is written to status.ModelID if non-empty (populated when truss push creates a new model).
func (r *BasetenModelReconciler) updatePushStatus(modelName, namespace, pushStatus, modelID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger := log.FromContext(ctx).WithValues("model", modelName)

	model := &modelsv1alpha1.BasetenModel{}
	if err := r.Get(ctx, client.ObjectKey{Name: modelName, Namespace: namespace}, model); err != nil {
		logger.Error(err, "Failed to fetch CR for push status update")
		return
	}

	model.Status.TrussPushStatus = pushStatus
	model.Status.TrussPushTime = nil
	if modelID != "" {
		model.Status.ModelID = modelID
	}
	if err := r.Status().Update(ctx, model); err != nil {
		logger.Error(err, "Failed to update push status", "pushStatus", pushStatus)
	}
}

// readSetupScript reads the setup script from a ConfigMap or inline spec.
func (r *BasetenModelReconciler) readSetupScript(ctx context.Context, tc *modelsv1alpha1.TrussConfig, namespace string) (string, error) {
	if tc.SetupScript != nil && tc.SetupScript.ConfigMapRef != nil {
		ref := tc.SetupScript.ConfigMapRef
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, cm); err != nil {
			return "", fmt.Errorf("ConfigMap '%s' not found: %w", ref.Name, err)
		}
		script, ok := cm.Data[ref.Key]
		if !ok {
			return "", fmt.Errorf("key '%s' not found in ConfigMap '%s'", ref.Key, ref.Name)
		}
		return script, nil
	}
	if tc.SetupScript != nil && tc.SetupScript.Inline != nil {
		return *tc.SetupScript.Inline, nil
	}
	return "", nil
}

func (r *BasetenModelReconciler) reconcileEnvironment(ctx context.Context, model *modelsv1alpha1.BasetenModel, modelID, envName string) (*baseten.Environment, *ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling environment", "environment", envName)

	env, err := r.BasetenClient.GetEnvironment(ctx, modelID, envName)
	if err != nil {
		if baseten.IsNotFoundError(err) {
			logger.Info("Creating environment with settings", "environment", envName)

			if err := r.BasetenClient.CreateEnvironment(ctx, modelID, &model.Spec.Environment); err != nil {
				logger.Error(err, "Failed to create environment")
				r.Recorder.Eventf(model, corev1.EventTypeWarning, EventEnvironmentCreateFailed, "Failed to create environment %s: %v", envName, err)
				r.logUpdateStatus(ctx, model, statusUpdate{
					deploymentStatus: baseten.DeploymentStatusFailed,
					message:          fmt.Sprintf("Failed to create environment '%s': %v", envName, err),
					modelID:          modelID,
				})
				return nil, &ctrl.Result{}, err
			}

			logger.Info("Environment created successfully", "environment", envName)
			r.Recorder.Eventf(model, corev1.EventTypeNormal, EventEnvironmentCreated, "Created environment %s with autoscaling settings", envName)
			r.logUpdateStatus(ctx, model, statusUpdate{
				deploymentStatus: statusPending,
				message:          fmt.Sprintf("Created environment %s", envName),
				modelID:          modelID,
			})
			result := ctrl.Result{RequeueAfter: 10 * time.Second}
			return nil, &result, nil
		}

		logger.Error(err, "Failed to get environment")
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          fmt.Sprintf("Failed to get environment '%s': %v", envName, err),
			modelID:          modelID,
		})
		return nil, &ctrl.Result{}, err
	}

	// Check for both autoscaling and promotion settings drift
	autoscalingDrift, autoscalingChanges := baseten.HasAutoscalingDrift(model.Spec.Environment.Autoscaling, env.AutoscalingSettings)
	promotionDrift, promotionChanges := baseten.HasPromotionSettingsDrift(model.Spec.Environment.PromotionSettings, env.PromotionSettings)

	if autoscalingDrift || promotionDrift {
		allChanges := append(autoscalingChanges, promotionChanges...)
		logger.Info("Environment settings drift detected, updating",
			"environment", envName,
			"changes", allChanges)

		activeMsg := ""
		if env.CurrentDeployment != nil {
			activeMsg = steadyStateMessage(env.CurrentDeployment, envName, env.AutoscalingSettings) + " | "
		}

		// Only pass configs that have drift (nil = skip in PATCH)
		var autoscalingConfig *modelsv1alpha1.AutoscalingConfig
		if autoscalingDrift {
			autoscalingConfig = model.Spec.Environment.Autoscaling
		}
		var promotionConfig *modelsv1alpha1.PromotionSettingsConfig
		if promotionDrift {
			promotionConfig = model.Spec.Environment.PromotionSettings
		}

		if err := r.BasetenClient.UpdateEnvironmentSettings(ctx, modelID, envName, autoscalingConfig, promotionConfig); err != nil {
			logger.Error(err, "Failed to update environment settings")
			if autoscalingDrift {
				r.Recorder.Eventf(model, corev1.EventTypeWarning, EventAutoscalingUpdateFailed, "Failed to update autoscaling for %s: %v", envName, err)
			}
			if promotionDrift {
				r.Recorder.Eventf(model, corev1.EventTypeWarning, EventPromotionSettingsUpdateFailed, "Failed to update promotion settings for %s: %v", envName, err)
			}
			r.logUpdateStatus(ctx, model, statusUpdate{
				deploymentStatus: baseten.DeploymentStatusFailed,
				message:          fmt.Sprintf("%sfailed to update settings for %s: %v", activeMsg, envName, err),
				modelID:          modelID,
			})
			return nil, &ctrl.Result{}, err
		}

		logger.Info("Environment settings updated successfully", "environment", envName)

		// Emit specific events for each type of drift
		if autoscalingDrift {
			driftMsg := fmt.Sprintf("updating autoscaling: %s", autoscalingChanges[0])
			if len(autoscalingChanges) > 1 {
				driftMsg = fmt.Sprintf("updating autoscaling: %d changes", len(autoscalingChanges))
			}
			r.Recorder.Event(model, corev1.EventTypeNormal, EventAutoscalingUpdated, driftMsg)
		}
		if promotionDrift {
			driftMsg := fmt.Sprintf("updating promotion settings: %s", promotionChanges[0])
			if len(promotionChanges) > 1 {
				driftMsg = fmt.Sprintf("updating promotion settings: %d changes", len(promotionChanges))
			}
			r.Recorder.Event(model, corev1.EventTypeNormal, EventPromotionSettingsUpdated, driftMsg)
		}

		statusMsg := fmt.Sprintf("updating settings: %d changes", len(allChanges))
		if len(allChanges) == 1 {
			statusMsg = fmt.Sprintf("updating settings: %s", allChanges[0])
		}
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: statusPending,
			message:          fmt.Sprintf("%s%s", activeMsg, statusMsg),
			modelID:          modelID,
		})
		result := ctrl.Result{RequeueAfter: 10 * time.Second}
		return nil, &result, nil
	}

	return env, nil, nil
}

func (r *BasetenModelReconciler) validateSourceDeployment(ctx context.Context, model *modelsv1alpha1.BasetenModel, modelID, sourceDeploymentName string, activeDep *baseten.Deployment) (string, *ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ap := activePrefix(activeDep)

	// Get source deployment ID and status by name (created by CI/CD via truss push, or by operator via trussConfig)
	sourceDeploymentID, sourceDeploymentStatus, err := r.BasetenClient.FindDeploymentIDByName(ctx, modelID, sourceDeploymentName)
	if err != nil {
		logger.Error(err, "Failed to lookup source deployment")
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventSourceDeploymentNotFound, "Failed to lookup source deployment %q: %v", sourceDeploymentName, err)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          fmt.Sprintf("%sunable to promote: source deployment %s not found", ap, sourceDeploymentName),
			modelID:          modelID,
		})
		return "", &ctrl.Result{}, err
	}

	if sourceDeploymentID == "" {
		logger.Info("Source deployment not found", "sourceDeploymentName", sourceDeploymentName)
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventSourceDeploymentNotFound, "Source deployment %q not found for model %q", sourceDeploymentName, model.Spec.ModelName)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusFailed,
			message:          fmt.Sprintf("%sunable to promote: source deployment %s not found", ap, sourceDeploymentName),
			modelID:          modelID,
		})
		return "", &ctrl.Result{}, fmt.Errorf("source deployment '%s' not found for model '%s'", sourceDeploymentName, model.Spec.ModelName)
	}

	logger.Info("Found source deployment in Baseten", "sourceDeploymentID", sourceDeploymentID, "sourceDeploymentName", sourceDeploymentName, "status", sourceDeploymentStatus)

	switch sourceDeploymentStatus {
	case baseten.DeploymentStatusFailed, baseten.DeploymentStatusDeployFailed, baseten.DeploymentStatusBuildFailed:
		logger.Info("Source deployment failed", "sourceDeploymentName", sourceDeploymentName, "status", sourceDeploymentStatus)
		if result, shouldReturn := r.tryRetryDeployment(ctx, model, modelID, sourceDeploymentID, sourceDeploymentName, sourceDeploymentStatus); shouldReturn {
			return "", result, nil
		}
		// Retries exhausted — terminal error
		r.Recorder.Eventf(model, corev1.EventTypeWarning, EventSourceDeploymentFailed, "Source deployment %q is in %s state (retries exhausted)", sourceDeploymentName, sourceDeploymentStatus)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: sourceDeploymentStatus,
			message:          fmt.Sprintf("%sunable to promote: source deployment %s is %s (retries exhausted after %s)", ap, sourceDeploymentName, sourceDeploymentStatus, deploymentRetryDeadline),
			modelID:          modelID,
		})
		return "", &ctrl.Result{}, reconcile.TerminalError(fmt.Errorf("source deployment '%s' is %s (retries exhausted after %s)", sourceDeploymentName, sourceDeploymentStatus, deploymentRetryDeadline))

	case baseten.DeploymentStatusInactive:
		logger.Info("Activating source deployment", "sourceDeploymentID", sourceDeploymentID, "currentStatus", sourceDeploymentStatus)
		if err := r.BasetenClient.ActivateDeployment(ctx, modelID, sourceDeploymentID); err != nil {
			logger.Error(err, "Failed to activate source deployment")
			r.logUpdateStatus(ctx, model, statusUpdate{
				deploymentStatus: baseten.DeploymentStatusFailed,
				message:          fmt.Sprintf("%sunable to promote: failed to activate source deployment %s: %v", ap, sourceDeploymentName, err),
				modelID:          modelID,
			})
			result := ctrl.Result{RequeueAfter: 30 * time.Second}
			return "", &result, nil
		}
		logger.Info("Source deployment activation initiated", "sourceDeploymentID", sourceDeploymentID)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: baseten.DeploymentStatusActivating,
			message:          fmt.Sprintf("%swaiting to promote: source deployment %s is ACTIVATING", ap, sourceDeploymentName),
			modelID:          modelID,
		})
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return "", &result, nil

	case baseten.DeploymentStatusScaledToZero:
		logger.Info("Source deployment is SCALED_TO_ZERO, proceeding with promotion")

	case baseten.DeploymentStatusDeploying, baseten.DeploymentStatusBuilding, baseten.DeploymentStatusWakingUp, baseten.DeploymentStatusActivating, baseten.DeploymentStatusUpdating:
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: sourceDeploymentStatus,
			message:          fmt.Sprintf("%swaiting to promote: source deployment %s is %s", ap, sourceDeploymentName, sourceDeploymentStatus),
			modelID:          modelID,
		})
		logger.Info("Source deployment not ready for promotion yet", "status", sourceDeploymentStatus)
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return "", &result, nil

	case baseten.DeploymentStatusActive:
		logger.Info("Source deployment is ACTIVE, proceeding with promotion")

	default:
		logger.Info("Source deployment has unknown status, waiting", "status", sourceDeploymentStatus)
		r.logUpdateStatus(ctx, model, statusUpdate{
			deploymentStatus: sourceDeploymentStatus,
			message:          fmt.Sprintf("%swaiting to promote: source deployment %s has status %s", ap, sourceDeploymentName, sourceDeploymentStatus),
			modelID:          modelID,
		})
		result := ctrl.Result{RequeueAfter: 30 * time.Second}
		return "", &result, nil
	}

	return sourceDeploymentID, nil, nil
}

// activePrefix returns "active: {name} ({replicas} replicas) | " when there's
// an active deployment, or "" when there isn't. Used to consistently prefix
// all promotion-phase messages with the current live deployment info.
func activePrefix(active *baseten.Deployment) string {
	if active == nil {
		return ""
	}
	return fmt.Sprintf("active: %s (%d replicas) | ", active.Name, active.ActiveReplicaCount)
}

func promotionMessage(active, candidate *baseten.Deployment) string {
	return fmt.Sprintf("%spromoting: %s (%s, %d replicas)",
		activePrefix(active),
		candidate.Name, candidate.Status, candidate.ActiveReplicaCount)
}

func candidateFailedMessage(active, candidate *baseten.Deployment) string {
	return fmt.Sprintf("%scandidate %s failed: %s",
		activePrefix(active),
		candidate.Name, candidate.Status)
}

func steadyStateMessage(dep *baseten.Deployment, envName string, autoscaling *baseten.AutoscalingSettings) string {
	msg := fmt.Sprintf("active: %s (%d replicas", dep.Name, dep.ActiveReplicaCount)
	if autoscaling != nil {
		msg += fmt.Sprintf(", min:%d max:%d", autoscaling.MinReplica, autoscaling.MaxReplica)
	}
	if dep.Status != baseten.DeploymentStatusActive {
		msg += fmt.Sprintf(", %s", dep.Status)
	}
	msg += fmt.Sprintf(") in %s environment", envName)
	return msg
}

func isProgressingStatus(status string) bool {
	switch status {
	case statusPromoting, statusPromotingDeployment, statusPending,
		baseten.DeploymentStatusActivating,
		baseten.DeploymentStatusBuilding,
		baseten.DeploymentStatusDeploying,
		baseten.DeploymentStatusWakingUp,
		baseten.DeploymentStatusUpdating,
		baseten.DeploymentStatusLoadingModel:
		return true
	}
	return false
}

// invalidateModelID clears the cached model ID when an API call returns a not-found error,
// so the next reconcile re-resolves it from the model name.
// Returns true if the model ID was invalidated (caller should requeue).
func (r *BasetenModelReconciler) invalidateModelID(ctx context.Context, model *modelsv1alpha1.BasetenModel, err error) bool {
	if !baseten.IsNotFoundError(err) || model.Status.ModelID == "" {
		return false
	}
	logger := log.FromContext(ctx)
	logger.Info("Invalidating cached model ID", "modelID", model.Status.ModelID)
	r.Recorder.Eventf(model, corev1.EventTypeWarning, EventModelNotFound, "Invalidating cached model ID %s, will re-resolve", model.Status.ModelID)
	model.Status.ModelID = ""
	if updateErr := r.Status().Update(ctx, model); updateErr != nil {
		logger.Error(updateErr, "Failed to invalidate model ID in status")
	}
	return true
}

func (r *BasetenModelReconciler) logUpdateStatus(ctx context.Context, model *modelsv1alpha1.BasetenModel, s statusUpdate) {
	if err := r.updateStatus(ctx, model, s); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update status")
	}
}

func (r *BasetenModelReconciler) updateStatus(ctx context.Context, model *modelsv1alpha1.BasetenModel, s statusUpdate) error {
	// Snapshot fields that determine whether an update is needed
	prev := statusSnapshot{
		deploymentStatus:        model.Status.DeploymentStatus,
		message:                 model.Status.Message,
		activeReplicaCount:      model.Status.ActiveReplicaCount,
		modelID:                 model.Status.ModelID,
		sourceDeploymentID:      model.Status.SourceDeploymentID,
		sourceDeploymentName:    model.Status.SourceDeploymentName,
		activeDeploymentName:    model.Status.ActiveDeploymentName,
		candidateDeploymentID:   model.Status.CandidateDeploymentID,
		candidateDeploymentName: model.Status.CandidateDeploymentName,
	}

	model.Status.DeploymentStatus = s.deploymentStatus
	model.Status.Message = s.message
	model.Status.ActiveReplicaCount = s.replicaCount

	if s.modelID != "" {
		model.Status.ModelID = s.modelID
	}
	if s.sourceDeploymentID != "" {
		model.Status.SourceDeploymentID = s.sourceDeploymentID
	}
	if s.sourceDeploymentName != "" {
		model.Status.SourceDeploymentName = s.sourceDeploymentName
	}

	if s.activeDeploymentName != "" {
		model.Status.ActiveDeploymentName = s.activeDeploymentName
	}
	if s.clearCandidate {
		model.Status.CandidateDeploymentID = ""
		model.Status.CandidateDeploymentName = ""
	} else {
		if s.candidateDeploymentID != "" {
			model.Status.CandidateDeploymentID = s.candidateDeploymentID
		}
		if s.candidateDeploymentName != "" {
			model.Status.CandidateDeploymentName = s.candidateDeploymentName
		}
	}

	// Auto-clear deployment retry tracking on healthy status
	if s.deploymentStatus == baseten.DeploymentStatusActive || s.deploymentStatus == baseten.DeploymentStatusScaledToZero {
		model.Status.FirstDeploymentFailureTime = nil
		model.Status.DeploymentRetryCount = 0
		model.Status.NextRetryTime = nil
	} else if s.firstDeploymentFailureTime != nil {
		model.Status.FirstDeploymentFailureTime = s.firstDeploymentFailureTime
	}

	readyStatus := metav1.ConditionFalse
	if s.deploymentStatus == baseten.DeploymentStatusActive || s.deploymentStatus == baseten.DeploymentStatusScaledToZero {
		readyStatus = metav1.ConditionTrue
	}
	setCondition(&model.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             readyStatus,
		ObservedGeneration: model.Generation,
		Reason:             s.deploymentStatus,
		Message:            s.message,
	})

	progressingStatus := metav1.ConditionFalse
	if isProgressingStatus(s.deploymentStatus) {
		progressingStatus = metav1.ConditionTrue
	}
	setCondition(&model.Status.Conditions, metav1.Condition{
		Type:               conditionProgressing,
		Status:             progressingStatus,
		ObservedGeneration: model.Generation,
		Reason:             s.deploymentStatus,
		Message:            s.message,
	})

	// Skip the API call if nothing meaningful changed (avoids triggering a watch event → extra reconcile)
	curr := statusSnapshot{
		deploymentStatus:        model.Status.DeploymentStatus,
		message:                 model.Status.Message,
		activeReplicaCount:      model.Status.ActiveReplicaCount,
		modelID:                 model.Status.ModelID,
		sourceDeploymentID:      model.Status.SourceDeploymentID,
		sourceDeploymentName:    model.Status.SourceDeploymentName,
		activeDeploymentName:    model.Status.ActiveDeploymentName,
		candidateDeploymentID:   model.Status.CandidateDeploymentID,
		candidateDeploymentName: model.Status.CandidateDeploymentName,
	}
	if curr == prev && !conditionsChanged(model.Status.Conditions, s.deploymentStatus, readyStatus, progressingStatus) {
		return nil
	}

	return r.Status().Update(ctx, model)
}

type statusSnapshot struct {
	deploymentStatus        string
	message                 string
	activeReplicaCount      int32
	modelID                 string
	sourceDeploymentID      string
	sourceDeploymentName    string
	activeDeploymentName    string
	candidateDeploymentID   string
	candidateDeploymentName string
}

func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == condition.Type {
			// Only update LastTransitionTime when the status actually changes (K8s convention)
			if c.Status == condition.Status {
				condition.LastTransitionTime = c.LastTransitionTime
			} else {
				condition.LastTransitionTime = metav1.Now()
			}
			(*conditions)[i] = condition
			return
		}
	}
	condition.LastTransitionTime = metav1.Now()
	*conditions = append(*conditions, condition)
}

// conditionsChanged checks if the condition Reason or Status differs from what's already set.
func conditionsChanged(conditions []metav1.Condition, reason string, readyStatus, progressingStatus metav1.ConditionStatus) bool {
	for _, c := range conditions {
		switch c.Type {
		case conditionReady:
			if c.Status != readyStatus || c.Reason != reason {
				return true
			}
		case conditionProgressing:
			if c.Status != progressingStatus || c.Reason != reason {
				return true
			}
		}
	}
	return false
}

func (r *BasetenModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&modelsv1alpha1.BasetenModel{}).
		Named("basetenmodel").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}
