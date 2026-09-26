package truss

import (
	"os"
	"path/filepath"
	"strings"
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
	for _, key := range []string{"cpu", "memory"} {
		if _, ok := resources[key]; ok {
			t.Errorf("resources.%s should be omitted when unset, got %v", key, resources[key])
		}
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

func TestGenerateConfigYAML_CPUOnly(t *testing.T) {
	tc := &modelsv1alpha1.TrussConfig{
		Resources: modelsv1alpha1.TrussResources{
			CPU:    "2",
			Memory: "4Gi",
			UseGpu: ptr(false),
		},
		BaseImage: modelsv1alpha1.TrussBaseImage{
			Image: "public.ecr.aws/q9t5s3a7/vllm-cpu-release-repo:v0.29.0",
		},
	}

	data, err := GenerateConfigYAML(tc, "cpu-model")
	if err != nil {
		t.Fatalf("GenerateConfigYAML() error: %v", err)
	}

	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	resources, ok := parsed["resources"].(map[string]any)
	if !ok {
		t.Fatal("resources missing or wrong type")
	}
	if _, ok := resources["accelerator"]; ok {
		t.Errorf("resources.accelerator must be omitted for CPU-only (truss rejects empty), got %v", resources["accelerator"])
	}
	if resources["cpu"] != "2" {
		t.Errorf("resources.cpu = %v, want \"2\"", resources["cpu"])
	}
	if resources["memory"] != "4Gi" {
		t.Errorf("resources.memory = %v, want 4Gi", resources["memory"])
	}
	if resources["use_gpu"] != false {
		t.Errorf("resources.use_gpu = %v, want false", resources["use_gpu"])
	}
	// cpu must stay a YAML string: truss parses "2" and 2 differently from "500m".
	if !strings.Contains(string(data), `cpu: "2"`) {
		t.Errorf("cpu should be emitted as a quoted string, got:\n%s", data)
	}
}

// Deployment names embed this hash, so any drift re-pushes every existing
// trussConfig model. Values captured before cpu/memory were added.
func TestHashTrussConfig_StableForExistingGPUSpecs(t *testing.T) {
	tests := []struct {
		name   string
		tc     *modelsv1alpha1.TrussConfig
		script string
		want   string
	}{
		{
			name: "accelerator only",
			tc: &modelsv1alpha1.TrussConfig{
				Resources: modelsv1alpha1.TrussResources{Accelerator: "H100:2"},
				BaseImage: modelsv1alpha1.TrussBaseImage{Image: "test:latest"},
			},
			script: "echo hello",
			want:   "60c81d3c",
		},
		{
			name: "accelerator with useGpu",
			tc: &modelsv1alpha1.TrussConfig{
				PythonVersion: "py312",
				Resources:     modelsv1alpha1.TrussResources{Accelerator: "H100:1", UseGpu: ptr(true)},
				BaseImage:     modelsv1alpha1.TrussBaseImage{Image: "us-docker.pkg.dev/img:v6"},
			},
			want: "f88ab883",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HashTrussConfig(tt.tc, tt.script); got != tt.want {
				t.Errorf("HashTrussConfig() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHashTrussConfig_CPUFieldsChangeHash(t *testing.T) {
	base := modelsv1alpha1.TrussConfig{
		Resources: modelsv1alpha1.TrussResources{CPU: "2", Memory: "4Gi"},
		BaseImage: modelsv1alpha1.TrussBaseImage{Image: "test:latest"},
	}
	moreCPU := base
	moreCPU.Resources.CPU = "4"
	moreMem := base
	moreMem.Resources.Memory = "8Gi"

	h := HashTrussConfig(&base, "")
	if h == HashTrussConfig(&moreCPU, "") {
		t.Error("changing cpu should change the hash")
	}
	if h == HashTrussConfig(&moreMem, "") {
		t.Error("changing memory should change the hash")
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
		// Digest-pinned refs: the digest must not leak into the deployment name
		// (Baseten rejects names carrying ':'/'@' — the 2026-08-26 incident).
		{"f6a7b8c9", "us-docker.pkg.dev/repo/vllm:0.11.2.1@sha256:a3f1c9e2b8d4f6a7c5e9d1b3f8a2c4e6d8b0a2c4e6f8a0b2c4d6e8f0a1b3c5d7", "depl-vllm-0.11.2.1-f6a7b8c9"},
		{"a7b8c9d0", "us-docker.pkg.dev/repo/vllm@sha256:a3f1c9e2b8d4f6a7c5e9d1b3f8a2c4e6d8b0a2c4e6f8a0b2c4d6e8f0a1b3c5d7", "depl-vllm-latest-a7b8c9d0"},
	}
	for _, tt := range tests {
		got := DeploymentName(tt.hash, tt.imageURI)
		if got != tt.want {
			t.Errorf("DeploymentName(%q, %q) = %q, want %q", tt.hash, tt.imageURI, got, tt.want)
		}
		if strings.ContainsAny(got, ":@/") {
			t.Errorf("DeploymentName(%q, %q) = %q contains characters invalid in a Baseten deployment name", tt.hash, tt.imageURI, got)
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
		// Digest suffixes must be dropped, not parsed as the tag.
		{"us-docker.pkg.dev/repo/vllm:0.11.2.1@sha256:a3f1c9e2b8d4f6a7c5e9d1b3f8a2c4e6d8b0a2c4e6f8a0b2c4d6e8f0a1b3c5d7", "vllm", "0.11.2.1"},
		{"us-docker.pkg.dev/repo/vllm@sha256:a3f1c9e2b8d4f6a7c5e9d1b3f8a2c4e6d8b0a2c4e6f8a0b2c4d6e8f0a1b3c5d7", "vllm", "latest"},
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
