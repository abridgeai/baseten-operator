package baseten

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
)

const (
	defaultBaseURL = "https://api.baseten.co/v1"
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

// ClientInterface defines the contract for interacting with the Baseten API.
type ClientInterface interface {
	FindModelIDByName(ctx context.Context, modelName string) (string, error)
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

// Client is a REST API client for Baseten.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
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
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout:   defaultTimeout,
			Transport: transport,
		},
	}, nil
}

// Model represents a Baseten model
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ModelsResponse represents the response from listing models
type ModelsResponse struct {
	Models []Model `json:"models"`
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

// DeploymentsResponse represents the response from listing deployments
type DeploymentsResponse struct {
	Deployments []Deployment `json:"deployments"`
}

// DeploymentDetailsResponse represents the response from listing deployments with full details
type DeploymentDetailsResponse struct {
	Deployments []DeploymentDetail `json:"deployments"`
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

// EnvironmentsResponse represents the response from listing environments
type EnvironmentsResponse struct {
	Environments []Environment `json:"environments"`
}

// CreateEnvironmentRequest represents the request to create an environment
type CreateEnvironmentRequest struct {
	Name                string                 `json:"name"`
	AutoscalingSettings map[string]interface{} `json:"autoscaling_settings,omitempty"`
	PromotionSettings   map[string]interface{} `json:"promotion_settings,omitempty"`
}

// PromoteRequest represents the request to promote a deployment
type PromoteRequest struct {
	DeploymentID                string `json:"deployment_id"`
	ScaleDownPreviousDeployment bool   `json:"scale_down_previous_deployment"`
	PreserveEnvInstanceType     bool   `json:"preserve_env_instance_type"`
}

func (c *Client) newRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Api-Key "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Message: string(body)}
	}

	return resp, nil
}

// FindModelIDByName lists all models (GET /v1/models) and returns the ID matching modelName.
func (c *Client) FindModelIDByName(ctx context.Context, modelName string) (string, error) {
	req, err := c.newRequest(ctx, "GET", c.baseURL+"/models", nil)
	if err != nil {
		return "", err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var modelsResp ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	for _, model := range modelsResp.Models {
		if model.Name == modelName {
			return model.ID, nil
		}
	}

	return "", nil
}

// FindDeploymentIDByName lists all deployments (GET /v1/models/{id}/deployments) and returns the ID and status matching deploymentName.
func (c *Client) FindDeploymentIDByName(ctx context.Context, modelID, deploymentName string) (string, string, error) {
	req, err := c.newRequest(ctx, "GET", fmt.Sprintf("%s/models/%s/deployments", c.baseURL, modelID), nil)
	if err != nil {
		return "", "", err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var deploymentsResp DeploymentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&deploymentsResp); err != nil {
		return "", "", fmt.Errorf("failed to decode response: %w", err)
	}

	for _, deployment := range deploymentsResp.Deployments {
		if deployment.Name == deploymentName {
			return deployment.ID, deployment.Status, nil
		}
	}

	return "", "", nil
}

func (c *Client) ActivateDeployment(ctx context.Context, modelID, deploymentID string) error {
	req, err := c.newRequest(ctx, "POST", fmt.Sprintf("%s/models/%s/deployments/%s/activate", c.baseURL, modelID, deploymentID), nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

func (c *Client) GetEnvironment(ctx context.Context, modelID, envName string) (*Environment, error) {
	req, err := c.newRequest(ctx, "GET", fmt.Sprintf("%s/models/%s/environments/%s", c.baseURL, modelID, envName), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var env Environment
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &env, nil
}

func (c *Client) ListEnvironments(ctx context.Context, modelID string) ([]Environment, error) {
	req, err := c.newRequest(ctx, "GET", fmt.Sprintf("%s/models/%s/environments", c.baseURL, modelID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var envsResp EnvironmentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&envsResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return envsResp.Environments, nil
}

func (c *Client) CreateEnvironment(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error {
	reqBody := CreateEnvironmentRequest{
		Name:                envConfig.Name,
		AutoscalingSettings: convertAutoscalingConfig(envConfig.Autoscaling),
		PromotionSettings:   convertPromotionSettings(envConfig.PromotionSettings),
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := c.newRequest(ctx, "POST", fmt.Sprintf("%s/models/%s/environments", c.baseURL, modelID), bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

func (c *Client) UpdateEnvironmentSettings(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
	reqBody := map[string]interface{}{}
	if autoscalingConfig != nil {
		if settings := convertAutoscalingConfig(autoscalingConfig); settings != nil {
			reqBody["autoscaling_settings"] = settings
		}
	}
	if promotionConfig != nil {
		if settings := convertPromotionSettings(promotionConfig); settings != nil {
			reqBody["promotion_settings"] = settings
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := c.newRequest(ctx, "PATCH", fmt.Sprintf("%s/models/%s/environments/%s", c.baseURL, modelID, envName), bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
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

	reqBody := PromoteRequest{
		DeploymentID:                deploymentID,
		ScaleDownPreviousDeployment: scaleDownPrevious,
		PreserveEnvInstanceType:     preserveInstanceType,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := c.newRequest(ctx, "POST", fmt.Sprintf("%s/models/%s/environments/%s/promote", c.baseURL, modelID, targetEnv), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var promotedDeployment Deployment
	if err := json.NewDecoder(resp.Body).Decode(&promotedDeployment); err != nil {
		return nil, fmt.Errorf("failed to decode promotion response: %w", err)
	}

	return &promotedDeployment, nil
}

func (c *Client) ListDeployments(ctx context.Context, modelID string) ([]DeploymentDetail, error) {
	req, err := c.newRequest(ctx, "GET", fmt.Sprintf("%s/models/%s/deployments", c.baseURL, modelID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var deploymentsResp DeploymentDetailsResponse
	if err := json.NewDecoder(resp.Body).Decode(&deploymentsResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return deploymentsResp.Deployments, nil
}

func (c *Client) UpdateDeploymentAutoscaling(ctx context.Context, modelID, deploymentID string, minReplica int32) error {
	reqBody := map[string]interface{}{
		"min_replica": minReplica,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := c.newRequest(ctx, "PATCH", fmt.Sprintf("%s/models/%s/deployments/%s/autoscaling_settings", c.baseURL, modelID, deploymentID), bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

func (c *Client) DeleteDeployment(ctx context.Context, modelID, deploymentID string) error {
	req, err := c.newRequest(ctx, "DELETE", fmt.Sprintf("%s/models/%s/deployments/%s", c.baseURL, modelID, deploymentID), nil)
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

func (c *Client) RetryDeployment(ctx context.Context, modelID, deploymentID string) (*RetryResponse, error) {
	req, err := c.newRequest(ctx, "POST", fmt.Sprintf("%s/models/%s/deployments/%s/retry", c.baseURL, modelID, deploymentID), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var retryResp RetryResponse
	if err := json.NewDecoder(resp.Body).Decode(&retryResp); err != nil {
		return nil, fmt.Errorf("failed to decode retry response: %w", err)
	}
	return &retryResp, nil
}

// convertAutoscalingConfig converts CRD autoscaling config to API format
func convertAutoscalingConfig(config *modelsv1alpha1.AutoscalingConfig) map[string]interface{} {
	if config == nil {
		return nil
	}

	result := make(map[string]interface{})

	if config.MinReplicas != nil {
		result["min_replica"] = *config.MinReplicas
	}
	if config.MaxReplicas != nil {
		result["max_replica"] = *config.MaxReplicas
	}
	if config.ConcurrencyTarget != nil {
		result["concurrency_target"] = *config.ConcurrencyTarget
	}
	if config.AutoscalingWindow != nil {
		result["autoscaling_window"] = *config.AutoscalingWindow
	}
	if config.ScaleDownDelay != nil {
		result["scale_down_delay"] = *config.ScaleDownDelay
	}
	if config.TargetUtilizationPercentage != nil {
		result["target_utilization_percentage"] = *config.TargetUtilizationPercentage
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// convertPromotionSettings converts CRD promotion settings to API format (UpdatePromotionSettingsV1).
// Used for environment creation (POST). promotion_cleanup_strategy is a top-level field in the API,
// sourced from spec.rollingDeployConfig.promotionCleanupStrategy in the CRD.
func convertPromotionSettings(config *modelsv1alpha1.PromotionSettingsConfig) map[string]interface{} {
	if config == nil {
		return nil
	}

	result := make(map[string]interface{})

	if config.RedeployOnPromotion != nil {
		result["redeploy_on_promotion"] = *config.RedeployOnPromotion
	}
	if config.RollingDeploy != nil {
		result["rolling_deploy"] = *config.RollingDeploy
	}
	if config.RampUpWhilePromoting != nil {
		result["ramp_up_while_promoting"] = *config.RampUpWhilePromoting
	}
	if config.RampUpDurationSeconds != nil {
		result["ramp_up_duration_seconds"] = *config.RampUpDurationSeconds
	}

	if config.PromotionCleanupStrategy != nil {
		result["promotion_cleanup_strategy"] = *config.PromotionCleanupStrategy
	}

	// Convert rolling deploy config if present
	if config.RollingDeployConfig != nil {
		rollingConfig := convertRollingDeployConfig(config.RollingDeployConfig)
		if rollingConfig != nil {
			result["rolling_deploy_config"] = rollingConfig
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// convertRollingDeployConfig converts CRD rolling deploy config to API format (UpdateRollingDeployConfigV1).
func convertRollingDeployConfig(config *modelsv1alpha1.RollingDeployConfig) map[string]interface{} {
	if config == nil {
		return nil
	}

	result := make(map[string]interface{})

	if config.Strategy != nil {
		result["rolling_deploy_strategy"] = *config.Strategy
	}
	if config.MaxSurgePercent != nil {
		result["max_surge_percent"] = *config.MaxSurgePercent
	}
	if config.MaxUnavailablePercent != nil {
		result["max_unavailable_percent"] = *config.MaxUnavailablePercent
	}
	if config.StabilizationTimeSeconds != nil {
		result["stabilization_time_seconds"] = *config.StabilizationTimeSeconds
	}

	if len(result) == 0 {
		return nil
	}
	return result
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
