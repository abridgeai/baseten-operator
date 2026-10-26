package controller

import (
	"context"
	"errors"
	"testing"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func cancellationFixture(t *testing.T) (*BasetenModelReconciler, *modelsv1alpha1.BasetenModel, *baseten.MockClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := modelsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	m := &modelsv1alpha1.BasetenModel{ObjectMeta: metav1.ObjectMeta{Name: "cancellation", Namespace: "default", Generation: 2},
		Spec: modelsv1alpha1.BasetenModelSpec{Mode: modelsv1alpha1.ModeReconcile, ModelName: "model", SourceDeploymentName: "source", DeletionPolicy: modelsv1alpha1.DeletionPolicyRetain,
			PromotionCancellation: &modelsv1alpha1.PromotionCancellation{ModelID: "model1", EnvironmentName: "dev", CandidateDeploymentID: "candidate"},
			Environment:           modelsv1alpha1.EnvironmentConfig{Name: "dev", Autoscaling: &modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(0))}}},
		Status: modelsv1alpha1.BasetenModelStatus{ModelID: "model1", CandidateDeploymentID: "candidate"}}
	c := &baseten.MockClient{}
	r := &BasetenModelReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(m).WithObjects(m).Build(), BasetenClient: c, Recorder: record.NewFakeRecorder(100)}
	return r, m, c
}

func TestPromotionCancellationOrdersCancellationBeforeSettingsAndSuppressesPromotion(t *testing.T) {
	r, m, c := cancellationFixture(t)
	env := &baseten.Environment{Name: "dev", CandidateDeployment: &baseten.Deployment{ID: "candidate"}, PromotionStatus: "RELEASING", AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1}}
	c.GetEnvironmentFunc = func(context.Context, string, string) (*baseten.Environment, error) { return env, nil }
	calls := []string{}
	c.CancelPromotionFunc = func(_ context.Context, model, environment string) error {
		if model != "model1" || environment != "dev" {
			t.Fatal("wrong cancellation target")
		}
		calls = append(calls, "cancel")
		env.PromotionStatus = "RAMPING_DOWN"
		return nil
	}
	c.UpdateEnvironmentSettingsFunc = func(_ context.Context, _, _ string, a *modelsv1alpha1.AutoscalingConfig, p *modelsv1alpha1.PromotionSettingsConfig) error {
		if env.CandidateDeployment != nil || env.PromotionStatus != "" {
			t.Fatal("floor before cancellation completed")
		}
		if a.MinReplicas == nil || *a.MinReplicas != 0 || a.MaxReplicas != nil || p != nil {
			t.Fatalf("unexpected settings: %+v %+v", a, p)
		}
		calls = append(calls, "floor")
		env.AutoscalingSettings.MinReplica = 0
		return nil
	}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: m.Name, Namespace: m.Namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	reconcile() // cancellation acceptance is not completion; no repeat cancellation
	if len(calls) != 1 {
		t.Fatalf("calls=%v", calls)
	}
	env.CandidateDeployment = nil
	env.PromotionStatus = ""
	reconcile()
	reconcile()
	reconcile() // no promotion or source push after completion
	if len(calls) != 2 || calls[0] != "cancel" || calls[1] != "floor" {
		t.Fatalf("calls=%v", calls)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, m); err != nil {
		t.Fatal(err)
	}
	if m.Status.DeploymentStatus != statusPromotionCanceled || m.Status.CandidateDeploymentID != "candidate" {
		t.Fatalf("status=%+v", m.Status)
	}
	ready := getCondition(m.Status.Conditions, conditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != 2 {
		t.Fatalf("condition=%+v", ready)
	}
}

func TestPromotionCancellationRefusesUnsafeMutations(t *testing.T) {
	for _, tc := range []string{"unknown model", "different environment", "different candidate", "candidate became current", "unowned candidate", "push in flight", "observer", "cancel rejected"} {
		t.Run(tc, func(t *testing.T) {
			r, m, c := cancellationFixture(t)
			env := &baseten.Environment{Name: "dev", CandidateDeployment: &baseten.Deployment{ID: "candidate"}, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1}}
			cancelCalls := 0
			switch tc {
			case "unknown model":
				m.Status.ModelID = ""
			case "different environment":
				m.Spec.Environment.Name = "prod"
			case "different candidate":
				env.CandidateDeployment.ID = "other"
			case "candidate became current":
				env.CurrentDeployment = &baseten.Deployment{ID: "candidate"}
			case "unowned candidate":
				m.Status.CandidateDeploymentID = "other"
			case "push in flight":
				m.Status.TrussPushStatus = statusTrussPushing
			case "observer":
				m.Spec.Mode = modelsv1alpha1.ModeObserve
			}
			c.GetEnvironmentFunc = func(context.Context, string, string) (*baseten.Environment, error) { return env, nil }
			c.CancelPromotionFunc = func(context.Context, string, string) error { cancelCalls++; return errors.New("cancel rejected") }
			c.UpdateEnvironmentSettingsFunc = func(context.Context, string, string, *modelsv1alpha1.AutoscalingConfig, *modelsv1alpha1.PromotionSettingsConfig) error {
				t.Fatal("unsafe floor")
				return nil
			}
			_, err := r.reconcilePromotionCancellation(context.Background(), m)
			if tc == "cancel rejected" {
				if err == nil || cancelCalls != 1 {
					t.Fatalf("err=%v calls=%d", err, cancelCalls)
				}
			} else if err != nil || cancelCalls != 0 {
				t.Fatalf("err=%v calls=%d", err, cancelCalls)
			}
		})
	}
}

func TestPromotionCancellationAcknowledgesNewGenerationAndReportsLiveReplicas(t *testing.T) {
	r, m, c := cancellationFixture(t)
	env := &baseten.Environment{Name: "dev", CurrentDeployment: &baseten.Deployment{ID: "predecessor", ActiveReplicaCount: 2}, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}}
	c.GetEnvironmentFunc = func(context.Context, string, string) (*baseten.Environment, error) { return env, nil }
	if _, err := r.reconcilePromotionCancellation(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if m.Status.ActiveReplicaCount != 2 {
		t.Fatalf("replicas=%d", m.Status.ActiveReplicaCount)
	}
	m.Generation++
	if err := r.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcilePromotionCancellation(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, m); err != nil {
		t.Fatal(err)
	}
	if ready := getCondition(m.Status.Conditions, conditionReady); ready == nil || ready.ObservedGeneration != m.Generation {
		t.Fatalf("condition=%+v generation=%d", ready, m.Generation)
	}
}

func TestPromotionCancellationHonorsPauseAndRetainDeletion(t *testing.T) {
	for _, tc := range []string{"pause", "legacy pause", "retain deletion"} {
		t.Run(tc, func(t *testing.T) {
			r, m, _ := cancellationFixture(t)
			m.Finalizers = []string{modelsv1alpha1.FinalizerName}
			if tc == "pause" {
				m.Spec.Mode = modelsv1alpha1.ModePause
			}
			if tc == "legacy pause" {
				m.Spec.Paused = true //nolint:staticcheck // verify legacy kill switch
			}
			m.Spec.PromotionCancellation.ModelID = "stale-model"
			if err := r.Update(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			if tc == "retain deletion" {
				if err := r.Delete(context.Background(), m); err != nil {
					t.Fatal(err)
				}
			}
			key := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
			// Every Baseten mock is unset: any external call fails this test.
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			err := r.Get(context.Background(), key, m)
			if tc == "retain deletion" {
				if err == nil {
					t.Fatal("Retain deletion blocked by cancellation")
				}
			} else if err != nil || m.Status.DeploymentStatus != statusPaused {
				t.Fatalf("err=%v status=%s", err, m.Status.DeploymentStatus)
			}
		})
	}
}

func TestPromotionCancellationPreservesServingCapacity(t *testing.T) {
	r, m, c := cancellationFixture(t)
	m.Spec.Environment.Autoscaling.MinReplicas = ptr(int32(3))
	if err := r.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	env := &baseten.Environment{Name: "dev", CandidateDeployment: &baseten.Deployment{ID: "candidate"}, CurrentDeployment: &baseten.Deployment{ID: "serving", ActiveReplicaCount: 3}, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 3}}
	c.GetEnvironmentFunc = func(context.Context, string, string) (*baseten.Environment, error) { return env, nil }
	c.CancelPromotionFunc = func(context.Context, string, string) error { env.CandidateDeployment = nil; return nil }
	if _, err := r.reconcilePromotionCancellation(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcilePromotionCancellation(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if env.AutoscalingSettings.MinReplica != 3 || m.Status.ActiveReplicaCount != 3 {
		t.Fatal("cancellation altered serving capacity")
	}
	// A later candidate must never be canceled by this pinned request.
	env.CandidateDeployment = &baseten.Deployment{ID: "later-candidate"}
	c.CancelPromotionFunc = func(context.Context, string, string) error { t.Fatal("canceled another attempt"); return nil }
	if _, err := r.reconcilePromotionCancellation(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func TestPromotionCancellationObserveNeverWrites(t *testing.T) {
	r, m, c := cancellationFixture(t)
	m.Spec.Mode = modelsv1alpha1.ModeObserve
	if err := r.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	env := &baseten.Environment{Name: "dev", CandidateDeployment: &baseten.Deployment{ID: "candidate"}, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 3}}
	c.GetEnvironmentFunc = func(context.Context, string, string) (*baseten.Environment, error) { return env, nil }
	key := types.NamespacedName{Name: m.Name, Namespace: m.Namespace}
	// All mutation mocks remain unset, including the settings PATCH despite spec min zero.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	env.CandidateDeployment = nil
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if env.AutoscalingSettings.MinReplica != 3 {
		t.Fatal("observer changed environment settings")
	}
}
