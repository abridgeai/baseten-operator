package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

type model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type deployment struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	Status              string               `json:"status"`
	ActiveReplicaCount  int32                `json:"active_replica_count"`
	CreatedAt           string               `json:"created_at,omitempty"`
	IsProduction        bool                 `json:"is_production,omitempty"`
	IsDevelopment       bool                 `json:"is_development,omitempty"`
	Environment         *string              `json:"environment,omitempty"`
	AutoscalingSettings *autoscalingSettings `json:"autoscaling_settings,omitempty"`
}

type autoscalingSettings struct {
	MinReplica        int32 `json:"min_replica"`
	MaxReplica        int32 `json:"max_replica"`
	ConcurrencyTarget int32 `json:"concurrency_target"`
}

type promotionSettings struct {
	RedeployOnPromotion      *bool                `json:"redeploy_on_promotion,omitempty"`
	RollingDeploy            *bool                `json:"rolling_deploy,omitempty"`
	PromotionCleanupStrategy *string              `json:"promotion_cleanup_strategy,omitempty"`
	RampUpWhilePromoting     *bool                `json:"ramp_up_while_promoting,omitempty"`
	RampUpDurationSeconds    *int32               `json:"ramp_up_duration_seconds,omitempty"`
	RollingDeployConfig      *rollingDeployConfig `json:"rolling_deploy_config,omitempty"`
}

type rollingDeployConfig struct {
	Strategy                 *string `json:"rolling_deploy_strategy,omitempty"`
	MaxSurgePercent          *int32  `json:"max_surge_percent,omitempty"`
	MaxUnavailablePercent    *int32  `json:"max_unavailable_percent,omitempty"`
	StabilizationTimeSeconds *int32  `json:"stabilization_time_seconds,omitempty"`
}

type environment struct {
	Name                string               `json:"name"`
	CurrentDeployment   *deployment          `json:"current_deployment,omitempty"`
	CandidateDeployment *deployment          `json:"candidate_deployment,omitempty"`
	AutoscalingSettings *autoscalingSettings `json:"autoscaling_settings,omitempty"`
	PromotionSettings   *promotionSettings   `json:"promotion_settings,omitempty"`
}

type state struct {
	mu              sync.RWMutex
	models          []model
	deployments     map[string][]deployment // modelID -> deployments
	environments    map[string]*environment // modelID:envName -> environment
	deletedModelIDs []string                // recorded by DELETE /v1/models/{id} for test assertions
}

func newState() *state {
	s := &state{
		deployments:  make(map[string][]deployment),
		environments: make(map[string]*environment),
	}
	s.reset()
	return s
}

func (s *state) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.models = []model{
		{ID: "model-001", Name: "test-model"},
	}
	s.deployments = map[string][]deployment{
		"model-001": {
			{ID: "deploy-001", Name: "img-1.0-wgt-1.0-p-1.0", Status: "ACTIVE", ActiveReplicaCount: 1,
				AutoscalingSettings: &autoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10}},
			{ID: "deploy-002", Name: "img-2.0-wgt-1.0-p-1.0", Status: "ACTIVE", ActiveReplicaCount: 1,
				AutoscalingSettings: &autoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10}},
		},
	}
	s.environments = make(map[string]*environment)
	s.deletedModelIDs = nil
}

func (s *state) envKey(modelID, envName string) string {
	return modelID + ":" + envName
}

func main() {
	st := newState()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		st.mu.RLock()
		defer st.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]interface{}{"models": st.models})
	})

	mux.HandleFunc("GET /v1/models/{model_id}/deployments", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		st.mu.RLock()
		defer st.mu.RUnlock()
		deps := st.deployments[modelID]
		if deps == nil {
			deps = []deployment{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"deployments": deps})
	})

	mux.HandleFunc("GET /v1/models/{model_id}/deployments/{dep_id}", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		depID := r.PathValue("dep_id")
		st.mu.RLock()
		defer st.mu.RUnlock()
		for _, d := range st.deployments[modelID] {
			if d.ID == depID {
				writeJSON(w, http.StatusOK, d)
				return
			}
		}
		http.Error(w, "deployment not found", http.StatusNotFound)
	})

	mux.HandleFunc("PATCH /v1/models/{model_id}/deployments/{dep_id}/autoscaling_settings", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		depID := r.PathValue("dep_id")
		var req struct {
			MinReplica *int32 `json:"min_replica,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		for i, d := range st.deployments[modelID] {
			if d.ID == depID {
				if req.MinReplica != nil && st.deployments[modelID][i].AutoscalingSettings != nil {
					st.deployments[modelID][i].AutoscalingSettings.MinReplica = *req.MinReplica
				}
				writeJSON(w, http.StatusOK, st.deployments[modelID][i])
				return
			}
		}
		http.Error(w, "deployment not found", http.StatusNotFound)
	})

	mux.HandleFunc("DELETE /v1/models/{model_id}", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		st.mu.Lock()
		defer st.mu.Unlock()
		found := false
		for i, m := range st.models {
			if m.ID == modelID {
				st.models = append(st.models[:i], st.models[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		// Cascade: drop all deployments and environments under this model.
		delete(st.deployments, modelID)
		for k := range st.environments {
			if strings.HasPrefix(k, modelID+":") {
				delete(st.environments, k)
			}
		}
		st.deletedModelIDs = append(st.deletedModelIDs, modelID)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("DELETE /v1/models/{model_id}/deployments/{dep_id}", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		depID := r.PathValue("dep_id")
		st.mu.Lock()
		defer st.mu.Unlock()
		deps := st.deployments[modelID]
		for i, d := range deps {
			if d.ID == depID {
				st.deployments[modelID] = append(deps[:i], deps[i+1:]...)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		http.Error(w, "deployment not found", http.StatusNotFound)
	})

	mux.HandleFunc("POST /v1/models/{model_id}/deployments/{dep_id}/activate", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		depID := r.PathValue("dep_id")
		st.mu.Lock()
		defer st.mu.Unlock()
		for i, d := range st.deployments[modelID] {
			if d.ID == depID {
				st.deployments[modelID][i].Status = "ACTIVATING"
				// Also update the env view so subsequent GET /environments reflects the transition
				// and the controller doesn't re-detect INACTIVE on the next reconcile.
				for _, env := range st.environments {
					if env.CurrentDeployment != nil && env.CurrentDeployment.ID == depID {
						env.CurrentDeployment.Status = "ACTIVATING"
					}
					if env.CandidateDeployment != nil && env.CandidateDeployment.ID == depID {
						env.CandidateDeployment.Status = "ACTIVATING"
					}
				}
				writeJSON(w, http.StatusOK, st.deployments[modelID][i])
				return
			}
		}
		http.Error(w, "deployment not found", http.StatusNotFound)
	})

	mux.HandleFunc("POST /v1/models/{model_id}/deployments/{dep_id}/retry", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		depID := r.PathValue("dep_id")
		st.mu.Lock()
		defer st.mu.Unlock()
		for i, d := range st.deployments[modelID] {
			if d.ID == depID {
				st.deployments[modelID][i].Status = "BUILDING"
				// Also update candidate deployment in any environment that references this deployment
				for _, env := range st.environments {
					if env.CandidateDeployment != nil && env.CandidateDeployment.ID == depID {
						env.CandidateDeployment.Status = "BUILDING"
					}
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"retried":    true,
					"reason":     "",
					"deployment": st.deployments[modelID][i],
				})
				return
			}
		}
		http.Error(w, "deployment not found", http.StatusNotFound)
	})

	mux.HandleFunc("GET /v1/models/{model_id}/environments", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		st.mu.RLock()
		defer st.mu.RUnlock()
		var envs []environment
		for key, env := range st.environments {
			if strings.HasPrefix(key, modelID+":") {
				envs = append(envs, *env)
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"environments": envs})
	})

	mux.HandleFunc("GET /v1/models/{model_id}/environments/{env_name}", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		envName := r.PathValue("env_name")
		st.mu.RLock()
		defer st.mu.RUnlock()
		env, ok := st.environments[st.envKey(modelID, envName)]
		if !ok {
			http.Error(w, "environment not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, env)
	})

	mux.HandleFunc("POST /v1/models/{model_id}/environments", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		var req struct {
			Name                string               `json:"name"`
			AutoscalingSettings *autoscalingSettings `json:"autoscaling_settings,omitempty"`
			PromotionSettings   *promotionSettings   `json:"promotion_settings,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		key := st.envKey(modelID, req.Name)
		st.environments[key] = &environment{
			Name:                req.Name,
			AutoscalingSettings: req.AutoscalingSettings,
			PromotionSettings:   req.PromotionSettings,
		}
		writeJSON(w, http.StatusCreated, st.environments[key])
	})

	mux.HandleFunc("PATCH /v1/models/{model_id}/environments/{env_name}", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		envName := r.PathValue("env_name")
		st.mu.Lock()
		defer st.mu.Unlock()
		key := st.envKey(modelID, envName)
		env, ok := st.environments[key]
		if !ok {
			http.Error(w, "environment not found", http.StatusNotFound)
			return
		}
		var req struct {
			AutoscalingSettings *autoscalingSettings `json:"autoscaling_settings,omitempty"`
			PromotionSettings   *promotionSettings   `json:"promotion_settings,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.AutoscalingSettings != nil {
			env.AutoscalingSettings = req.AutoscalingSettings
		}
		if req.PromotionSettings != nil {
			env.PromotionSettings = req.PromotionSettings
		}
		writeJSON(w, http.StatusOK, env)
	})

	mux.HandleFunc("POST /v1/models/{model_id}/environments/{env_name}/promote", func(w http.ResponseWriter, r *http.Request) {
		modelID := r.PathValue("model_id")
		envName := r.PathValue("env_name")
		var req struct {
			DeploymentID string `json:"deployment_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		st.mu.Lock()
		defer st.mu.Unlock()

		var source *deployment
		for _, d := range st.deployments[modelID] {
			if d.ID == req.DeploymentID {
				source = &d
				break
			}
		}
		if source == nil {
			http.Error(w, "deployment not found", http.StatusNotFound)
			return
		}

		promoted := deployment{
			ID:                 fmt.Sprintf("promoted-%s", req.DeploymentID),
			Name:               source.Name + ".1234567890",
			Status:             "DEPLOYING",
			ActiveReplicaCount: 0,
		}

		// Add promoted deployment to deployments list so retry API can find it
		st.deployments[modelID] = append(st.deployments[modelID], promoted)

		key := st.envKey(modelID, envName)
		env, ok := st.environments[key]
		if !ok {
			http.Error(w, "environment not found", http.StatusNotFound)
			return
		}
		env.CandidateDeployment = &promoted

		writeJSON(w, http.StatusOK, promoted)
	})

	mux.HandleFunc("POST /v1/_control", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action         string `json:"action"`
			ModelID        string `json:"model_id,omitempty"`
			EnvName        string `json:"env_name,omitempty"`
			DeploymentName string `json:"deployment_name,omitempty"`
			DeploymentID   string `json:"deployment_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		st.mu.Lock()
		defer st.mu.Unlock()

		modelID := req.ModelID
		if modelID == "" {
			modelID = "model-001"
		}
		envName := req.EnvName
		if envName == "" {
			envName = "dev"
		}

		key := st.envKey(modelID, envName)
		env := st.environments[key]

		switch req.Action {
		case "complete_promotion":
			if env != nil && env.CandidateDeployment != nil {
				env.CandidateDeployment.Status = "ACTIVE"
				env.CandidateDeployment.ActiveReplicaCount = 1
				env.CurrentDeployment = env.CandidateDeployment
				env.CandidateDeployment = nil
			}
		case "fail_promotion":
			if env != nil && env.CandidateDeployment != nil {
				env.CandidateDeployment.Status = "BUILD_FAILED"
				// Also update the deployment in the deployments list (used by GET /deployments)
				for i, d := range st.deployments[modelID] {
					if d.ID == env.CandidateDeployment.ID {
						st.deployments[modelID][i].Status = "BUILD_FAILED"
					}
				}
			}
		case "fail_promotion_stopped":
			if env != nil && env.CandidateDeployment != nil {
				env.CandidateDeployment.Status = "BUILD_STOPPED"
				for i, d := range st.deployments[modelID] {
					if d.ID == env.CandidateDeployment.ID {
						st.deployments[modelID][i].Status = "BUILD_STOPPED"
					}
				}
			}
		case "deactivate_current":
			// Simulate Baseten's TTL marking the env's current deployment as INACTIVE.
			if env != nil && env.CurrentDeployment != nil {
				env.CurrentDeployment.Status = "INACTIVE"
				env.CurrentDeployment.ActiveReplicaCount = 0
				// Also update the deployment in the deployments list (used by GET /deployments)
				for i, d := range st.deployments[modelID] {
					if d.ID == env.CurrentDeployment.ID {
						st.deployments[modelID][i].Status = "INACTIVE"
						st.deployments[modelID][i].ActiveReplicaCount = 0
					}
				}
			}
		case "complete_retry":
			// Transition candidate from failed/building back to ACTIVE (simulates successful retry)
			if env != nil && env.CandidateDeployment != nil {
				env.CandidateDeployment.Status = "ACTIVE"
				env.CandidateDeployment.ActiveReplicaCount = 1
				env.CurrentDeployment = env.CandidateDeployment
				env.CandidateDeployment = nil
			}
		case "get_deployments":
			// Return current deployment state for test verification
			deps := st.deployments[modelID]
			writeJSON(w, http.StatusOK, map[string]interface{}{"deployments": deps})
			return
		case "get_model_deletes":
			// Return the list of model IDs that have been deleted via DELETE /v1/models/{id},
			// for test assertions about cascading-delete behavior.
			writeJSON(w, http.StatusOK, map[string]interface{}{"deleted_model_ids": st.deletedModelIDs})
			return
		case "force_model_delete_failure":
			// Pre-delete the model so the next DeleteModel call returns 404.
			for i, m := range st.models {
				if m.ID == modelID {
					st.models = append(st.models[:i], st.models[i+1:]...)
					break
				}
			}
		case "setup_cleanup_test":
			// Add orphan deployments for cleanup e2e test
			// After complete_promotion on "dev", the env has a current deployment.
			// We add extra deployments that should be classified as orphans.
			deps := st.deployments[modelID]
			envStr := "dev"
			deps = append(deps,
				deployment{ID: "orphan-active-leak", Name: "img-old-wgt-old-p-old", Status: "ACTIVE", ActiveReplicaCount: 1,
					CreatedAt: "2025-01-01T00:00:00Z",
					AutoscalingSettings: &autoscalingSettings{MinReplica: 2, MaxReplica: 5, ConcurrencyTarget: 10}},
				deployment{ID: "orphan-inactive-old", Name: "img-ancient-wgt-1-p-1", Status: "INACTIVE", ActiveReplicaCount: 0,
					CreatedAt: "2025-06-01T00:00:00Z",
					AutoscalingSettings: &autoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10}},
				deployment{ID: "orphan-inactive-stale", Name: "img-stale-wgt-1-p-1", Status: "INACTIVE", ActiveReplicaCount: 0,
					CreatedAt: "2025-09-01T00:00:00Z",
					AutoscalingSettings: &autoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10}},
				deployment{ID: "orphan-scaled-zero", Name: "img-scaled-wgt-1-p-1", Status: "SCALED_TO_ZERO", ActiveReplicaCount: 0,
					CreatedAt: "2025-08-01T00:00:00Z",
					AutoscalingSettings: &autoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10}},
				deployment{ID: "protected-prod", Name: "img-prod-wgt-1-p-1", Status: "ACTIVE", ActiveReplicaCount: 1,
					IsProduction: true, CreatedAt: "2025-01-01T00:00:00Z", Environment: &envStr,
					AutoscalingSettings: &autoscalingSettings{MinReplica: 1, MaxReplica: 5, ConcurrencyTarget: 10}},
			)
			st.deployments[modelID] = deps
		case "setup_truss_deployment":
			// Seed a deployment with the given name (simulates a truss push result).
			// Used by trussConfig e2e tests where we pre-seed the expected deployment
			// so the controller finds it via FindDeploymentIDByName and skips the actual push.
			depName := req.DeploymentName
			if depName == "" {
				http.Error(w, "deployment_name required", http.StatusBadRequest)
				return
			}
			depID := req.DeploymentID
			if depID == "" {
				depID = "truss-" + depName
			}
			deps := st.deployments[modelID]
			deps = append(deps, deployment{
				ID: depID, Name: depName, Status: "ACTIVE", ActiveReplicaCount: 0,
				AutoscalingSettings: &autoscalingSettings{MinReplica: 0, MaxReplica: 5, ConcurrencyTarget: 10},
			})
			st.deployments[modelID] = deps
		case "reset":
			st.mu.Unlock()
			st.reset()
			st.mu.Lock()
		default:
			http.Error(w, "unknown action: "+req.Action, http.StatusBadRequest)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("WARN: unmatched request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	})

	log.Println("Mock Baseten API server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ERROR: failed to encode JSON: %v", err)
	}
}
