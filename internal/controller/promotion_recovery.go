package controller

import (
	"context"
	"fmt"
	"time"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	ctrl "sigs.k8s.io/controller-runtime"
)

const statusPromotionRecovered = "PROMOTION_RECOVERED"

func (r *BasetenModelReconciler) reconcilePromotionRecovery(ctx context.Context, model *modelsv1alpha1.BasetenModel) (ctrl.Result, error) {
	request := model.Spec.PromotionRecovery
	pending := func(message string) (ctrl.Result, error) {
		err := r.updateStatus(ctx, model, statusUpdate{deploymentStatus: statusPending, message: message})
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	blocked := func(message string) (ctrl.Result, error) {
		err := r.updateStatus(ctx, model, statusUpdate{deploymentStatus: baseten.DeploymentStatusFailed, message: "promotion recovery blocked: " + message})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if model.Spec.Mode != modelsv1alpha1.ModePause || model.Spec.Environment.Autoscaling == nil || model.Spec.Environment.Autoscaling.MinReplicas == nil || *model.Spec.Environment.Autoscaling.MinReplicas != 0 {
		return blocked("requires Pause and an unused environment with minimum zero")
	}
	if request.ModelID == "" || request.ModelID != model.Status.ModelID || request.EnvironmentName != model.Spec.Environment.Name || (!request.ObserveOnly && request.CandidateDeploymentID == "") {
		return blocked("model identity changed or is unknown")
	}
	if model.Status.TrussPushStatus == statusTrussPushing {
		return pending("waiting for the in-flight source push to settle")
	}
	if !request.ObserveOnly && request.CandidateDeploymentID != model.Status.CandidateDeploymentID {
		return blocked("candidate is not the promotion recorded by this controller")
	}
	env, err := r.BasetenClient.GetEnvironment(ctx, request.ModelID, model.Spec.Environment.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if env.CurrentDeployment != nil && env.CurrentDeployment.ID == request.CandidateDeploymentID {
		return blocked("the candidate is now current; serving dependencies must be reassessed")
	}
	if env.CandidateDeployment != nil || env.PromotionStatus != "" {
		if request.ObserveOnly || env.PromotionStatus == "RAMPING_DOWN" {
			return pending("waiting for Baseten or the owning region to finish canceling the promotion")
		}
		if env.CandidateDeployment == nil || env.CandidateDeployment.ID != request.CandidateDeploymentID {
			return blocked("the environment's candidate changed or cannot be identified")
		}
		// Never delete or deactivate the model, its source, or a serving predecessor.
		if err := r.BasetenClient.CancelPromotion(ctx, request.ModelID, request.EnvironmentName); err != nil {
			return ctrl.Result{}, err
		}
		return pending("waiting for Baseten to finish canceling the abandoned promotion")
	}
	if env.ScheduleDecodeErr != nil {
		return blocked("environment autoscaling schedules cannot be decoded")
	}
	if env.AutoscalingSettings == nil {
		return blocked("environment autoscaling is unknown")
	}
	if baseten.ScheduleActive(env) {
		return blocked("an autoscaling schedule owns the environment floor")
	}
	if env.AutoscalingSettings.MinReplica != 0 {
		if request.ObserveOnly {
			return pending("waiting for the owning region to floor the environment")
		}
		zero := int32(0)
		if err := r.BasetenClient.UpdateEnvironmentSettings(ctx, request.ModelID, request.EnvironmentName, &modelsv1alpha1.AutoscalingConfig{MinReplicas: &zero}, nil); err != nil {
			return ctrl.Result{}, err
		}
		return pending("waiting for Baseten to confirm the environment floor")
	}
	if !model.DeletionTimestamp.IsZero() {
		if model.Spec.DeletionPolicy != modelsv1alpha1.DeletionPolicyRetain {
			return blocked("recovered resources must retain the shared Baseten model on deletion")
		}
		return r.releaseFinalizer(ctx, model)
	}
	err = r.updateStatus(ctx, model, statusUpdate{deploymentStatus: statusPromotionRecovered, replicaCount: recoveryReplicaCount(env),
		message: fmt.Sprintf("promotion abandoned; %s floor is zero; reconciliation remains paused", env.Name)})
	return ctrl.Result{RequeueAfter: 30 * time.Second}, err
}

func recoveryReplicaCount(env *baseten.Environment) int32 {
	if env.CurrentDeployment != nil {
		return env.CurrentDeployment.ActiveReplicaCount
	}
	return 0
}
