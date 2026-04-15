package controller

// Event reason constants for Kubernetes events emitted by the controller.
const (
	// Normal events
	EventDeploymentActive          = "DeploymentActive"
	EventDeploymentPromoted        = "DeploymentPromoted"
	EventEnvironmentCreated        = "EnvironmentCreated"
	EventAutoscalingUpdated        = "AutoscalingUpdated"
	EventPromotionSettingsUpdated  = "PromotionSettingsUpdated"
	EventOrphanDeploymentsScaledIn = "OrphanDeploymentsScaledIn"
	EventOrphanDeploymentsDeleted  = "OrphanDeploymentsDeleted"
	EventTrussPushStarted          = "TrussPushStarted"
	EventTrussPushCompleted        = "TrussPushCompleted"
	EventReconciliationPaused      = "ReconciliationPaused"
	EventDeploymentRetried         = "DeploymentRetried"

	// Warning events
	EventModelNotFound                 = "ModelNotFound"
	EventEnvironmentCreateFailed       = "EnvironmentCreateFailed"
	EventAutoscalingUpdateFailed       = "AutoscalingUpdateFailed"
	EventPromotionSettingsUpdateFailed = "PromotionSettingsUpdateFailed"
	EventSetupScriptNotFound           = "SetupScriptNotFound"
	EventTrussPushFailed               = "TrussPushFailed"
	EventSourceDeploymentNotFound      = "SourceDeploymentNotFound"
	EventSourceDeploymentFailed        = "SourceDeploymentFailed"
	EventPromotionFailed               = "PromotionFailed"
	EventPromotionBlocked              = "PromotionBlocked"
	EventDeploymentRetryFailed         = "DeploymentRetryFailed"
	EventDeploymentRetryExhausted      = "DeploymentRetryExhausted"
)
