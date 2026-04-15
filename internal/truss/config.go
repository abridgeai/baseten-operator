package truss

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// GenerateConfigYAML converts a CRD TrussConfig into truss config.yaml content (snake_case).
func GenerateConfigYAML(tc *modelsv1alpha1.TrussConfig, modelName string) ([]byte, error) {
	config := map[string]any{
		"model_name": modelName,
	}

	if tc.PythonVersion != "" {
		config["python_version"] = tc.PythonVersion
	}

	// Resources
	resources := map[string]any{
		"accelerator": tc.Resources.Accelerator,
	}
	if tc.Resources.UseGpu != nil {
		resources["use_gpu"] = *tc.Resources.UseGpu
	}
	config["resources"] = resources

	// Secrets
	if len(tc.Secrets) > 0 {
		config["secrets"] = tc.Secrets
	}

	// Environment variables
	if len(tc.EnvironmentVariables) > 0 {
		config["environment_variables"] = tc.EnvironmentVariables
	}

	// Base image
	baseImage := map[string]any{
		"image": tc.BaseImage.Image,
	}
	if tc.BaseImage.DockerAuth != nil {
		baseImage["docker_auth"] = map[string]any{
			"auth_method": tc.BaseImage.DockerAuth.AuthMethod,
			"secret_name": tc.BaseImage.DockerAuth.SecretName,
			"registry":    tc.BaseImage.DockerAuth.Registry,
		}
	}
	config["base_image"] = baseImage

	// Docker server
	if tc.DockerServer != nil {
		ds := map[string]any{}
		if tc.DockerServer.NoBuild != nil {
			ds["no_build"] = *tc.DockerServer.NoBuild
		}
		if tc.DockerServer.StartCommand != "" {
			ds["start_command"] = tc.DockerServer.StartCommand
		}
		if tc.DockerServer.ReadinessEndpoint != "" {
			ds["readiness_endpoint"] = tc.DockerServer.ReadinessEndpoint
		}
		if tc.DockerServer.LivenessEndpoint != "" {
			ds["liveness_endpoint"] = tc.DockerServer.LivenessEndpoint
		}
		if tc.DockerServer.PredictEndpoint != "" {
			ds["predict_endpoint"] = tc.DockerServer.PredictEndpoint
		}
		if tc.DockerServer.ServerPort != nil {
			ds["server_port"] = *tc.DockerServer.ServerPort
		}
		config["docker_server"] = ds
	}

	// Runtime
	if tc.Runtime != nil {
		rt := map[string]any{}
		if tc.Runtime.PredictConcurrency != nil {
			rt["predict_concurrency"] = *tc.Runtime.PredictConcurrency
		}
		config["runtime"] = rt
	}

	// Model metadata
	if tc.ModelMetadata != nil {
		mm := map[string]any{}
		if tc.ModelMetadata.ExampleModelInput != nil && tc.ModelMetadata.ExampleModelInput.Raw != nil {
			var exampleInput any
			if err := json.Unmarshal(tc.ModelMetadata.ExampleModelInput.Raw, &exampleInput); err == nil {
				mm["example_model_input"] = exampleInput
			}
		}
		if len(tc.ModelMetadata.Tags) > 0 {
			mm["tags"] = tc.ModelMetadata.Tags
		}
		config["model_metadata"] = mm
	}

	return yaml.Marshal(config)
}

// HashTrussConfig computes a deterministic hash of the truss config and setup script.
// Used for change detection and deployment naming.
func HashTrussConfig(tc *modelsv1alpha1.TrussConfig, setupScript string) string {
	data, _ := json.Marshal(tc)
	combined := append(data, []byte(setupScript)...)
	hash := sha256.Sum256(combined)
	return fmt.Sprintf("%x", hash[:4]) // 8 hex chars
}

// DeploymentName generates a deterministic deployment name from the image and config hash.
// Format: depl-{imageName}-{imageTag}-{hash} e.g. depl-vllm-0.11.2.1-692a5091
// Falls back to depl-{hash} if image URI is empty.
func DeploymentName(configHash string, imageURI string) string {
	name, tag := parseImage(imageURI)
	if name != "" {
		return fmt.Sprintf("depl-%s-%s-%s", name, tag, configHash)
	}
	return fmt.Sprintf("depl-%s", configHash)
}

// parseImage extracts the image name and tag from a Docker image URI.
// "us-docker.pkg.dev/repo/vllm:0.11.2.1" → ("vllm", "0.11.2.1")
// "nginx:latest" → ("nginx", "latest")
// "nginx" → ("nginx", "latest")
// "" → ("", "")
func parseImage(uri string) (name, tag string) {
	if uri == "" {
		return "", ""
	}

	// Split off tag
	tag = "latest"
	ref := uri
	if i := strings.LastIndex(ref, ":"); i != -1 {
		tag = ref[i+1:]
		ref = ref[:i]
	}

	// Extract image name (last path segment)
	if i := strings.LastIndex(ref, "/"); i != -1 {
		name = ref[i+1:]
	} else {
		name = ref
	}

	return name, tag
}

// WriteTrussDirectory writes config.yaml and optionally data/setup.sh to a directory.
func WriteTrussDirectory(dir string, configYAML []byte, setupScript []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating truss directory: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), configYAML, 0o644); err != nil {
		return fmt.Errorf("writing config.yaml: %w", err)
	}

	if len(setupScript) > 0 {
		dataDir := filepath.Join(dir, "data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return fmt.Errorf("creating data directory: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "setup.sh"), setupScript, 0o644); err != nil {
			return fmt.Errorf("writing setup.sh: %w", err)
		}
	}

	return nil
}
