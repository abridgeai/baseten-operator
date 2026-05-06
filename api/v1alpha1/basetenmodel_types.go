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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// FinalizerName is the finalizer added to BasetenModel resources so the operator
// can perform cleanup before the resource is removed.
const FinalizerName = "models.baseten.com/finalizer"

// DeletionPolicy controls what happens to the upstream Baseten model when the
// BasetenModel CR is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain leaves the Baseten model intact when the CR is deleted (default).
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete removes the Baseten model when the CR is deleted.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// BasetenModelSpec defines the desired state of BasetenModel.
// Exactly one of sourceDeploymentName or trussConfig must be specified.
// +kubebuilder:validation:XValidation:rule="has(self.sourceDeploymentName) || has(self.trussConfig)",message="one of sourceDeploymentName or trussConfig must be specified"
// +kubebuilder:validation:XValidation:rule="!(has(self.sourceDeploymentName) && has(self.trussConfig))",message="sourceDeploymentName and trussConfig are mutually exclusive"
type BasetenModelSpec struct {
	// ModelName references an existing Baseten model
	// +required
	// +kubebuilder:validation:MinLength=1
	ModelName string `json:"modelName"`

	// SourceDeploymentName is the deployment to promote (created by CI/CD via truss push).
	// Mutually exclusive with trussConfig.
	// Format: img-{container-img-version}-wgt-{model-weight-version}-p-{platform-config-version}
	// +optional
	SourceDeploymentName string `json:"sourceDeploymentName,omitempty"`

	// TrussConfig specifies the truss configuration for the operator to create a deployment
	// via truss push. The operator generates a config.yaml, pushes to Baseten, then promotes
	// the resulting deployment to the target environment.
	// Mutually exclusive with sourceDeploymentName.
	// +optional
	TrussConfig *TrussConfig `json:"trussConfig,omitempty"`

	// Environment defines the target environment and its configuration
	// +required
	Environment EnvironmentConfig `json:"environment"`

	// OrphanDeploymentCleanup configures automatic cleanup of orphan deployments.
	// Orphan deployments are those NOT actively used by any environment and NOT
	// matching the current sourceDeploymentName.
	// +optional
	OrphanDeploymentCleanup *OrphanDeploymentCleanupConfig `json:"orphanDeploymentCleanup,omitempty"`

	// Paused stops the controller from reconciling this resource.
	// Use this during emergencies or click-ops to prevent the operator from making
	// changes while you configure manually in the Baseten UI. When paused, no API
	// calls are made and the last known status is preserved in the message.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// DeletionPolicy controls cleanup of the upstream Baseten model when this CR is deleted.
	// "Retain" (default): the Baseten model is left untouched; the CR vanishes.
	// "Delete": the operator calls DELETE /v1/models/{model_id} before allowing the CR to be removed,
	// which cascades to all deployments, environments, and promotion history under that model.
	// +optional
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// TrussConfig defines the truss configuration for creating a deployment.
// Fields map to the truss config.yaml format (https://docs.baseten.co/reference/truss-configuration).
type TrussConfig struct {
	// PythonVersion is the Python version to build with (e.g., "py311", "py312")
	// +optional
	PythonVersion string `json:"pythonVersion,omitempty"`

	// Resources defines compute resources for the deployment
	// +required
	Resources TrussResources `json:"resources"`

	// Secrets defines secret keys available to the deployment.
	// Values should be empty strings (actual values are configured in Baseten).
	// +optional
	Secrets map[string]string `json:"secrets,omitempty"`

	// EnvironmentVariables defines environment variables for the deployment.
	// Do not store secrets here — use the secrets field instead.
	// +optional
	EnvironmentVariables map[string]string `json:"environmentVariables,omitempty"`

	// BaseImage specifies the Docker base image for the deployment
	// +required
	BaseImage TrussBaseImage `json:"baseImage"`

	// DockerServer configures the custom server running in the container
	// +optional
	DockerServer *TrussDockerServer `json:"dockerServer,omitempty"`

	// Runtime configures runtime behavior
	// +optional
	Runtime *TrussRuntime `json:"runtime,omitempty"`

	// ModelMetadata provides metadata about the model
	// +optional
	ModelMetadata *TrussModelMetadata `json:"modelMetadata,omitempty"`

	// SetupScript references a setup/startup script for the deployment.
	// Large scripts should be stored in a ConfigMap and referenced via configMapRef.
	// +optional
	SetupScript *SetupScriptSource `json:"setupScript,omitempty"`
}

// TrussResources defines compute resources for the deployment
type TrussResources struct {
	// Accelerator specifies GPU type and count (e.g., "H100:2", "A100:4", "L4")
	// +required
	Accelerator string `json:"accelerator"`

	// UseGpu enables GPU support
	// +optional
	UseGpu *bool `json:"useGpu,omitempty"`
}

// TrussBaseImage specifies the Docker base image
type TrussBaseImage struct {
	// Image is the Docker image URI (e.g., "us-docker.pkg.dev/.../vllm:0.11.2.1")
	// +required
	Image string `json:"image"`

	// DockerAuth configures authentication for pulling the base image
	// +optional
	DockerAuth *TrussDockerAuth `json:"dockerAuth,omitempty"`
}

// TrussDockerAuth configures Docker registry authentication
type TrussDockerAuth struct {
	// AuthMethod is the authentication method (e.g., "GCP_SERVICE_ACCOUNT_JSON")
	// +required
	AuthMethod string `json:"authMethod"`

	// SecretName is the Baseten secret name containing credentials
	// +required
	SecretName string `json:"secretName"`

	// Registry is the Docker registry hostname (e.g., "us-docker.pkg.dev")
	// +required
	Registry string `json:"registry"`
}

// TrussDockerServer configures the custom server in the container
type TrussDockerServer struct {
	// NoBuild skips the image build step when using a pre-built base image.
	// Set to true when baseImage already contains everything needed.
	// +optional
	NoBuild *bool `json:"noBuild,omitempty"`

	// StartCommand is the command to start the server
	// +optional
	StartCommand string `json:"startCommand,omitempty"`

	// ReadinessEndpoint is the HTTP path for readiness checks
	// +optional
	ReadinessEndpoint string `json:"readinessEndpoint,omitempty"`

	// LivenessEndpoint is the HTTP path for liveness checks
	// +optional
	LivenessEndpoint string `json:"livenessEndpoint,omitempty"`

	// PredictEndpoint is the HTTP path for prediction requests
	// +optional
	PredictEndpoint string `json:"predictEndpoint,omitempty"`

	// ServerPort is the port the server listens on (note: 8080 is reserved by Baseten)
	// +optional
	ServerPort *int32 `json:"serverPort,omitempty"`
}

// TrussRuntime configures runtime behavior
type TrussRuntime struct {
	// PredictConcurrency is the maximum number of concurrent prediction requests
	// +optional
	PredictConcurrency *int32 `json:"predictConcurrency,omitempty"`
}

// TrussModelMetadata provides metadata about the model
type TrussModelMetadata struct {
	// ExampleModelInput provides a sample input for the Baseten model testing UI.
	// Accepts arbitrary JSON (e.g., {"prompt": "hello", "max_tokens": 128}).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	ExampleModelInput *runtime.RawExtension `json:"exampleModelInput,omitempty"`

	// Tags are labels for the model (e.g., "openai-compatible")
	// +optional
	Tags []string `json:"tags,omitempty"`
}

// SetupScriptSource defines where to load the setup script from
type SetupScriptSource struct {
	// ConfigMapRef references a key in a ConfigMap containing the script
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`

	// Inline contains the script content directly (for small scripts only)
	// +optional
	Inline *string `json:"inline,omitempty"`
}

// ConfigMapKeyRef references a specific key in a ConfigMap
type ConfigMapKeyRef struct {
	// Name is the ConfigMap name
	// +required
	Name string `json:"name"`

	// Key is the key in the ConfigMap's data
	// +required
	Key string `json:"key"`
}

// OrphanDeploymentCleanupConfig defines the cleanup policy for orphan deployments.
// Orphan deployments are those NOT serving in any environment: not the current or
// candidate deployment of any environment, and not matching spec.sourceDeploymentName.
type OrphanDeploymentCleanupConfig struct {
	// ScaleToZero sets min_replica=0 on orphan deployments to release GPU resources.
	// +optional
	ScaleToZero *bool `json:"scaleToZero,omitempty"`

	// Delete enables deletion of orphan deployments older than DeleteAfterDays.
	// +optional
	Delete *bool `json:"delete,omitempty"`

	// DeleteAfterDays is the minimum age in days before an orphan deployment can be deleted.
	// Required when Delete is true. Cleanup skips deletion if not set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	DeleteAfterDays *int32 `json:"deleteAfterDays,omitempty"`

	// MinToKeep is the minimum number of orphan deployments to preserve regardless of age.
	// Sorted by created_at descending (newest kept first).
	// Required when Delete is true. Cleanup skips deletion if not set.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinToKeep *int32 `json:"minToKeep,omitempty"`

	// IntervalMinutes is how often cleanup runs (in minutes).
	// Required. Cleanup is skipped entirely if not set.
	// +optional
	// +kubebuilder:validation:Minimum=10
	IntervalMinutes *int32 `json:"intervalMinutes,omitempty"`
}

// EnvironmentConfig defines the environment and its management settings
type EnvironmentConfig struct {
	// Name is the environment name (e.g., dev, staging, production)
	// Must be lowercase alphanumeric characters and hyphens only
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// Autoscaling configures autoscaling behavior for this environment
	// Maps to Baseten API: autoscaling_settings
	// +optional
	Autoscaling *AutoscalingConfig `json:"autoscaling,omitempty"`

	// PromotionSettings configures deployment promotion behavior for this environment
	// Maps to Baseten API: promotion_settings (create) and promote operation flags
	// +optional
	PromotionSettings *PromotionSettingsConfig `json:"promotionSettings,omitempty"`
}

// AutoscalingConfig defines autoscaling parameters
// Maps to Baseten API: UpdateAutoscalingSettingsV1
type AutoscalingConfig struct {
	// MinReplicas is the minimum number of replicas
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// ConcurrencyTarget is the number of requests per replica before scaling up
	// +optional
	// +kubebuilder:validation:Minimum=1
	ConcurrencyTarget *int32 `json:"concurrencyTarget,omitempty"`

	// AutoscalingWindow is the timeframe of traffic considered for autoscaling decisions (in seconds)
	// +optional
	// +kubebuilder:validation:Minimum=0
	AutoscalingWindow *int32 `json:"autoscalingWindow,omitempty"`

	// ScaleDownDelay is the waiting period before scaling down any active replica (in seconds)
	// +optional
	// +kubebuilder:validation:Minimum=0
	ScaleDownDelay *int32 `json:"scaleDownDelay,omitempty"`

	// TargetUtilizationPercentage is the target utilization percentage for scaling decisions
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetUtilizationPercentage *int32 `json:"targetUtilizationPercentage,omitempty"`
}

// PromotionSettingsConfig defines promotion behavior settings
// Maps to Baseten API: UpdatePromotionSettingsV1 and PromoteToEnvironmentRequestV1
type PromotionSettingsConfig struct {
	// ScaleDownPreviousDeployment scales down the previous deployment when promoting.
	// Promote-time only: passed to POST /promote, NOT stored on the environment.
	// Maps to Baseten API promote operation: scale_down_previous_deployment
	// +optional
	ScaleDownPreviousDeployment *bool `json:"scaleDownPreviousDeployment,omitempty"`

	// PreserveInstanceType preserves the environment's instance type when promoting.
	// Promote-time only: passed to POST /promote, NOT stored on the environment.
	// Maps to Baseten API promote operation: preserve_env_instance_type
	// +optional
	PreserveInstanceType *bool `json:"preserveInstanceType,omitempty"`

	// RedeployOnPromotion creates a new deployment on promotion with a copy of the image
	// Maps to Baseten API: redeploy_on_promotion
	// +optional
	RedeployOnPromotion *bool `json:"redeployOnPromotion,omitempty"`

	// RollingDeploy enables rolling deploy orchestration for the environment
	// Maps to Baseten API: rolling_deploy
	// +optional
	RollingDeploy *bool `json:"rollingDeploy,omitempty"`

	// PromotionCleanupStrategy is the cleanup strategy after a promotion completes
	// Maps to Baseten API: promotion_cleanup_strategy (top-level in promotion_settings)
	// +optional
	// +kubebuilder:validation:Enum=KEEP;SCALE_TO_ZERO;DEACTIVATE
	PromotionCleanupStrategy *string `json:"promotionCleanupStrategy,omitempty"`

	// RampUpWhilePromoting enables traffic ramp-up (canary) during promotion
	// Maps to Baseten API: ramp_up_while_promoting
	// +optional
	RampUpWhilePromoting *bool `json:"rampUpWhilePromoting,omitempty"`

	// RampUpDurationSeconds is the duration of traffic ramp-up in seconds
	// Maps to Baseten API: ramp_up_duration_seconds
	// +optional
	// +kubebuilder:validation:Minimum=0
	RampUpDurationSeconds *int32 `json:"rampUpDurationSeconds,omitempty"`

	// RollingDeployConfig configures rolling deployment behavior
	// Maps to Baseten API: rolling_deploy_config (UpdateRollingDeployConfigV1)
	// +optional
	RollingDeployConfig *RollingDeployConfig `json:"rollingDeployConfig,omitempty"`
}

// RollingDeployConfig defines rolling deployment configuration
// Maps to Baseten API: UpdateRollingDeployConfigV1
type RollingDeployConfig struct {
	// Strategy is the rolling deploy strategy
	// Maps to Baseten API: rolling_deploy_strategy
	// +optional
	// +kubebuilder:validation:Enum=REPLICA
	Strategy *string `json:"strategy,omitempty"`

	// MaxSurgePercent is the maximum surge percentage for rolling deploys
	// Maps to Baseten API: max_surge_percent
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxSurgePercent *int32 `json:"maxSurgePercent,omitempty"`

	// MaxUnavailablePercent is the maximum unavailable percentage for rolling deploys
	// Maps to Baseten API: max_unavailable_percent
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxUnavailablePercent *int32 `json:"maxUnavailablePercent,omitempty"`

	// StabilizationTimeSeconds is the stabilization time in seconds for rolling deploys
	// Maps to Baseten API: stabilization_time_seconds
	// +optional
	// +kubebuilder:validation:Minimum=0
	StabilizationTimeSeconds *int32 `json:"stabilizationTimeSeconds,omitempty"`
}

// BasetenModelStatus defines the observed state of BasetenModel
type BasetenModelStatus struct {
	// ModelID is the Baseten model ID
	// +optional
	ModelID string `json:"modelID,omitempty"`

	// SourceDeploymentID is the resolved Baseten deployment ID for the source deployment
	// Cached after lookup to avoid repeated API calls and to surface errors
	// +optional
	SourceDeploymentID string `json:"sourceDeploymentID,omitempty"`

	// SourceDeploymentName is the source deployment name that was resolved
	// Mirrors spec.sourceDeploymentName to confirm what the controller actually resolved
	// +optional
	SourceDeploymentName string `json:"sourceDeploymentName,omitempty"`

	// ActiveDeploymentName is the name of the currently live deployment in the environment
	// Populated from the environment's current_deployment.name on each reconcile
	// +optional
	ActiveDeploymentName string `json:"activeDeploymentName,omitempty"`

	// CandidateDeploymentID is the ID of the deployment being promoted (double-promote guard)
	// Set after calling Promote(), cleared when promotion completes (current matches prefix)
	// +optional
	CandidateDeploymentID string `json:"candidateDeploymentID,omitempty"`

	// CandidateDeploymentName is the name of the deployment being promoted
	// Populated from the environment's candidate_deployment.name on each reconcile
	// +optional
	CandidateDeploymentName string `json:"candidateDeploymentName,omitempty"`

	// DeploymentStatus represents the most relevant deployment status
	// Shows candidate status during promotion, otherwise active deployment status
	// +optional
	DeploymentStatus string `json:"deploymentStatus,omitempty"`

	// ActiveReplicaCount is the number of active replicas for the deployment
	// +optional
	ActiveReplicaCount int32 `json:"activeReplicaCount"`

	// Message provides human-readable information about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// TrussConfigHash is the hash of the trussConfig + setup script content.
	// Used for change detection — a new hash triggers a new truss push.
	// +optional
	TrussConfigHash string `json:"trussConfigHash,omitempty"`

	// TrussPushStatus tracks the internal state of the truss push operation.
	// Values: TRUSS_PUSHING (async push in flight), TRUSS_PUSH_DONE (deployment created)
	// +optional
	TrussPushStatus string `json:"trussPushStatus,omitempty"`

	// TrussPushTime is the last time a truss push was initiated.
	// Used to detect stale TRUSS_PUSHING state and retry after timeout.
	// +optional
	TrussPushTime *metav1.Time `json:"trussPushTime,omitempty"`

	// LastCleanupTime is the last time orphan deployment cleanup was executed
	// +optional
	LastCleanupTime *metav1.Time `json:"lastCleanupTime,omitempty"`

	// FirstDeploymentFailureTime is when the current deployment first entered a retryable
	// failure state (FAILED, DEPLOY_FAILED, BUILD_FAILED). Used to enforce the 2-hour
	// retry deadline — after which the operator stops retrying and returns a terminal error.
	// Automatically cleared when the deployment becomes ACTIVE or SCALED_TO_ZERO.
	// +optional
	FirstDeploymentFailureTime *metav1.Time `json:"firstDeploymentFailureTime,omitempty"`

	// DeploymentRetryCount tracks the number of retry attempts for the current failure.
	// Used to compute exponential backoff intervals. Cleared on ACTIVE or SCALED_TO_ZERO.
	// +optional
	DeploymentRetryCount int32 `json:"deploymentRetryCount,omitempty"`

	// NextRetryTime is the earliest time the next retry attempt is allowed.
	// Computed as exponential backoff (2m base, doubling, capped at 30m) with 0-50% jitter.
	// Cleared on ACTIVE or SCALED_TO_ZERO.
	// +optional
	NextRetryTime *metav1.Time `json:"nextRetryTime,omitempty"`

	// Conditions represent the current state of the BasetenModel resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bm
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelName`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environment.name`
// +kubebuilder:printcolumn:name="Active_Deployment",type=string,JSONPath=`.status.activeDeploymentName`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.activeReplicaCount`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.deploymentStatus`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Source_Deployment",type=string,JSONPath=`.spec.sourceDeploymentName`
// +kubebuilder:printcolumn:name="Candidate_Deployment",type=string,JSONPath=`.status.candidateDeploymentName`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// BasetenModel is the Schema for the basetenmodels API
type BasetenModel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BasetenModel
	// +required
	Spec BasetenModelSpec `json:"spec"`

	// status defines the observed state of BasetenModel
	// +optional
	Status BasetenModelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BasetenModelList contains a list of BasetenModel
type BasetenModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BasetenModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BasetenModel{}, &BasetenModelList{})
}
