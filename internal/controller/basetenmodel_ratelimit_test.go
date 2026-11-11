package controller

import (
	"context"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	"github.com/abridgeai/baseten-operator/internal/truss"
)

var _ = Describe("Baseten API rate limits", func() {
	const (
		rlModelID   = "model-123"
		rlEnvName   = "dev"
		rlModelName = "test-model"
		rlSource    = "img-1.0-wgt-1.0-p-1.2"
		rlSourceID  = "src-456"
	)

	var (
		reconciler *BasetenModelReconciler
		mockClient *baseten.MockClient
		recorder   *record.FakeRecorder
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		mockClient = &baseten.MockClient{}
		recorder = record.NewFakeRecorder(20)
		reconciler = &BasetenModelReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			BasetenClient: mockClient,
			TrussPusher:   &truss.MockPusher{},
			Recorder:      recorder,
		}
	})

	newModel := func(name string) *modelsv1alpha1.BasetenModel {
		return &modelsv1alpha1.BasetenModel{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  "default",
				Finalizers: []string{modelsv1alpha1.FinalizerName},
			},
			Spec: modelsv1alpha1.BasetenModelSpec{
				ModelName:            rlModelName,
				SourceDeploymentName: rlSource,
				Environment: modelsv1alpha1.EnvironmentConfig{
					Name: rlEnvName,
					Autoscaling: &modelsv1alpha1.AutoscalingConfig{
						MinReplicas:       ptr(int32(0)),
						MaxReplicas:       ptr(int32(5)),
						ConcurrencyTarget: ptr(int32(10)),
					},
				},
			},
		}
	}

	reconcile := func(model *modelsv1alpha1.BasetenModel) (ctrl.Result, error) {
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		return reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace}})
	}

	reconcileByName := func(name string) (ctrl.Result, error) {
		return reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}})
	}

	cleanup := func(name string) {
		model := &modelsv1alpha1.BasetenModel{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model); err == nil {
			_ = k8sClient.Delete(ctx, model)
		}
	}

	drainEvents := func() []string {
		var events []string
		for {
			select {
			case e := <-recorder.Events:
				events = append(events, e)
			default:
				return events
			}
		}
	}

	statusOf := func(name string) modelsv1alpha1.BasetenModelStatus {
		model := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
		return model.Status
	}

	rateLimited := func(after time.Duration) error {
		err := &baseten.APIError{StatusCode: http.StatusTooManyRequests, Message: "slow down"}
		if after > 0 {
			err.RetryAfter = after
			err.HasRetryAfter = true
		}
		return err
	}

	mockFound := func() {
		mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName, team string) (string, error) {
			return rlModelID, nil
		}
	}

	mockEnv := func(current *baseten.Deployment) {
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return &baseten.Environment{
				Name:                rlEnvName,
				CurrentDeployment:   current,
				AutoscalingSettings: testAutoscalingSettings(),
			}, nil
		}
	}

	// expectSoon asserts a 429 was turned into a short requeue that does not
	// touch the failed-deployment retry budget or the workqueue error limiter.
	// It returns the events emitted during the reconcile.
	expectSoon := func(name string, result ctrl.Result, err error) []string {
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))

		status := statusOf(name)
		Expect(status.Message).To(Equal("rate limited"))
		Expect(status.DeploymentStatus).To(Equal("RateLimited"))
		Expect(status.DeploymentRetryCount).To(Equal(int32(0)))
		Expect(status.NextRetryTime).To(BeNil())

		ready := getCondition(status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		progressing := getCondition(status.Conditions, "Progressing")
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
		Expect(progressing.Reason).To(Equal("RateLimited"))

		events := drainEvents()
		Expect(events).To(ContainElement(ContainSubstring("RateLimited")))
		return events
	}

	expectHonorsRetryAfter := func(result ctrl.Result, err error, after time.Duration) {
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">=", after))
		Expect(result.RequeueAfter).To(BeNumerically("<=", after+5*time.Second))
	}

	It("retries a 429 from model lookup within a minute", func() {
		name := "rl-lookup"
		defer cleanup(name)
		mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName, team string) (string, error) {
			return "", rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		events := expectSoon(name, result, err)
		Expect(events).NotTo(ContainElement(ContainSubstring("ModelNotFound")))
	})

	It("retries a 429 from GetEnvironment within a minute", func() {
		name := "rl-get-env"
		defer cleanup(name)
		mockFound()
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
	})

	It("retries a 429 from CreateEnvironment within a minute", func() {
		name := "rl-create-env"
		defer cleanup(name)
		mockFound()
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return nil, &baseten.APIError{StatusCode: http.StatusNotFound, Message: "missing"}
		}
		mockClient.CreateEnvironmentFunc = func(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error {
			return rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
	})

	It("retries a 429 from UpdateEnvironmentSettings within a minute", func() {
		name := "rl-update-settings"
		defer cleanup(name)
		mockFound()
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return &baseten.Environment{
				Name:                rlEnvName,
				AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1, MaxReplica: 5, ConcurrencyTarget: 10},
			}, nil
		}
		mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, a *modelsv1alpha1.AutoscalingConfig, p *modelsv1alpha1.PromotionSettingsConfig) error {
			return rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
	})

	It("retries a 429 from UpdateAutoscalingSchedules within a minute", func() {
		name := "rl-update-schedules"
		model := newModel(name)
		model.Spec.Environment.AutoscalingSchedule = &modelsv1alpha1.AutoscalingScheduleConfig{
			Timezone: ptr("America/New_York"),
			Schedules: []modelsv1alpha1.AutoscalingSchedule{{
				Name:        "overnight",
				Cadence:     "DAILY",
				Weekdays:    []modelsv1alpha1.Weekday{"MONDAY"},
				StartHour:   ptr(int32(22)),
				EndHour:     ptr(int32(7)),
				Autoscaling: modelsv1alpha1.ScheduleAutoscalingConfig{MinReplicas: 1, MaxReplicas: 10},
			}},
		}
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.UpdateAutoscalingSchedulesFunc = func(ctx context.Context, modelID, envName string, spec *modelsv1alpha1.AutoscalingScheduleConfig, observed *baseten.AutoscalingSchedules) error {
			return rateLimited(0)
		}
		result, err := reconcile(model)
		expectSoon(name, result, err)
	})

	It("retries a 429 from Promote within a minute and honors Retry-After", func() {
		name := "rl-promote"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusActive, nil
		}
		mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
			return nil, rateLimited(45 * time.Second)
		}
		result, err := reconcile(newModel(name))
		expectHonorsRetryAfter(result, err, 45*time.Second)
		status := statusOf(name)
		Expect(status.Message).To(Equal("rate limited"))
		Expect(status.DeploymentRetryCount).To(Equal(int32(0)))
		Expect(status.NextRetryTime).To(BeNil())
		events := drainEvents()
		Expect(events).To(ContainElement(ContainSubstring("RateLimited")))
		Expect(events).NotTo(ContainElement(ContainSubstring("PromotionFailed")))
	})

	It("does not clamp a Retry-After longer than a minute", func() {
		name := "rl-promote-long-retry-after"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusActive, nil
		}
		mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
			return nil, rateLimited(90 * time.Second)
		}
		result, err := reconcile(newModel(name))
		expectHonorsRetryAfter(result, err, 90*time.Second)
	})

	It("retries a 429 from activating an inactive source deployment", func() {
		name := "rl-activate-source"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusInactive, nil
		}
		mockClient.ActivateDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) error {
			return rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
	})

	It("retries a 429 from reactivating the current deployment", func() {
		name := "rl-activate-current"
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{ID: "cur-1", Name: rlSource + ".123", Status: baseten.DeploymentStatusInactive})
		mockClient.ActivateDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) error {
			Expect(deploymentID).To(Equal("cur-1"))
			return rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
	})

	It("retries a 429 from the deployment retry API without consuming the retry budget", func() {
		name := "rl-retry-api"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusFailed, nil
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		events := expectSoon(name, result, err)
		Expect(events).NotTo(ContainElement(ContainSubstring("DeploymentRetryFailed")))
	})

	It("does not push NextRetryTime out when the retry API returns 429", func() {
		name := "rl-retry-keeps-next"
		model := newModel(name)
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())

		pastRetry := metav1.NewTime(time.Now().Add(-time.Minute))
		firstFailure := metav1.NewTime(time.Now().Add(-30 * time.Minute))
		model.Status.DeploymentRetryCount = 2
		model.Status.NextRetryTime = &pastRetry
		model.Status.FirstDeploymentFailureTime = &firstFailure
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusFailed, nil
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			return nil, rateLimited(0)
		}

		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))

		status := statusOf(name)
		Expect(status.DeploymentRetryCount).To(Equal(int32(2)))
		Expect(status.NextRetryTime).NotTo(BeNil())
		Expect(status.NextRetryTime.Time).To(BeTemporally("~", pastRetry.Time, time.Second))
		Expect(status.Message).To(Equal("rate limited"))
	})

	It("retries a 429 from source deployment lookup without entering the retry budget", func() {
		name := "rl-find-deployment"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		retryCalled := false
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return "", "", rateLimited(0)
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			retryCalled = true
			return nil, nil
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
		Expect(retryCalled).To(BeFalse())
	})

	It("retries a 429 from the pre-retry status check without calling retry or bumping the count", func() {
		name := "rl-retry-recheck"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		calls := 0
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			calls++
			if calls == 1 {
				return rlSourceID, baseten.DeploymentStatusFailed, nil
			}
			return "", "", rateLimited(0)
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			Fail("retry API must not be called when the status re-check is rate limited")
			return nil, nil
		}
		result, err := reconcile(newModel(name))
		expectSoon(name, result, err)
	})

	It("retries a 429 from truss deployment lookup within a minute", func() {
		name := "rl-truss-lookup"
		model := newModel(name)
		model.Spec.SourceDeploymentName = ""
		model.Spec.TrussConfig = &modelsv1alpha1.TrussConfig{
			PythonVersion: "py312",
			Resources:     modelsv1alpha1.TrussResources{Accelerator: "H100:1"},
			BaseImage:     modelsv1alpha1.TrussBaseImage{Image: "test:latest"},
		}
		defer cleanup(name)
		mockFound()
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return "", "", rateLimited(0)
		}
		result, err := reconcile(model)
		expectSoon(name, result, err)
		Expect(statusOf(name).TrussPushStatus).To(BeEmpty())
	})

	It("retries a 429 from observe-mode model lookup within a minute", func() {
		name := "rl-observe-lookup"
		model := newModel(name)
		model.Spec.Mode = modelsv1alpha1.ModeObserve
		defer cleanup(name)
		mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName, team string) (string, error) {
			return "", rateLimited(0)
		}
		result, err := reconcile(model)
		expectSoon(name, result, err)
	})

	It("retries a 429 from observe-mode GetEnvironment within a minute", func() {
		name := "rl-observe-env"
		model := newModel(name)
		model.Spec.Mode = modelsv1alpha1.ModeObserve
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.ModelID = rlModelID
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcileByName(name)
		expectSoon(name, result, err)
	})

	It("retries a 429 from DeleteModel within a minute and keeps the finalizer", func() {
		name := "rl-delete-model"
		model := newModel(name)
		model.Spec.DeletionPolicy = modelsv1alpha1.DeletionPolicyDelete
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.ModelID = rlModelID
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		Expect(k8sClient.Delete(ctx, got)).To(Succeed())

		mockClient.DeleteModelFunc = func(ctx context.Context, modelID string) error {
			return rateLimited(0)
		}
		result, err := reconcileByName(name)
		expectSoon(name, result, err)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		Expect(got.Finalizers).To(ContainElement(modelsv1alpha1.FinalizerName))
		Expect(got.Status.DeploymentStatus).NotTo(Equal(statusDeleteFailed))
	})

	It("retries a 429 from delete guardrail listing without blocking deletion", func() {
		name := "rl-delete-guardrails"
		model := newModel(name)
		model.Spec.DeletionPolicy = modelsv1alpha1.DeletionPolicyDeleteWithGuardrails
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.ModelID = rlModelID
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		Expect(k8sClient.Delete(ctx, got)).To(Succeed())

		mockClient.ListEnvironmentsFunc = func(ctx context.Context, modelID string) ([]baseten.Environment, error) {
			return nil, rateLimited(0)
		}
		mockClient.DeleteModelFunc = func(ctx context.Context, modelID string) error {
			Fail("DeleteModel must not run while listing environments is rate limited")
			return nil
		}
		result, err := reconcileByName(name)
		expectSoon(name, result, err)
		Expect(statusOf(name).DeploymentStatus).NotTo(Equal(statusDeleteBlocked))
	})

	It("retries a 429 from orphan deployment listing within a minute", func() {
		name := "rl-orphan-list"
		model := newModel(name)
		model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
			ScaleToZero:     ptr(true),
			IntervalMinutes: ptr(int32(10)),
		}
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcile(model)
		expectSoon(name, result, err)
	})

	It("retries a 429 from scaling in an orphan deployment", func() {
		name := "rl-orphan-scale"
		model := newModel(name)
		model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
			ScaleToZero:     ptr(true),
			IntervalMinutes: ptr(int32(10)),
		}
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
			return []baseten.DeploymentDetail{{
				ID:                  "orphan-1",
				Name:                "old-deploy",
				Status:              baseten.DeploymentStatusActive,
				AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 2},
			}}, nil
		}
		mockClient.ListEnvironmentsFunc = func(ctx context.Context, modelID string) ([]baseten.Environment, error) {
			return nil, nil
		}
		mockClient.UpdateDeploymentAutoscalingFunc = func(ctx context.Context, modelID, deploymentID string, minReplica int32) error {
			return rateLimited(0)
		}
		result, err := reconcile(model)
		expectSoon(name, result, err)
	})

	It("retries a 429 from deleting a stale orphan deployment", func() {
		name := "rl-orphan-delete"
		model := newModel(name)
		model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
			Delete:          ptr(true),
			DeleteAfterDays: ptr(int32(1)),
			MinToKeep:       ptr(int32(0)),
			IntervalMinutes: ptr(int32(10)),
		}
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
			return []baseten.DeploymentDetail{{
				ID:        "orphan-old",
				Name:      "old-deploy",
				Status:    baseten.DeploymentStatusInactive,
				CreatedAt: time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339),
			}}, nil
		}
		mockClient.ListEnvironmentsFunc = func(ctx context.Context, modelID string) ([]baseten.Environment, error) {
			return nil, nil
		}
		mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) error {
			return rateLimited(0)
		}
		result, err := reconcile(model)
		expectSoon(name, result, err)
	})

	It("still returns a non-429 promote error to the caller", func() {
		name := "rl-promote-500"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusActive, nil
		}
		mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
			return nil, &baseten.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
		}
		_, err := reconcile(newModel(name))
		Expect(err).To(HaveOccurred())
		events := drainEvents()
		Expect(events).To(ContainElement(ContainSubstring("PromotionFailed")))
		Expect(events).NotTo(ContainElement(ContainSubstring("RateLimited")))
	})
})
