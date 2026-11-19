package controller

import (
	"context"
	"fmt"
	"time"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	ctrl "sigs.k8s.io/controller-runtime"
)

const statusPromotionCanceled = "PROMOTION_CANCELED"

func (r *BasetenModelReconciler) reconcilePromotionCancellation(ctx context.Context, model *modelsv1alpha1.BasetenModel) (ctrl.Result, error) {
	request := model.Spec.PromotionCancellation
	pending := func(message string) (ctrl.Result, error) {
		err := r.updateStatus(ctx, model, statusUpdate{deploymentStatus: statusPending, message: message})
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	blocked := func(message string) (ctrl.Result, error) {
		err := r.updateStatus(ctx, model, statusUpdate{deploymentStatus: baseten.DeploymentStatusFailed, message: "promotion cancellation blocked: " + message})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if request.ModelID == "" || request.ModelID != model.Status.ModelID || request.EnvironmentName != model.Spec.Environment.Name || request.CandidateDeploymentID == "" {
		return blocked("model identity changed or is unknown")
	}
	if model.Status.TrussPushStatus == statusTrussPushing {
		return pending("waiting for the in-flight source push to settle")
	}
	if effectiveMode(model) != modelsv1alpha1.ModeObserve && request.CandidateDeploymentID != model.Status.CandidateDeploymentID {
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
		if effectiveMode(model) == modelsv1alpha1.ModeObserve || env.PromotionStatus == "RAMPING_DOWN" {
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
	// Cancellation does not choose replica settings. Reuse ordinary environment reconciliation
	// only after the promotion clears; observers never write those settings.
	if effectiveMode(model) != modelsv1alpha1.ModeObserve {
		var result *ctrl.Result
		env, result, err = r.reconcileEnvironment(ctx, model, request.ModelID, request.EnvironmentName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if result != nil {
			return *result, nil
		}
	}
	err = r.updateStatus(ctx, model, statusUpdate{deploymentStatus: statusPromotionCanceled, replicaCount: cancellationReplicaCount(env),
		message: fmt.Sprintf("promotion %s canceled in %s; further promotions suppressed while the request remains", request.CandidateDeploymentID, env.Name)})
	return ctrl.Result{RequeueAfter: 30 * time.Second}, err
}

func cancellationReplicaCount(env *baseten.Environment) int32 {
	if env.CurrentDeployment != nil {
		return env.CurrentDeployment.ActiveReplicaCount
	}
	return 0
}
