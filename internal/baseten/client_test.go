package baseten

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	managementapi "github.com/basetenlabs/baseten-go/client/managementapi"
)

const (
	testStrategyReplica    = "REPLICA"
	testCleanupScaleToZero = "SCALE_TO_ZERO"
)

func ptr[T any](v T) *T {
	return &v
}

// newTestClient builds a Client whose generated management API points at a test server.
// The server URL must NOT include "/v1" — the generated client prepends it, so handler
// path assertions below expect the "/v1/..." prefix.
func newTestClient(serverURL string) *Client {
	return &Client{
		api: &managementapi.Client{
			BaseURL:    serverURL,
			HTTPClient: http.DefaultClient,
			Headers:    http.Header{"Authorization": {"Api-Key test-key"}},
		},
	}
}

func TestDeploymentNameMatchesPrefix(t *testing.T) {
	tests := []struct {
		name           string
		deploymentName string
		prefix         string
		want           bool
	}{
		{"exact match", "img-1.0-wgt-1.0-p-1.2", "img-1.0-wgt-1.0-p-1.2", true},
		{"prefix with timestamp suffix", "img-1.0-wgt-1.0-p-1.2.1768269232", "img-1.0-wgt-1.0-p-1.2", true},
		{"no match", "img-2.0-wgt-1.0-p-1.2", "img-1.0-wgt-1.0-p-1.2", false},
		{"prefix is longer than deployment name", "img-1.0", "img-1.0-wgt-1.0-p-1.2", false},
		{"empty deployment name", "", "img-1.0", false},
		{"empty prefix", "img-1.0", "", false},
		{"both empty", "", "", true},
		{"prefix without dot separator should not match", "img-1.0-wgt-1.0-p-1.2extra", "img-1.0-wgt-1.0-p-1.2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeploymentNameMatchesPrefix(tt.deploymentName, tt.prefix)
			if got != tt.want {
				t.Errorf("DeploymentNameMatchesPrefix(%q, %q) = %v, want %v", tt.deploymentName, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestIsTerminalFailure(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{DeploymentStatusFailed, true},
		{DeploymentStatusDeployFailed, true},
		{DeploymentStatusBuildFailed, true},
		{DeploymentStatusBuildStopped, true},
		{DeploymentStatusActive, false},
		{DeploymentStatusBuilding, false},
		{DeploymentStatusDeploying, false},
		{DeploymentStatusScaledToZero, false},
		{DeploymentStatusInactive, false},
		{"UNKNOWN", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := IsTerminalFailure(tt.status)
			if got != tt.want {
				t.Errorf("IsTerminalFailure(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestIsRetryableFailure(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{DeploymentStatusFailed, true},
		{DeploymentStatusDeployFailed, true},
		{DeploymentStatusBuildFailed, true},
		{DeploymentStatusBuildStopped, false}, // intentional user action
		{DeploymentStatusActive, false},
		{DeploymentStatusBuilding, false},
		{DeploymentStatusDeploying, false},
		{DeploymentStatusScaledToZero, false},
		{DeploymentStatusInactive, false},
		{"UNKNOWN", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := IsRetryableFailure(tt.status)
			if got != tt.want {
				t.Errorf("IsRetryableFailure(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestRetryDeployment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/v1/models/m1/deployments/d1/retry") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"retried": true, "reason": "", "deployment": {"id": "d1", "name": "test-dep", "status": "BUILDING"}}`))
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	resp, err := client.RetryDeployment(context.Background(), "m1", "d1")
	if err != nil {
		t.Fatalf("RetryDeployment returned error: %v", err)
	}
	if !resp.Retried {
		t.Error("expected Retried=true")
	}
	if resp.Deployment == nil || resp.Deployment.Status != "BUILDING" {
		t.Error("expected deployment with BUILDING status")
	}
}

func TestIsNotFoundError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"APIError with 404", &APIError{StatusCode: http.StatusNotFound, Message: "not found"}, true},
		{"APIError with 500", &APIError{StatusCode: http.StatusInternalServerError, Message: "internal error"}, false},
		{"wrapped APIError with 404", fmt.Errorf("wrapped: %w", &APIError{StatusCode: http.StatusNotFound, Message: "not found"}), true},
		{"non-APIError", errors.New("some other error"), false},
		{"nil error", nil, false},
		{"APIError with 403", &APIError{StatusCode: http.StatusForbidden, Message: "forbidden"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsNotFoundError(tt.err)
			if got != tt.want {
				t.Errorf("IsNotFoundError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAPIError_Error(t *testing.T) {
	err := &APIError{StatusCode: 404, Message: "not found"}
	expected := "API request failed with status 404: not found"
	if err.Error() != expected {
		t.Errorf("APIError.Error() = %q, want %q", err.Error(), expected)
	}
}

func TestHasAutoscalingDrift(t *testing.T) {
	tests := []struct {
		name      string
		spec      *modelsv1alpha1.AutoscalingConfig
		env       *AutoscalingSettings
		wantDrift bool
		wantCount int
	}{
		{"nil spec", nil, &AutoscalingSettings{MinReplica: 1}, false, 0},
		{"nil env", &modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(1))}, nil, false, 0},
		{"both nil", nil, nil, false, 0},
		{
			"no drift - values match",
			&modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(0)), MaxReplicas: ptr(int32(5)), ConcurrencyTarget: ptr(int32(10))},
			&AutoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10},
			false, 0,
		},
		{
			"drift on single field",
			&modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(2))},
			&AutoscalingSettings{MinReplica: 0},
			true, 1,
		},
		{
			"drift on multiple fields",
			&modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(2)), MaxReplicas: ptr(int32(10)), ConcurrencyTarget: ptr(int32(20))},
			&AutoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10},
			true, 3,
		},
		{
			"spec field nil - not checked",
			&modelsv1alpha1.AutoscalingConfig{MinReplicas: nil},
			&AutoscalingSettings{MinReplica: 5},
			false, 0,
		},
		{
			"optional pointer fields with drift",
			&modelsv1alpha1.AutoscalingConfig{AutoscalingWindow: ptr(int32(60)), ScaleDownDelay: ptr(int32(120)), TargetUtilizationPercentage: ptr(int32(80))},
			&AutoscalingSettings{AutoscalingWindow: ptr(int32(30)), ScaleDownDelay: ptr(int32(60)), TargetUtilizationPercentage: ptr(int32(50))},
			true, 3,
		},
		{
			"optional pointer fields - env nil treated as zero",
			&modelsv1alpha1.AutoscalingConfig{AutoscalingWindow: ptr(int32(60))},
			&AutoscalingSettings{AutoscalingWindow: nil},
			true, 1,
		},
		{
			"optional pointer fields - no drift when both zero",
			&modelsv1alpha1.AutoscalingConfig{AutoscalingWindow: ptr(int32(0))},
			&AutoscalingSettings{AutoscalingWindow: nil},
			false, 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDrift, gotDrifts := HasAutoscalingDrift(tt.spec, tt.env)
			if gotDrift != tt.wantDrift {
				t.Errorf("HasAutoscalingDrift() drift = %v, want %v", gotDrift, tt.wantDrift)
			}
			if tt.wantDrift && len(gotDrifts) != tt.wantCount {
				t.Errorf("HasAutoscalingDrift() drifts count = %d, want %d, drifts: %v", len(gotDrifts), tt.wantCount, gotDrifts)
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Setenv("BASETEN_API_KEY", "test-api-key")
		t.Setenv("BASETEN_API_BASE_URL", "")
		client, err := NewClient()
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if got := client.api.Headers.Get("Authorization"); got != "Api-Key test-api-key" {
			t.Errorf("Authorization header = %q, want %q", got, "Api-Key test-api-key")
		}
		if client.api.BaseURL != defaultBaseURL {
			t.Errorf("BaseURL = %q, want %q", client.api.BaseURL, defaultBaseURL)
		}
	})

	t.Run("base url override used verbatim", func(t *testing.T) {
		t.Setenv("BASETEN_API_KEY", "test-api-key")
		t.Setenv("BASETEN_API_BASE_URL", "https://example.com")
		client, err := NewClient()
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if client.api.BaseURL != "https://example.com" {
			t.Errorf("BaseURL = %q, want %q", client.api.BaseURL, "https://example.com")
		}
	})

	t.Run("missing env var", func(t *testing.T) {
		t.Setenv("BASETEN_API_KEY", "")
		_, err := NewClient()
		if err == nil {
			t.Fatal("NewClient() expected error, got nil")
		}
	})
}

func TestToUpdateAutoscalingSettings(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		if result := toUpdateAutoscalingSettings(nil); result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("empty config (all nil fields)", func(t *testing.T) {
		if result := toUpdateAutoscalingSettings(&modelsv1alpha1.AutoscalingConfig{}); result != nil {
			t.Errorf("expected nil for empty config, got %v", result)
		}
	})

	t.Run("all fields set", func(t *testing.T) {
		result := toUpdateAutoscalingSettings(&modelsv1alpha1.AutoscalingConfig{
			MinReplicas:                 ptr(int32(0)),
			MaxReplicas:                 ptr(int32(5)),
			ConcurrencyTarget:           ptr(int32(10)),
			AutoscalingWindow:           ptr(int32(60)),
			ScaleDownDelay:              ptr(int32(120)),
			TargetUtilizationPercentage: ptr(int32(80)),
		})
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		assertIntPtr(t, "min_replica", result.MinReplica, 0)
		assertIntPtr(t, "max_replica", result.MaxReplica, 5)
		assertIntPtr(t, "concurrency_target", result.ConcurrencyTarget, 10)
		assertIntPtr(t, "autoscaling_window", result.AutoscalingWindow, 60)
		assertIntPtr(t, "scale_down_delay", result.ScaleDownDelay, 120)
		assertIntPtr(t, "target_utilization_percentage", result.TargetUtilizationPercentage, 80)
	})

	t.Run("partial fields", func(t *testing.T) {
		result := toUpdateAutoscalingSettings(&modelsv1alpha1.AutoscalingConfig{
			MinReplicas: ptr(int32(1)),
		})
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		assertIntPtr(t, "min_replica", result.MinReplica, 1)
		if result.MaxReplica != nil {
			t.Errorf("expected max_replica nil, got %v", *result.MaxReplica)
		}
	})
}

func TestToUpdatePromotionSettings(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		if result := toUpdatePromotionSettings(nil); result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("empty config", func(t *testing.T) {
		if result := toUpdatePromotionSettings(&modelsv1alpha1.PromotionSettingsConfig{}); result != nil {
			t.Errorf("expected nil for empty config, got %v", result)
		}
	})

	t.Run("all fields set", func(t *testing.T) {
		strategy := testStrategyReplica
		cleanup := testCleanupScaleToZero
		result := toUpdatePromotionSettings(&modelsv1alpha1.PromotionSettingsConfig{
			RedeployOnPromotion:      ptr(true),
			RollingDeploy:            ptr(true),
			PromotionCleanupStrategy: &cleanup,
			RampUpWhilePromoting:     ptr(false),
			RampUpDurationSeconds:    ptr(int32(300)),
			RollingDeployConfig: &modelsv1alpha1.RollingDeployConfig{
				Strategy: &strategy,
			},
		})
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.RedeployOnPromotion == nil || !*result.RedeployOnPromotion {
			t.Error("missing/incorrect redeploy_on_promotion")
		}
		if result.RollingDeploy == nil || !*result.RollingDeploy {
			t.Error("missing/incorrect rolling_deploy")
		}
		if result.PromotionCleanupStrategy == nil || string(*result.PromotionCleanupStrategy) != testCleanupScaleToZero {
			t.Error("missing/incorrect promotion_cleanup_strategy")
		}
		if result.RampUpWhilePromoting == nil || *result.RampUpWhilePromoting {
			t.Error("missing/incorrect ramp_up_while_promoting")
		}
		assertIntPtr(t, "ramp_up_duration_seconds", result.RampUpDurationSeconds, 300)
		if result.RollingDeployConfig == nil {
			t.Error("missing rolling_deploy_config")
		}
	})

	t.Run("partial fields without rolling deploy config", func(t *testing.T) {
		result := toUpdatePromotionSettings(&modelsv1alpha1.PromotionSettingsConfig{
			RedeployOnPromotion: ptr(true),
		})
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.RollingDeployConfig != nil {
			t.Error("expected no rolling_deploy_config")
		}
		if result.RollingDeploy != nil {
			t.Error("expected no rolling_deploy")
		}
	})
}

func TestToUpdateRollingDeployConfig(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		if result := toUpdateRollingDeployConfig(nil); result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("empty config", func(t *testing.T) {
		if result := toUpdateRollingDeployConfig(&modelsv1alpha1.RollingDeployConfig{}); result != nil {
			t.Errorf("expected nil for empty config, got %v", result)
		}
	})

	t.Run("all fields set", func(t *testing.T) {
		strategy := testStrategyReplica
		result := toUpdateRollingDeployConfig(&modelsv1alpha1.RollingDeployConfig{
			Strategy:                 &strategy,
			MaxSurgePercent:          ptr(int32(25)),
			MaxUnavailablePercent:    ptr(int32(25)),
			StabilizationTimeSeconds: ptr(int32(60)),
		})
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if result.RollingDeployStrategy == nil || string(*result.RollingDeployStrategy) != testStrategyReplica {
			t.Error("missing/incorrect rolling_deploy_strategy")
		}
		assertIntPtr(t, "max_surge_percent", result.MaxSurgePercent, 25)
		assertIntPtr(t, "max_unavailable_percent", result.MaxUnavailablePercent, 25)
		assertIntPtr(t, "stabilization_time_seconds", result.StabilizationTimeSeconds, 60)
	})
}

func assertIntPtr(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: expected %d, got nil", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", name, *got, want)
	}
}

// writeJSON encodes v as the response body with the application/json content type the
// generated management client requires on success responses.
func writeJSON(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("failed to encode JSON: %v", err)
	}
}

// writeEmptyOK writes a minimal successful JSON response for endpoints whose body the
// client ignores but which the generated client still decodes.
func writeEmptyOK(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeJSON(t, w, struct{}{})
}

func writeError(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("failed to write body: %v", err)
	}
}

func decodeJSON(t *testing.T, r *http.Request, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
}

func status(s string) managementapi.DeploymentStatus {
	return managementapi.DeploymentStatus(s)
}

func TestFindModelIDByName(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Api-Key test-key" {
				t.Error("missing or wrong authorization header")
			}
			writeJSON(t, w, managementapi.Models{Models: []managementapi.Model{{Id: "m1", Name: "my-model"}}})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		id, err := c.FindModelIDByName(context.Background(), "my-model")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "m1" {
			t.Errorf("FindModelIDByName() = %q, want %q", id, "m1")
		}
	})

	t.Run("not found returns empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, managementapi.Models{Models: []managementapi.Model{{Id: "m1", Name: "other-model"}}})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		id, err := c.FindModelIDByName(context.Background(), "my-model")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "" {
			t.Errorf("expected empty, got %q", id)
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusInternalServerError, "server error")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		_, err := c.FindModelIDByName(context.Background(), "my-model")
		if err == nil {
			t.Fatal("expected error")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected APIError, got %T: %v", err, err)
		}
		if apiErr.StatusCode != 500 {
			t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
		}
	})
}

func TestDeleteModel(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		if err := c.DeleteModel(context.Background(), "model1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusNotFound, "not found")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.DeleteModel(context.Background(), "model1")
		if err == nil {
			t.Fatal("expected error")
		}
		if !IsNotFoundError(err) {
			t.Errorf("expected IsNotFoundError, got %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusInternalServerError, "server error")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.DeleteModel(context.Background(), "model1")
		if err == nil {
			t.Fatal("expected error")
		}
		if IsNotFoundError(err) {
			t.Errorf("did not expect 404, got %v", err)
		}
	})
}

func TestFindDeploymentIDByName(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models/model1/deployments" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeJSON(t, w, managementapi.Deployments{Deployments: []managementapi.Deployment{
				{Id: "d1", Name: "my-dep", Status: status("ACTIVE")},
			}})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		id, st, err := c.FindDeploymentIDByName(context.Background(), "model1", "my-dep")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "d1" {
			t.Errorf("got id %q, want %q", id, "d1")
		}
		if st != "ACTIVE" {
			t.Errorf("got status %q, want %q", st, "ACTIVE")
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, managementapi.Deployments{Deployments: []managementapi.Deployment{}})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		id, st, err := c.FindDeploymentIDByName(context.Background(), "model1", "missing")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "" {
			t.Errorf("expected empty id, got %q", id)
		}
		if st != "" {
			t.Errorf("expected empty status, got %q", st)
		}
	})
}

func TestActivateDeployment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1/deployments/d1/activate" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.ActivateDeployment(context.Background(), "model1", "d1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusBadRequest, "bad request")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.ActivateDeployment(context.Background(), "model1", "d1")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestGetEnvironment(t *testing.T) {
	const testEnvName = "dev"

	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models/model1/environments/dev" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeJSON(t, w, managementapi.Environment{
				Name:              testEnvName,
				CurrentDeployment: managementapi.Deployment{Id: "d1", Name: "dep-1", Status: status("ACTIVE")},
			})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		env, err := c.GetEnvironment(context.Background(), "model1", testEnvName)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if env.Name != testEnvName {
			t.Errorf("Name = %q, want %q", env.Name, testEnvName)
		}
		if env.CurrentDeployment == nil || env.CurrentDeployment.ID != "d1" {
			t.Error("expected current deployment with ID d1")
		}
	})

	t.Run("404 returns APIError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusNotFound, "not found")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		_, err := c.GetEnvironment(context.Background(), "model1", testEnvName)
		if err == nil {
			t.Fatal("expected error")
		}
		if !IsNotFoundError(err) {
			t.Errorf("expected not found error, got: %v", err)
		}
	})
}

func TestListEnvironments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, managementapi.Environments{
			Environments: []managementapi.Environment{
				{
					Name:              "dev",
					CurrentDeployment: managementapi.Deployment{Id: "d1", Name: "dep-1", Status: status("ACTIVE")},
				},
				{
					Name:              "staging",
					CurrentDeployment: managementapi.Deployment{Id: "d2", Name: "dep-2", Status: status("ACTIVE")},
					CandidateDeployment: &managementapi.Deployment{
						Id: "d3", Name: "dep-3", Status: status("DEPLOYING"),
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	envs, err := c.ListEnvironments(context.Background(), "model1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("expected 2 envs, got %d", len(envs))
	}
	if envs[0].Name != "dev" || envs[1].Name != "staging" {
		t.Errorf("unexpected env names: %v, %v", envs[0].Name, envs[1].Name)
	}
	if envs[0].CurrentDeployment == nil || envs[0].CurrentDeployment.ID != "d1" {
		t.Error("expected dev env to have current deployment d1")
	}
	if envs[1].CandidateDeployment == nil || envs[1].CandidateDeployment.ID != "d3" {
		t.Error("expected staging env to have candidate deployment d3")
	}
}

func TestCreateEnvironment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotBody managementapi.CreateEnvironmentRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1/environments" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			decodeJSON(t, r, &gotBody)
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.CreateEnvironment(context.Background(), "model1", &modelsv1alpha1.EnvironmentConfig{
			Name: "dev",
			Autoscaling: &modelsv1alpha1.AutoscalingConfig{
				MinReplicas: ptr(int32(1)),
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotBody.Name != "dev" {
			t.Errorf("body.Name = %q, want %q", gotBody.Name, "dev")
		}
		if gotBody.AutoscalingSettings == nil || gotBody.AutoscalingSettings.MinReplica == nil || *gotBody.AutoscalingSettings.MinReplica != 1 {
			t.Errorf("expected autoscaling min_replica=1, got %+v", gotBody.AutoscalingSettings)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusBadRequest, "invalid")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.CreateEnvironment(context.Background(), "model1", &modelsv1alpha1.EnvironmentConfig{Name: "dev"})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestUpdateEnvironmentSettings(t *testing.T) {
	t.Run("autoscaling only", func(t *testing.T) {
		var gotBody map[string]interface{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("expected PATCH, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1/environments/dev" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			decodeJSON(t, r, &gotBody)
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.UpdateEnvironmentSettings(context.Background(), "model1", "dev",
			&modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(2))},
			nil,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := gotBody["autoscaling_settings"]; !ok {
			t.Error("expected autoscaling_settings in body")
		}
		if _, ok := gotBody["promotion_settings"]; ok {
			t.Error("expected no promotion_settings in body when nil")
		}
	})

	t.Run("promotion only", func(t *testing.T) {
		var gotBody map[string]interface{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			decodeJSON(t, r, &gotBody)
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.UpdateEnvironmentSettings(context.Background(), "model1", "dev",
			nil,
			&modelsv1alpha1.PromotionSettingsConfig{RollingDeploy: ptr(true)},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := gotBody["promotion_settings"]; !ok {
			t.Error("expected promotion_settings in body")
		}
		if _, ok := gotBody["autoscaling_settings"]; ok {
			t.Error("expected no autoscaling_settings in body when nil")
		}
	})

	t.Run("both autoscaling and promotion", func(t *testing.T) {
		var gotBody map[string]interface{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			decodeJSON(t, r, &gotBody)
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.UpdateEnvironmentSettings(context.Background(), "model1", "dev",
			&modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(2))},
			&modelsv1alpha1.PromotionSettingsConfig{RollingDeploy: ptr(true)},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := gotBody["autoscaling_settings"]; !ok {
			t.Error("expected autoscaling_settings in body")
		}
		if _, ok := gotBody["promotion_settings"]; !ok {
			t.Error("expected promotion_settings in body")
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusForbidden, "forbidden")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.UpdateEnvironmentSettings(context.Background(), "model1", "dev",
			&modelsv1alpha1.AutoscalingConfig{},
			nil,
		)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestPromote(t *testing.T) {
	t.Run("success with defaults", func(t *testing.T) {
		var gotBody managementapi.PromoteToEnvironmentRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1/environments/dev/promote" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			decodeJSON(t, r, &gotBody)
			writeJSON(t, w, managementapi.Deployment{Id: "promoted-1", Name: "dep.123", Status: status("DEPLOYING")})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		dep, err := c.Promote(context.Background(), "model1", "d1", "dev", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dep.ID != "promoted-1" {
			t.Errorf("ID = %q, want %q", dep.ID, "promoted-1")
		}
		if gotBody.DeploymentId != "d1" {
			t.Errorf("DeploymentId = %q, want %q", gotBody.DeploymentId, "d1")
		}
		if gotBody.ScaleDownPreviousDeployment == nil || !*gotBody.ScaleDownPreviousDeployment {
			t.Error("expected ScaleDownPreviousDeployment=true by default")
		}
		if gotBody.PreserveEnvInstanceType == nil || !*gotBody.PreserveEnvInstanceType {
			t.Error("expected PreserveEnvInstanceType=true by default")
		}
	})

	t.Run("success with custom settings", func(t *testing.T) {
		var gotBody managementapi.PromoteToEnvironmentRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			decodeJSON(t, r, &gotBody)
			writeJSON(t, w, managementapi.Deployment{Id: "promoted-2", Name: "dep.456", Status: status("DEPLOYING")})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		settings := &modelsv1alpha1.PromotionSettingsConfig{
			ScaleDownPreviousDeployment: ptr(false),
			PreserveInstanceType:        ptr(false),
		}
		_, err := c.Promote(context.Background(), "model1", "d1", "dev", settings)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotBody.ScaleDownPreviousDeployment == nil || *gotBody.ScaleDownPreviousDeployment {
			t.Error("expected ScaleDownPreviousDeployment=false")
		}
		if gotBody.PreserveEnvInstanceType == nil || *gotBody.PreserveEnvInstanceType {
			t.Error("expected PreserveEnvInstanceType=false")
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusConflict, "conflict")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		_, err := c.Promote(context.Background(), "model1", "d1", "dev", nil)
		if err == nil {
			t.Fatal("expected error")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected APIError, got %T", err)
		}
		if apiErr.StatusCode != http.StatusConflict {
			t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusConflict)
		}
	})
}

func TestListDeployments(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models/model1/deployments" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeJSON(t, w, managementapi.Deployments{
				Deployments: []managementapi.Deployment{
					{Id: "d1", Name: "dep-1", Status: status("ACTIVE")},
					{Id: "d2", Name: "dep-2", Status: status("SCALED_TO_ZERO")},
				},
			})
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		deps, err := c.ListDeployments(context.Background(), "model1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(deps) != 2 {
			t.Fatalf("expected 2 deployments, got %d", len(deps))
		}
		if deps[0].ID != "d1" || deps[1].ID != "d2" {
			t.Errorf("unexpected deployment IDs: %v, %v", deps[0].ID, deps[1].ID)
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusInternalServerError, "server error")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		_, err := c.ListDeployments(context.Background(), "model1")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestUpdateDeploymentAutoscaling(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotBody map[string]interface{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("expected PATCH, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1/deployments/d1/autoscaling_settings" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			decodeJSON(t, r, &gotBody)
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.UpdateDeploymentAutoscaling(context.Background(), "model1", "d1", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if val, ok := gotBody["min_replica"]; !ok || val != float64(0) {
			t.Errorf("expected min_replica=0, got %v", gotBody)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusForbidden, "forbidden")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.UpdateDeploymentAutoscaling(context.Background(), "model1", "d1", 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestDeleteDeployment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE, got %s", r.Method)
			}
			if r.URL.Path != "/v1/models/model1/deployments/d1" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeEmptyOK(t, w)
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.DeleteDeployment(context.Background(), "model1", "d1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusNotFound, "not found")
		}))
		defer srv.Close()

		c := newTestClient(srv.URL)
		err := c.DeleteDeployment(context.Background(), "model1", "d1")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestHasPromotionSettingsDrift(t *testing.T) {
	tests := []struct {
		name      string
		spec      *modelsv1alpha1.PromotionSettingsConfig
		env       *PromotionSettings
		wantDrift bool
		wantCount int
	}{
		{"nil spec", nil, &PromotionSettings{}, false, 0},
		{"nil env with nil spec", nil, nil, false, 0},
		{"nil env with non-nil spec field", &modelsv1alpha1.PromotionSettingsConfig{RollingDeploy: ptr(true)}, nil, true, 1},
		{"both nil", nil, nil, false, 0},
		{
			"no drift - values match",
			&modelsv1alpha1.PromotionSettingsConfig{
				RollingDeploy:        ptr(true),
				RampUpWhilePromoting: ptr(false),
			},
			&PromotionSettings{
				RollingDeploy:        ptr(true),
				RampUpWhilePromoting: ptr(false),
			},
			false, 0,
		},
		{
			"drift on single field",
			&modelsv1alpha1.PromotionSettingsConfig{RollingDeploy: ptr(true)},
			&PromotionSettings{RollingDeploy: ptr(false)},
			true, 1,
		},
		{
			"drift on multiple fields",
			&modelsv1alpha1.PromotionSettingsConfig{
				RollingDeploy:         ptr(true),
				RampUpDurationSeconds: ptr(int32(600)),
				RedeployOnPromotion:   ptr(false),
			},
			&PromotionSettings{
				RollingDeploy:         ptr(false),
				RampUpDurationSeconds: ptr(int32(300)),
				RedeployOnPromotion:   ptr(true),
			},
			true, 3,
		},
		{
			"spec field nil - not checked",
			&modelsv1alpha1.PromotionSettingsConfig{RollingDeploy: nil},
			&PromotionSettings{RollingDeploy: ptr(true)},
			false, 0,
		},
		{
			"rolling deploy config drift",
			&modelsv1alpha1.PromotionSettingsConfig{
				RollingDeployConfig: &modelsv1alpha1.RollingDeployConfig{
					MaxSurgePercent: ptr(int32(50)),
				},
			},
			&PromotionSettings{
				RollingDeployConfig: &RollingDeploySettings{
					MaxSurgePercent: ptr(int32(25)),
				},
			},
			true, 1,
		},
		{
			"rolling deploy config drift - env missing config",
			&modelsv1alpha1.PromotionSettingsConfig{
				RollingDeployConfig: &modelsv1alpha1.RollingDeployConfig{
					Strategy:        ptr(testStrategyReplica),
					MaxSurgePercent: ptr(int32(25)),
				},
			},
			&PromotionSettings{},
			true, 2,
		},
		{
			"promotion cleanup strategy drift",
			&modelsv1alpha1.PromotionSettingsConfig{
				PromotionCleanupStrategy: ptr(testCleanupScaleToZero),
			},
			&PromotionSettings{
				PromotionCleanupStrategy: ptr("KEEP"),
			},
			true, 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDrift, gotDrifts := HasPromotionSettingsDrift(tt.spec, tt.env)
			if gotDrift != tt.wantDrift {
				t.Errorf("HasPromotionSettingsDrift() drift = %v, want %v, drifts: %v", gotDrift, tt.wantDrift, gotDrifts)
			}
			if tt.wantDrift && len(gotDrifts) != tt.wantCount {
				t.Errorf("HasPromotionSettingsDrift() drifts count = %d, want %d, drifts: %v", len(gotDrifts), tt.wantCount, gotDrifts)
			}
		})
	}
}

// TestNetworkError verifies that a transport-level failure (unreachable server) does not
// surface as an *APIError, so IsNotFoundError and friends treat it as a generic error.
func TestNetworkError(t *testing.T) {
	c := newTestClient("http://127.0.0.1:1") // unreachable port
	_, err := c.FindModelIDByName(context.Background(), "my-model")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Error("network error should not be APIError")
	}
}
