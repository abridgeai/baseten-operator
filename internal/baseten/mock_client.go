package baseten

import (
	"context"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
)

type MockClient struct {
	FindModelIDByNameFunc           func(ctx context.Context, modelName string) (string, error)
	DeleteModelFunc                 func(ctx context.Context, modelID string) error
	GetEnvironmentFunc              func(ctx context.Context, modelID, envName string) (*Environment, error)
	ListEnvironmentsFunc            func(ctx context.Context, modelID string) ([]Environment, error)
	CreateEnvironmentFunc           func(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error
	UpdateEnvironmentSettingsFunc   func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error
	FindDeploymentIDByNameFunc      func(ctx context.Context, modelID, deploymentName string) (string, string, error)
	ActivateDeploymentFunc          func(ctx context.Context, modelID, deploymentID string) error
	PromoteFunc                     func(ctx context.Context, modelID, deploymentID, targetEnv string, settings *modelsv1alpha1.PromotionSettingsConfig) (*Deployment, error)
	ListDeploymentsFunc             func(ctx context.Context, modelID string) ([]DeploymentDetail, error)
	UpdateDeploymentAutoscalingFunc func(ctx context.Context, modelID, deploymentID string, minReplica int32) error
	DeleteDeploymentFunc            func(ctx context.Context, modelID, deploymentID string) error
	RetryDeploymentFunc             func(ctx context.Context, modelID, deploymentID string) (*RetryResponse, error)
}

var _ ClientInterface = (*MockClient)(nil)

func (m *MockClient) FindModelIDByName(ctx context.Context, modelName string) (string, error) {
	return m.FindModelIDByNameFunc(ctx, modelName)
}

func (m *MockClient) DeleteModel(ctx context.Context, modelID string) error {
	return m.DeleteModelFunc(ctx, modelID)
}

func (m *MockClient) GetEnvironment(ctx context.Context, modelID, envName string) (*Environment, error) {
	return m.GetEnvironmentFunc(ctx, modelID, envName)
}

func (m *MockClient) CreateEnvironment(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error {
	return m.CreateEnvironmentFunc(ctx, modelID, envConfig)
}

func (m *MockClient) UpdateEnvironmentSettings(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
	return m.UpdateEnvironmentSettingsFunc(ctx, modelID, envName, autoscalingConfig, promotionConfig)
}

func (m *MockClient) FindDeploymentIDByName(ctx context.Context, modelID, deploymentName string) (string, string, error) {
	return m.FindDeploymentIDByNameFunc(ctx, modelID, deploymentName)
}

func (m *MockClient) ActivateDeployment(ctx context.Context, modelID, deploymentID string) error {
	return m.ActivateDeploymentFunc(ctx, modelID, deploymentID)
}

func (m *MockClient) Promote(ctx context.Context, modelID, deploymentID, targetEnv string, settings *modelsv1alpha1.PromotionSettingsConfig) (*Deployment, error) {
	return m.PromoteFunc(ctx, modelID, deploymentID, targetEnv, settings)
}

func (m *MockClient) ListEnvironments(ctx context.Context, modelID string) ([]Environment, error) {
	return m.ListEnvironmentsFunc(ctx, modelID)
}

func (m *MockClient) ListDeployments(ctx context.Context, modelID string) ([]DeploymentDetail, error) {
	return m.ListDeploymentsFunc(ctx, modelID)
}

func (m *MockClient) UpdateDeploymentAutoscaling(ctx context.Context, modelID, deploymentID string, minReplica int32) error {
	return m.UpdateDeploymentAutoscalingFunc(ctx, modelID, deploymentID, minReplica)
}

func (m *MockClient) DeleteDeployment(ctx context.Context, modelID, deploymentID string) error {
	return m.DeleteDeploymentFunc(ctx, modelID, deploymentID)
}

func (m *MockClient) RetryDeployment(ctx context.Context, modelID, deploymentID string) (*RetryResponse, error) {
	return m.RetryDeploymentFunc(ctx, modelID, deploymentID)
}
