package baseten

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	managementapi "github.com/basetenlabs/baseten-go/client/managementapi"
)

const (
	// Host root only; the generated client appends the "/v1/..." path segments itself.
	defaultBaseURL = "https://api.baseten.co"
	defaultTimeout = 30 * time.Second
	apiKeyEnvVar   = "BASETEN_API_KEY"

	// Deployment status constants
	DeploymentStatusActive       = "ACTIVE"
	DeploymentStatusFailed       = "FAILED"
	DeploymentStatusScaledToZero = "SCALED_TO_ZERO"
	DeploymentStatusInactive     = "INACTIVE"
	DeploymentStatusDeploying    = "DEPLOYING"
	DeploymentStatusBuilding     = "BUILDING"
	DeploymentStatusWakingUp     = "WAKING_UP"
	DeploymentStatusActivating   = "ACTIVATING"
	DeploymentStatusUpdating     = "UPDATING"
	DeploymentStatusDeployFailed = "DEPLOY_FAILED"
	DeploymentStatusLoadingModel = "LOADING_MODEL"
	DeploymentStatusUnhealthy    = "UNHEALTHY"
	DeploymentStatusBuildFailed  = "BUILD_FAILED"
	DeploymentStatusBuildStopped = "BUILD_STOPPED"
	DeploymentStatusDeactivating = "DEACTIVATING"
)

// APIError represents an error response from the Baseten API with status code information.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API request failed with status %d: %s", e.StatusCode, e.Message)
}

// IsNotFoundError checks if an error is a 404 Not Found error from the Baseten API.
func IsNotFoundError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// toAPIError maps a generated-client HTTP failure (*managementapi.ResponseError) to *APIError
// so IsNotFoundError keeps working; other errors (network, decode) pass through unchanged.
func toAPIError(err error) error {
	if err == nil {
		return nil
	}
	var respErr *managementapi.ResponseError
	if errors.As(err, &respErr) {
		return &APIError{StatusCode: respErr.StatusCode, Message: respErr.Body}
	}
	return err
}

// ClientInterface defines the contract for interacting with the Baseten API.
type ClientInterface interface {
	FindModelIDByName(ctx context.Context, modelName string) (string, error)
	DeleteModel(ctx context.Context, modelID string) error
	GetEnvironment(ctx context.Context, modelID, envName string) (*Environment, error)
	ListEnvironments(ctx context.Context, modelID string) ([]Environment, error)
	CreateEnvironment(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error
	UpdateEnvironmentSettings(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error
	FindDeploymentIDByName(ctx context.Context, modelID, deploymentName string) (string, string, error)
	ActivateDeployment(ctx context.Context, modelID, deploymentID string) error
	Promote(ctx context.Context, modelID, deploymentID, targetEnv string, settings *modelsv1alpha1.PromotionSettingsConfig) (*Deployment, error)
	ListDeployments(ctx context.Context, modelID string) ([]DeploymentDetail, error)
	UpdateDeploymentAutoscaling(ctx context.Context, modelID, deploymentID string, minReplica int32) error
	DeleteDeployment(ctx context.Context, modelID, deploymentID string) error
	RetryDeployment(ctx context.Context, modelID, deploymentID string) (*RetryResponse, error)
}

// Client is a Baseten API client backed by the generated baseten-go management SDK.
type Client struct {
	api *managementapi.Client
}

var _ ClientInterface = (*Client)(nil)

func NewClient() (*Client, error) {
	apiKey := os.Getenv(apiKeyEnvVar)
	if apiKey == "" {
		return nil, fmt.Errorf("%s environment variable not set", apiKeyEnvVar)
	}

	baseURL := os.Getenv("BASETEN_API_BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Client{
		api: &managementapi.Client{
			BaseURL: baseURL,
			HTTPClient: &http.Client{
				Timeout:   defaultTimeout,
				Transport: transport,
			},
			Headers: http.Header{"Authorization": {"Api-Key " + apiKey}},
		},
	}, nil
}

// Deployment represents a Baseten deployment
type Deployment struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Status             string `json:"status"` // ACTIVE, BUILDING, FAILED, etc.
	ActiveReplicaCount int32  `json:"active_replica_count"`
}

// DeploymentDetail has the full fields from the list deployments API
type DeploymentDetail struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	Status              string               `json:"status"`
	ActiveReplicaCount  int32                `json:"active_replica_count"`
	CreatedAt           string               `json:"created_at"`
	IsProduction        bool                 `json:"is_production"`
	IsDevelopment       bool                 `json:"is_development"`
	Environment         *string              `json:"environment"`
	AutoscalingSettings *AutoscalingSettings `json:"autoscaling_settings"`
}

// AutoscalingSettings represents the autoscaling configuration from API response
type AutoscalingSettings struct {
	MinReplica                  int32  `json:"min_replica"`
	MaxReplica                  int32  `json:"max_replica"`
	ConcurrencyTarget           int32  `json:"concurrency_target"`
	AutoscalingWindow           *int32 `json:"autoscaling_window"`
	ScaleDownDelay              *int32 `json:"scale_down_delay"`
	TargetUtilizationPercentage *int32 `json:"target_utilization_percentage"`
}

// PromotionSettings represents the promotion configuration from API response (PromotionSettingsV1).
// Note: scale_down_previous_deployment and preserve_env_instance_type are promote-time-only
// parameters (POST /promote), NOT environment-level settings returned by GET /environments.
type PromotionSettings struct {
	RedeployOnPromotion      *bool                  `json:"redeploy_on_promotion"`
	RollingDeploy            *bool                  `json:"rolling_deploy"`
	PromotionCleanupStrategy *string                `json:"promotion_cleanup_strategy"`
	RampUpWhilePromoting     *bool                  `json:"ramp_up_while_promoting"`
	RampUpDurationSeconds    *int32                 `json:"ramp_up_duration_seconds"`
	RollingDeployConfig      *RollingDeploySettings `json:"rolling_deploy_config"`
}

// RollingDeploySettings represents the rolling deploy configuration from API response (RollingDeployConfigV1).
type RollingDeploySettings struct {
	Strategy                 *string `json:"rolling_deploy_strategy"`
	MaxSurgePercent          *int32  `json:"max_surge_percent"`
	MaxUnavailablePercent    *int32  `json:"max_unavailable_percent"`
	StabilizationTimeSeconds *int32  `json:"stabilization_time_seconds"`
}

// Environment represents a Baseten environment with its current deployment
type Environment struct {
	Name                string               `json:"name"`
	CurrentDeployment   *Deployment          `json:"current_deployment,omitempty"`
	CandidateDeployment *Deployment          `json:"candidate_deployment,omitempty"`
	AutoscalingSettings *AutoscalingSettings `json:"autoscaling_settings,omitempty"`
	PromotionSettings   *PromotionSettings   `json:"promotion_settings,omitempty"`
}

// FindModelIDByName lists all models and returns the ID matching modelName ("" if not found).
func (c *Client) FindModelIDByName(ctx context.Context, modelName string) (string, error) {
	models, err := c.api.GetModels(ctx)
	if err != nil {
		return "", toAPIError(err)
	}
	for _, m := range models.Models {
		if m.Name == modelName {
			return m.Id, nil
		}
	}
	return "", nil
}

// DeleteModel deletes a Baseten model and cascades to all deployments and environments under it.
func (c *Client) DeleteModel(ctx context.Context, modelID string) error {
	_, err := c.api.DeleteModels(ctx, modelID)
	return toAPIError(err)
}

// FindDeploymentIDByName lists deployments and returns the ID and status matching deploymentName.
func (c *Client) FindDeploymentIDByName(ctx context.Context, modelID, deploymentName string) (string, string, error) {
	deps, err := c.api.GetModelsDeployments(ctx, modelID)
	if err != nil {
		return "", "", toAPIError(err)
	}
	for _, d := range deps.Deployments {
		if d.Name == deploymentName {
			return d.Id, string(d.Status), nil
		}
	}
	return "", "", nil
}

func (c *Client) ActivateDeployment(ctx context.Context, modelID, deploymentID string) error {
	_, err := c.api.PostModelsDeploymentsActivate(ctx, modelID, deploymentID)
	return toAPIError(err)
}

func (c *Client) GetEnvironment(ctx context.Context, modelID, envName string) (*Environment, error) {
	env, err := c.api.GetModelsEnvironmentsEnvName(ctx, modelID, envName)
	if err != nil {
		return nil, toAPIError(err)
	}
	return toEnvironment(env), nil
}

func (c *Client) ListEnvironments(ctx context.Context, modelID string) ([]Environment, error) {
	envs, err := c.api.GetModelsEnvironments(ctx, modelID)
	if err != nil {
		return nil, toAPIError(err)
	}
	result := make([]Environment, 0, len(envs.Environments))
	for i := range envs.Environments {
		result = append(result, *toEnvironment(&envs.Environments[i]))
	}
	return result, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error {
	_, err := c.api.PostModelsEnvironments(ctx, modelID, managementapi.CreateEnvironmentRequest{
		Name:                envConfig.Name,
		AutoscalingSettings: toUpdateAutoscalingSettings(envConfig.Autoscaling),
		PromotionSettings:   toUpdatePromotionSettings(envConfig.PromotionSettings),
	})
	return toAPIError(err)
}

func (c *Client) UpdateEnvironmentSettings(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
	_, err := c.api.PatchModelsEnvironments(ctx, modelID, envName, managementapi.UpdateEnvironmentRequest{
		AutoscalingSettings: toUpdateAutoscalingSettings(autoscalingConfig),
		PromotionSettings:   toUpdatePromotionSettings(promotionConfig),
	})
	return toAPIError(err)
}

func (c *Client) Promote(ctx context.Context, modelID, deploymentID, targetEnv string, promotionSettings *modelsv1alpha1.PromotionSettingsConfig) (*Deployment, error) {
	scaleDownPrevious := true
	preserveInstanceType := true

	if promotionSettings != nil {
		if promotionSettings.ScaleDownPreviousDeployment != nil {
			scaleDownPrevious = *promotionSettings.ScaleDownPreviousDeployment
		}
		if promotionSettings.PreserveInstanceType != nil {
			preserveInstanceType = *promotionSettings.PreserveInstanceType
		}
	}

	dep, err := c.api.PostModelsEnvironmentsPromote(ctx, modelID, targetEnv, managementapi.PromoteToEnvironmentRequest{
		DeploymentId:                deploymentID,
		ScaleDownPreviousDeployment: &scaleDownPrevious,
		PreserveEnvInstanceType:     &preserveInstanceType,
	})
	if err != nil {
		return nil, toAPIError(err)
	}
	return toDeployment(dep), nil
}

func (c *Client) ListDeployments(ctx context.Context, modelID string) ([]DeploymentDetail, error) {
	deps, err := c.api.GetModelsDeployments(ctx, modelID)
	if err != nil {
		return nil, toAPIError(err)
	}
	result := make([]DeploymentDetail, 0, len(deps.Deployments))
	for _, d := range deps.Deployments {
		result = append(result, toDeploymentDetail(d))
	}
	return result, nil
}

func (c *Client) UpdateDeploymentAutoscaling(ctx context.Context, modelID, deploymentID string, minReplica int32) error {
	mr := int(minReplica)
	_, err := c.api.PatchModelsDeploymentsAutoscalingSettings(ctx, modelID, deploymentID, managementapi.UpdateAutoscalingSettings{
		MinReplica: &mr,
	})
	return toAPIError(err)
}

func (c *Client) DeleteDeployment(ctx context.Context, modelID, deploymentID string) error {
	_, err := c.api.DeleteModelsDeployments(ctx, modelID, deploymentID)
	return toAPIError(err)
}

func (c *Client) RetryDeployment(ctx context.Context, modelID, deploymentID string) (*RetryResponse, error) {
	resp, err := c.api.PostModelsDeploymentsRetry(ctx, modelID, deploymentID)
	if err != nil {
		return nil, toAPIError(err)
	}
	out := &RetryResponse{
		Retried:    resp.Retried,
		Deployment: toDeploymentPtr(resp.Deployment),
	}
	if resp.Reason != nil {
		out.Reason = *resp.Reason
	}
	return out, nil
}

func toDeployment(d *managementapi.Deployment) *Deployment {
	if d == nil {
		return nil
	}
	return &Deployment{
		ID:                 d.Id,
		Name:               d.Name,
		Status:             string(d.Status),
		ActiveReplicaCount: int32(d.ActiveReplicaCount),
	}
}

// toDeploymentPtr returns nil for an empty (absent) deployment, matching the operator's
// nil-means-no-deployment semantics.
func toDeploymentPtr(d managementapi.Deployment) *Deployment {
	if d.Id == "" {
		return nil
	}
	return toDeployment(&d)
}

func toDeploymentDetail(d managementapi.Deployment) DeploymentDetail {
	return DeploymentDetail{
		ID:                  d.Id,
		Name:                d.Name,
		Status:              string(d.Status),
		ActiveReplicaCount:  int32(d.ActiveReplicaCount),
		CreatedAt:           d.CreatedAt.Format(time.RFC3339),
		IsProduction:        d.IsProduction,
		IsDevelopment:       d.IsDevelopment,
		Environment:         d.Environment,
		AutoscalingSettings: toAutoscalingSettings(d.AutoscalingSettings),
	}
}

func toEnvironment(e *managementapi.Environment) *Environment {
	return &Environment{
		Name:                e.Name,
		CurrentDeployment:   toDeploymentPtr(e.CurrentDeployment),
		CandidateDeployment: toDeployment(e.CandidateDeployment),
		AutoscalingSettings: toAutoscalingSettings(e.AutoscalingSettings),
		PromotionSettings:   toPromotionSettings(e.PromotionSettings),
	}
}

func toAutoscalingSettings(a managementapi.AutoscalingSettings) *AutoscalingSettings {
	return &AutoscalingSettings{
		MinReplica:                  int32(a.MinReplica),
		MaxReplica:                  int32(a.MaxReplica),
		ConcurrencyTarget:           int32(a.ConcurrencyTarget),
		AutoscalingWindow:           intPtrToInt32Ptr(a.AutoscalingWindow),
		ScaleDownDelay:              intPtrToInt32Ptr(a.ScaleDownDelay),
		TargetUtilizationPercentage: intPtrToInt32Ptr(a.TargetUtilizationPercentage),
	}
}

func toPromotionSettings(p managementapi.PromotionSettings) *PromotionSettings {
	out := &PromotionSettings{
		RedeployOnPromotion:   p.RedeployOnPromotion,
		RollingDeploy:         p.RollingDeploy,
		RampUpWhilePromoting:  p.RampUpWhilePromoting,
		RampUpDurationSeconds: intPtrToInt32Ptr(p.RampUpDurationSeconds),
	}
	if p.PromotionCleanupStrategy != nil {
		s := string(*p.PromotionCleanupStrategy)
		out.PromotionCleanupStrategy = &s
	}
	if p.RollingDeployConfig != nil {
		out.RollingDeployConfig = toRollingDeploySettings(p.RollingDeployConfig)
	}
	return out
}

func toRollingDeploySettings(rc *managementapi.RollingDeployConfig) *RollingDeploySettings {
	out := &RollingDeploySettings{
		MaxSurgePercent:          intPtrToInt32Ptr(rc.MaxSurgePercent),
		MaxUnavailablePercent:    intPtrToInt32Ptr(rc.MaxUnavailablePercent),
		StabilizationTimeSeconds: intPtrToInt32Ptr(rc.StabilizationTimeSeconds),
	}
	if rc.RollingDeployStrategy != nil {
		s := string(*rc.RollingDeployStrategy)
		out.Strategy = &s
	}
	return out
}

// The builders below return nil when no fields are set, so the settings object is omitted
// from the request body rather than sent as an empty object.
func toUpdateAutoscalingSettings(c *modelsv1alpha1.AutoscalingConfig) *managementapi.UpdateAutoscalingSettings {
	if c == nil {
		return nil
	}
	out := &managementapi.UpdateAutoscalingSettings{}
	set := false
	if c.MinReplicas != nil {
		out.MinReplica = int32PtrToIntPtr(c.MinReplicas)
		set = true
	}
	if c.MaxReplicas != nil {
		out.MaxReplica = int32PtrToIntPtr(c.MaxReplicas)
		set = true
	}
	if c.ConcurrencyTarget != nil {
		out.ConcurrencyTarget = int32PtrToIntPtr(c.ConcurrencyTarget)
		set = true
	}
	if c.AutoscalingWindow != nil {
		out.AutoscalingWindow = int32PtrToIntPtr(c.AutoscalingWindow)
		set = true
	}
	if c.ScaleDownDelay != nil {
		out.ScaleDownDelay = int32PtrToIntPtr(c.ScaleDownDelay)
		set = true
	}
	if c.TargetUtilizationPercentage != nil {
		out.TargetUtilizationPercentage = int32PtrToIntPtr(c.TargetUtilizationPercentage)
		set = true
	}
	if !set {
		return nil
	}
	return out
}

func toUpdatePromotionSettings(c *modelsv1alpha1.PromotionSettingsConfig) *managementapi.UpdatePromotionSettings {
	if c == nil {
		return nil
	}
	out := &managementapi.UpdatePromotionSettings{}
	set := false
	if c.RedeployOnPromotion != nil {
		out.RedeployOnPromotion = c.RedeployOnPromotion
		set = true
	}
	if c.RollingDeploy != nil {
		out.RollingDeploy = c.RollingDeploy
		set = true
	}
	if c.RampUpWhilePromoting != nil {
		out.RampUpWhilePromoting = c.RampUpWhilePromoting
		set = true
	}
	if c.RampUpDurationSeconds != nil {
		out.RampUpDurationSeconds = int32PtrToIntPtr(c.RampUpDurationSeconds)
		set = true
	}
	if c.PromotionCleanupStrategy != nil {
		s := managementapi.PromotionCleanupStrategy(*c.PromotionCleanupStrategy)
		out.PromotionCleanupStrategy = &s
		set = true
	}
	if c.RollingDeployConfig != nil {
		if rc := toUpdateRollingDeployConfig(c.RollingDeployConfig); rc != nil {
			out.RollingDeployConfig = rc
			set = true
		}
	}
	if !set {
		return nil
	}
	return out
}

func toUpdateRollingDeployConfig(c *modelsv1alpha1.RollingDeployConfig) *managementapi.UpdateRollingDeployConfig {
	if c == nil {
		return nil
	}
	out := &managementapi.UpdateRollingDeployConfig{}
	set := false
	if c.Strategy != nil {
		s := managementapi.RollingDeployStrategy(*c.Strategy)
		out.RollingDeployStrategy = &s
		set = true
	}
	if c.MaxSurgePercent != nil {
		out.MaxSurgePercent = int32PtrToIntPtr(c.MaxSurgePercent)
		set = true
	}
	if c.MaxUnavailablePercent != nil {
		out.MaxUnavailablePercent = int32PtrToIntPtr(c.MaxUnavailablePercent)
		set = true
	}
	if c.StabilizationTimeSeconds != nil {
		out.StabilizationTimeSeconds = int32PtrToIntPtr(c.StabilizationTimeSeconds)
		set = true
	}
	if !set {
		return nil
	}
	return out
}

func intPtrToInt32Ptr(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

func int32PtrToIntPtr(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// DeploymentNameMatchesPrefix checks if a deployment name matches the given prefix,
// accounting for Baseten's timestamp suffix (e.g., "img-1.0-wgt-1.0-p-1.2.1768269232")
func DeploymentNameMatchesPrefix(deploymentName, prefix string) bool {
	return deploymentName == prefix || strings.HasPrefix(deploymentName, prefix+".")
}

// IsTerminalFailure returns true for deployment statuses that indicate a terminal failure
func IsTerminalFailure(status string) bool {
	switch status {
	case DeploymentStatusFailed, DeploymentStatusDeployFailed, DeploymentStatusBuildFailed, DeploymentStatusBuildStopped:
		return true
	}
	return false
}

// IsRetryableFailure returns true for deployment statuses that can be retried via the retry API.
// BUILD_STOPPED is excluded because it indicates an intentional user action.
func IsRetryableFailure(status string) bool {
	switch status {
	case DeploymentStatusFailed, DeploymentStatusDeployFailed, DeploymentStatusBuildFailed:
		return true
	}
	return false
}

// RetryResponse represents the response from the Baseten retry deployment API.
type RetryResponse struct {
	Retried    bool        `json:"retried"`
	Reason     string      `json:"reason"`
	Deployment *Deployment `json:"deployment"`
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// HasAutoscalingDrift compares spec autoscaling config with environment settings and returns drift details.
// Only fields explicitly set in spec (non-nil) are compared.
func HasAutoscalingDrift(spec *modelsv1alpha1.AutoscalingConfig, env *AutoscalingSettings) (bool, []string) {
	if spec == nil || env == nil {
		return false, nil
	}

	var drifts []string

	if spec.MinReplicas != nil && *spec.MinReplicas != env.MinReplica {
		drifts = append(drifts, fmt.Sprintf("minReplicas %d→%d", env.MinReplica, *spec.MinReplicas))
	}
	if spec.MaxReplicas != nil && *spec.MaxReplicas != env.MaxReplica {
		drifts = append(drifts, fmt.Sprintf("maxReplicas %d→%d", env.MaxReplica, *spec.MaxReplicas))
	}
	if spec.ConcurrencyTarget != nil && *spec.ConcurrencyTarget != env.ConcurrencyTarget {
		drifts = append(drifts, fmt.Sprintf("concurrencyTarget %d→%d", env.ConcurrencyTarget, *spec.ConcurrencyTarget))
	}
	if spec.AutoscalingWindow != nil && *spec.AutoscalingWindow != derefInt32(env.AutoscalingWindow) {
		drifts = append(drifts, fmt.Sprintf("autoscalingWindow %d→%d", derefInt32(env.AutoscalingWindow), *spec.AutoscalingWindow))
	}
	if spec.ScaleDownDelay != nil && *spec.ScaleDownDelay != derefInt32(env.ScaleDownDelay) {
		drifts = append(drifts, fmt.Sprintf("scaleDownDelay %d→%d", derefInt32(env.ScaleDownDelay), *spec.ScaleDownDelay))
	}
	if spec.TargetUtilizationPercentage != nil && *spec.TargetUtilizationPercentage != derefInt32(env.TargetUtilizationPercentage) {
		drifts = append(drifts, fmt.Sprintf("targetUtilization %d→%d", derefInt32(env.TargetUtilizationPercentage), *spec.TargetUtilizationPercentage))
	}

	return len(drifts) > 0, drifts
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// HasPromotionSettingsDrift compares spec promotion settings with environment settings and returns drift details.
// Only fields explicitly set in spec (non-nil) are compared.
// Note: ScaleDownPreviousDeployment and PreserveInstanceType are promote-time-only flags
// (passed to POST /promote), NOT environment-level settings — they are not compared here.
func HasPromotionSettingsDrift(spec *modelsv1alpha1.PromotionSettingsConfig, env *PromotionSettings) (bool, []string) {
	if spec == nil {
		return false, nil
	}
	// If env has no promotion settings yet, any non-nil spec field is drift
	if env == nil {
		env = &PromotionSettings{}
	}

	var drifts []string

	if spec.RedeployOnPromotion != nil && *spec.RedeployOnPromotion != derefBool(env.RedeployOnPromotion) {
		drifts = append(drifts, fmt.Sprintf("redeployOnPromotion %v→%v", derefBool(env.RedeployOnPromotion), *spec.RedeployOnPromotion))
	}
	if spec.RollingDeploy != nil && *spec.RollingDeploy != derefBool(env.RollingDeploy) {
		drifts = append(drifts, fmt.Sprintf("rollingDeploy %v→%v", derefBool(env.RollingDeploy), *spec.RollingDeploy))
	}
	if spec.RampUpWhilePromoting != nil && *spec.RampUpWhilePromoting != derefBool(env.RampUpWhilePromoting) {
		drifts = append(drifts, fmt.Sprintf("rampUpWhilePromoting %v→%v", derefBool(env.RampUpWhilePromoting), *spec.RampUpWhilePromoting))
	}
	if spec.RampUpDurationSeconds != nil && *spec.RampUpDurationSeconds != derefInt32(env.RampUpDurationSeconds) {
		drifts = append(drifts, fmt.Sprintf("rampUpDurationSeconds %d→%d", derefInt32(env.RampUpDurationSeconds), *spec.RampUpDurationSeconds))
	}

	if spec.PromotionCleanupStrategy != nil && *spec.PromotionCleanupStrategy != derefString(env.PromotionCleanupStrategy) {
		drifts = append(drifts, fmt.Sprintf("promotionCleanupStrategy %s→%s", derefString(env.PromotionCleanupStrategy), *spec.PromotionCleanupStrategy))
	}

	// Check nested rolling deploy config fields
	if spec.RollingDeployConfig != nil && env.RollingDeployConfig != nil {
		rc := spec.RollingDeployConfig
		erc := env.RollingDeployConfig
		if rc.Strategy != nil && *rc.Strategy != derefString(erc.Strategy) {
			drifts = append(drifts, fmt.Sprintf("rollingDeployStrategy %s→%s", derefString(erc.Strategy), *rc.Strategy))
		}
		if rc.MaxSurgePercent != nil && *rc.MaxSurgePercent != derefInt32(erc.MaxSurgePercent) {
			drifts = append(drifts, fmt.Sprintf("maxSurgePercent %d→%d", derefInt32(erc.MaxSurgePercent), *rc.MaxSurgePercent))
		}
		if rc.MaxUnavailablePercent != nil && *rc.MaxUnavailablePercent != derefInt32(erc.MaxUnavailablePercent) {
			drifts = append(drifts, fmt.Sprintf("maxUnavailablePercent %d→%d", derefInt32(erc.MaxUnavailablePercent), *rc.MaxUnavailablePercent))
		}
		if rc.StabilizationTimeSeconds != nil && *rc.StabilizationTimeSeconds != derefInt32(erc.StabilizationTimeSeconds) {
			drifts = append(drifts, fmt.Sprintf("stabilizationTimeSeconds %d→%d", derefInt32(erc.StabilizationTimeSeconds), *rc.StabilizationTimeSeconds))
		}
	} else if spec.RollingDeployConfig != nil {
		// env has no rolling deploy config — any non-nil spec field is drift
		rc := spec.RollingDeployConfig
		if rc.Strategy != nil {
			drifts = append(drifts, fmt.Sprintf("rollingDeployStrategy →%s", *rc.Strategy))
		}
		if rc.MaxSurgePercent != nil {
			drifts = append(drifts, fmt.Sprintf("maxSurgePercent →%d", *rc.MaxSurgePercent))
		}
		if rc.MaxUnavailablePercent != nil {
			drifts = append(drifts, fmt.Sprintf("maxUnavailablePercent →%d", *rc.MaxUnavailablePercent))
		}
		if rc.StabilizationTimeSeconds != nil {
			drifts = append(drifts, fmt.Sprintf("stabilizationTimeSeconds →%d", *rc.StabilizationTimeSeconds))
		}
	}

	return len(drifts) > 0, drifts
}
