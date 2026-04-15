package truss

import (
	"os"
	"path/filepath"
	"testing"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

func ptr[T any](v T) *T { return &v }

func TestGenerateConfigYAML(t *testing.T) {
	tc := &modelsv1alpha1.TrussConfig{
		PythonVersion: "py312",
		Resources: modelsv1alpha1.TrussResources{
			Accelerator: "H100:2",
			UseGpu:      ptr(true),
		},
		Secrets: map[string]string{
			"docker-registry-secret": "",
			"datadog-api-key":        "",
		},
		EnvironmentVariables: map[string]string{
			"DD_SITE":    "us5.datadoghq.com",
			"DD_SERVICE": "vllm-test",
		},
		BaseImage: modelsv1alpha1.TrussBaseImage{
			Image: "us-docker.pkg.dev/test/vllm:0.11.2.1",
			DockerAuth: &modelsv1alpha1.TrussDockerAuth{
				AuthMethod: "GCP_SERVICE_ACCOUNT_JSON",
				SecretName: "docker-registry-secret",
				Registry:   "us-docker.pkg.dev",
			},
		},
		DockerServer: &modelsv1alpha1.TrussDockerServer{
			NoBuild:           ptr(true),
			StartCommand:      "sh -c 'bash /app/data/setup.sh'",
			ReadinessEndpoint: "/health",
			LivenessEndpoint:  "/health",
			PredictEndpoint:   "/v1/completions",
			ServerPort:        ptr(int32(8000)),
		},
		Runtime: &modelsv1alpha1.TrussRuntime{
			PredictConcurrency: ptr(int32(256)),
		},
		ModelMetadata: &modelsv1alpha1.TrussModelMetadata{
			Tags: []string{"openai-compatible"},
		},
	}

	data, err := GenerateConfigYAML(tc, "test-model")
	if err != nil {
		t.Fatalf("GenerateConfigYAML() error: %v", err)
	}

	// Parse back and verify snake_case keys
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to parse generated YAML: %v", err)
	}

	// Top-level fields
	if parsed["model_name"] != "test-model" {
		t.Errorf("model_name = %v, want test-model", parsed["model_name"])
	}
	if parsed["python_version"] != "py312" {
		t.Errorf("python_version = %v, want py312", parsed["python_version"])
	}

	// Resources (snake_case)
	resources, ok := parsed["resources"].(map[string]any)
	if !ok {
		t.Fatal("resources missing or wrong type")
	}
	if resources["accelerator"] != "H100:2" {
		t.Errorf("resources.accelerator = %v, want H100:2", resources["accelerator"])
	}
	if resources["use_gpu"] != true {
		t.Errorf("resources.use_gpu = %v, want true", resources["use_gpu"])
	}

	// Base image (snake_case)
	baseImage, ok := parsed["base_image"].(map[string]any)
	if !ok {
		t.Fatal("base_image missing or wrong type")
	}
	if baseImage["image"] != "us-docker.pkg.dev/test/vllm:0.11.2.1" {
		t.Errorf("base_image.image = %v", baseImage["image"])
	}
	dockerAuth, ok := baseImage["docker_auth"].(map[string]any)
	if !ok {
		t.Fatal("docker_auth missing or wrong type")
	}
	if dockerAuth["auth_method"] != "GCP_SERVICE_ACCOUNT_JSON" {
		t.Errorf("docker_auth.auth_method = %v", dockerAuth["auth_method"])
	}

	// Docker server (snake_case)
	ds, ok := parsed["docker_server"].(map[string]any)
	if !ok {
		t.Fatal("docker_server missing or wrong type")
	}
	if ds["no_build"] != true {
		t.Errorf("docker_server.no_build = %v, want true", ds["no_build"])
	}
	if ds["start_command"] != "sh -c 'bash /app/data/setup.sh'" {
		t.Errorf("docker_server.start_command = %v", ds["start_command"])
	}
	if ds["predict_endpoint"] != "/v1/completions" {
		t.Errorf("docker_server.predict_endpoint = %v", ds["predict_endpoint"])
	}
	if ds["server_port"] != 8000 {
		t.Errorf("docker_server.server_port = %v", ds["server_port"])
	}

	// Runtime (snake_case)
	rt, ok := parsed["runtime"].(map[string]any)
	if !ok {
		t.Fatal("runtime missing or wrong type")
	}
	if rt["predict_concurrency"] != 256 {
		t.Errorf("runtime.predict_concurrency = %v", rt["predict_concurrency"])
	}
}

func TestGenerateConfigYAML_Minimal(t *testing.T) {
	tc := &modelsv1alpha1.TrussConfig{
		Resources: modelsv1alpha1.TrussResources{
			Accelerator: "L4",
		},
		BaseImage: modelsv1alpha1.TrussBaseImage{
			Image: "nvcr.io/nvidia/nemo:23.03",
		},
	}

	data, err := GenerateConfigYAML(tc, "minimal-model")
	if err != nil {
		t.Fatalf("GenerateConfigYAML() error: %v", err)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	if parsed["model_name"] != "minimal-model" {
		t.Errorf("model_name = %v", parsed["model_name"])
	}
	if parsed["python_version"] != nil {
		t.Errorf("python_version should be omitted, got %v", parsed["python_version"])
	}
	if parsed["docker_server"] != nil {
		t.Errorf("docker_server should be omitted, got %v", parsed["docker_server"])
	}
}

func TestHashTrussConfig(t *testing.T) {
	tc := &modelsv1alpha1.TrussConfig{
		Resources: modelsv1alpha1.TrussResources{Accelerator: "H100:2"},
		BaseImage: modelsv1alpha1.TrussBaseImage{Image: "test:latest"},
	}

	hash1 := HashTrussConfig(tc, "echo hello")
	hash2 := HashTrussConfig(tc, "echo hello")
	hash3 := HashTrussConfig(tc, "echo world")

	if hash1 != hash2 {
		t.Errorf("same input should produce same hash: %s != %s", hash1, hash2)
	}
	if hash1 == hash3 {
		t.Error("different input should produce different hash")
	}
	if len(hash1) != 8 {
		t.Errorf("hash should be 8 hex chars, got %d: %s", len(hash1), hash1)
	}
}

func TestDeploymentName(t *testing.T) {
	tests := []struct {
		hash     string
		imageURI string
		want     string
	}{
		{"a3f7c2b1", "us-docker.pkg.dev/repo/vllm:0.11.2.1", "depl-vllm-0.11.2.1-a3f7c2b1"},
		{"b2c3d4e5", "nginx:latest", "depl-nginx-latest-b2c3d4e5"},
		{"c3d4e5f6", "nginx", "depl-nginx-latest-c3d4e5f6"},
		{"d4e5f6a7", "nvcr.io/nvidia/nemo:23.03", "depl-nemo-23.03-d4e5f6a7"},
		{"e5f6a7b8", "", "depl-e5f6a7b8"},
	}
	for _, tt := range tests {
		got := DeploymentName(tt.hash, tt.imageURI)
		if got != tt.want {
			t.Errorf("DeploymentName(%q, %q) = %q, want %q", tt.hash, tt.imageURI, got, tt.want)
		}
	}
}

func TestParseImage(t *testing.T) {
	tests := []struct {
		uri      string
		wantName string
		wantTag  string
	}{
		{"us-docker.pkg.dev/repo/vllm:0.11.2.1", "vllm", "0.11.2.1"},
		{"nginx:latest", "nginx", "latest"},
		{"nginx", "nginx", "latest"},
		{"nvcr.io/nvidia/nemo:23.03", "nemo", "23.03"},
		{"", "", ""},
	}
	for _, tt := range tests {
		name, tag := parseImage(tt.uri)
		if name != tt.wantName || tag != tt.wantTag {
			t.Errorf("parseImage(%q) = (%q, %q), want (%q, %q)", tt.uri, name, tag, tt.wantName, tt.wantTag)
		}
	}
}

func TestWriteTrussDirectory(t *testing.T) {
	dir := t.TempDir()
	trussDir := filepath.Join(dir, "test-truss")

	configYAML := []byte("model_name: test\n")
	setupScript := []byte("#!/bin/bash\necho hello\n")

	if err := WriteTrussDirectory(trussDir, configYAML, setupScript); err != nil {
		t.Fatalf("WriteTrussDirectory() error: %v", err)
	}

	// Verify config.yaml
	data, err := os.ReadFile(filepath.Join(trussDir, "config.yaml"))
	if err != nil {
		t.Fatalf("reading config.yaml: %v", err)
	}
	if string(data) != "model_name: test\n" {
		t.Errorf("config.yaml content = %q", string(data))
	}

	// Verify data/setup.sh
	data, err = os.ReadFile(filepath.Join(trussDir, "data", "setup.sh"))
	if err != nil {
		t.Fatalf("reading setup.sh: %v", err)
	}
	if string(data) != "#!/bin/bash\necho hello\n" {
		t.Errorf("setup.sh content = %q", string(data))
	}
}

func TestWriteTrussDirectory_NoSetupScript(t *testing.T) {
	dir := t.TempDir()
	trussDir := filepath.Join(dir, "test-truss")

	if err := WriteTrussDirectory(trussDir, []byte("model_name: test\n"), nil); err != nil {
		t.Fatalf("WriteTrussDirectory() error: %v", err)
	}

	// data/ directory should not exist
	if _, err := os.Stat(filepath.Join(trussDir, "data")); !os.IsNotExist(err) {
		t.Error("data/ directory should not exist when no setup script is provided")
	}
}
