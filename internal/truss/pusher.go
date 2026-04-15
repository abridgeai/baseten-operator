package truss

import (
	"context"
	"fmt"
	"os"

	truss "github.com/basetenlabs/truss-go"
)

// PushResult contains the result of a truss push operation.
type PushResult struct {
	ModelID      string
	DeploymentID string
}

// PusherInterface abstracts the push operation for testing.
type PusherInterface interface {
	Push(ctx context.Context, trussDir, modelName, deploymentName string) (*PushResult, error)
	PushFromConfig(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*PushResult, error)
}

// Pusher wraps the truss-go SDK for pushing deployments to Baseten.
type Pusher struct {
	apiKey string
}

var _ PusherInterface = (*Pusher)(nil)

// NewPusher creates a Pusher with the given API key.
func NewPusher(apiKey string) *Pusher {
	return &Pusher{apiKey: apiKey}
}

// Push creates a deployment by pushing a truss directory to Baseten.
// The truss directory must contain a config.yaml and optionally a data/ directory.
func (p *Pusher) Push(ctx context.Context, trussDir, modelName, deploymentName string) (*PushResult, error) {
	client := truss.NewClient(truss.WithAPIKey(p.apiKey))

	result, err := client.Push(ctx, trussDir, modelName,
		truss.Publish(),
		truss.WithDeploymentName(deploymentName),
	)
	if err != nil {
		return nil, fmt.Errorf("truss push failed: %w", err)
	}

	return &PushResult{
		ModelID:      result.ModelID,
		DeploymentID: result.VersionID,
	}, nil
}

// PushFromConfig is a convenience that generates config.yaml, writes a temp directory,
// pushes to Baseten, and cleans up. Returns the push result.
func (p *Pusher) PushFromConfig(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*PushResult, error) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "truss-push-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Write truss directory contents
	if err := WriteTrussDirectory(tmpDir, configYAML, setupScript); err != nil {
		return nil, fmt.Errorf("writing truss directory: %w", err)
	}

	return p.Push(ctx, tmpDir, modelName, deploymentName)
}

// MockPusher is a test double for PusherInterface.
type MockPusher struct {
	PushFunc           func(ctx context.Context, trussDir, modelName, deploymentName string) (*PushResult, error)
	PushFromConfigFunc func(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*PushResult, error)
}

var _ PusherInterface = (*MockPusher)(nil)

func (m *MockPusher) Push(ctx context.Context, trussDir, modelName, deploymentName string) (*PushResult, error) {
	return m.PushFunc(ctx, trussDir, modelName, deploymentName)
}

func (m *MockPusher) PushFromConfig(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*PushResult, error) {
	return m.PushFromConfigFunc(ctx, configYAML, setupScript, modelName, deploymentName)
}
