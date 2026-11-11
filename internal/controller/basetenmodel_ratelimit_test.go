package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	trussgo "github.com/basetenlabs/truss-go"
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
		Expect(status.DeploymentStatus).To(Equal(statusRateLimited))
		Expect(status.DeploymentRetryCount).To(Equal(int32(0)))
		Expect(status.NextRetryTime).To(BeNil())

		ready := getCondition(status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		progressing := getCondition(status.Conditions, "Progressing")
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
		Expect(progressing.Reason).To(Equal(statusRateLimited))

		events := drainEvents()
		Expect(events).To(ContainElement(ContainSubstring("RateLimited")))
		return events
	}

	expectHonorsRetryAfter := func(result ctrl.Result, err error, after time.Duration) {
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">=", after))
		Expect(result.RequeueAfter).To(BeNumerically("<=", after+5*time.Second))
	}

	It("writes one initial status when model lookup is rate limited on a new CR", func() {
		name := "rl-lookup"
		defer cleanup(name)
		mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName, team string) (string, error) {
			return "", rateLimited(0)
		}
		result, err := reconcile(newModel(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(statusRateLimited))
		Expect(status.Message).To(Equal(rateLimitMessage))
		ready := getCondition(status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(statusRateLimited))
		events := drainEvents()
		Expect(events).To(ContainElement(ContainSubstring("RateLimited")))
		Expect(events).NotTo(ContainElement(ContainSubstring("ModelNotFound")))

		// A second 429 does not write a different status.
		result, err = reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		again := statusOf(name)
		Expect(again.DeploymentStatus).To(Equal(statusRateLimited))
		Expect(again.Message).To(Equal(rateLimitMessage))
	})

	It("keeps Ready when GetEnvironment is rate limited on a serving model", func() {
		name := "rl-get-env"
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 2})
		_, err := reconcile(newModel(name))
		Expect(err).NotTo(HaveOccurred())
		seed := statusOf(name)
		Expect(getCondition(seed.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		drainEvents()

		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(seed.DeploymentStatus))
		Expect(status.Message).To(Equal(seed.Message))
		ready := getCondition(status.Conditions, "Ready")
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(baseten.DeploymentStatusActive))
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
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

	It("keeps Ready when a settings-drift update is rate limited", func() {
		name := "rl-update-settings"
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		_, err := reconcile(newModel(name))
		Expect(err).NotTo(HaveOccurred())
		seed := statusOf(name)
		drainEvents()

		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return &baseten.Environment{
				Name:                rlEnvName,
				CurrentDeployment:   &baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1},
				AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1, MaxReplica: 5, ConcurrencyTarget: 10},
			}, nil
		}
		mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, a *modelsv1alpha1.AutoscalingConfig, p *modelsv1alpha1.PromotionSettingsConfig) error {
			return rateLimited(0)
		}
		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(seed.DeploymentStatus))
		Expect(status.Message).To(Equal(seed.Message))
		Expect(getCondition(status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
	})

	It("retries a 429 from UpdateAutoscalingSchedules without marking the model rate limited", func() {
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
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		Expect(statusOf(name).DeploymentStatus).NotTo(Equal(statusRateLimited))
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
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
		Expect(statusOf(name).FirstDeploymentFailureTime).To(BeNil())
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
		Expect(status.FirstDeploymentFailureTime.Time).To(BeTemporally("~", firstFailure.Add(result.RequeueAfter), 2*time.Second))
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
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		Expect(statusOf(name).DeploymentStatus).NotTo(Equal(statusRateLimited))
		Expect(retryCalled).To(BeFalse())
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
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

	It("writes one initial status when observe-mode model lookup is rate limited on a new CR", func() {
		name := "rl-observe-lookup"
		model := newModel(name)
		model.Spec.Mode = modelsv1alpha1.ModeObserve
		defer cleanup(name)
		mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName, team string) (string, error) {
			return "", rateLimited(0)
		}
		result, err := reconcile(model)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(statusRateLimited))
		Expect(getCondition(status.Conditions, "Ready")).NotTo(BeNil())
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
	})

	It("advances Ready observedGeneration when the spec changes and the deployment stays active", func() {
		name := "rl-observed-generation"
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		_, err := reconcile(newModel(name))
		Expect(err).NotTo(HaveOccurred())
		first := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, first)).To(Succeed())
		ready := getCondition(first.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.ObservedGeneration).To(Equal(first.Generation))

		oldGeneration := first.Generation
		first.Spec.Team = "Evals"
		Expect(k8sClient.Update(ctx, first)).To(Succeed())
		_, err = reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		second := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, second)).To(Succeed())
		Expect(second.Generation).NotTo(Equal(oldGeneration))
		ready = getCondition(second.Status.Conditions, "Ready")
		Expect(ready.ObservedGeneration).To(Equal(second.Generation))
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		progressing := getCondition(second.Status.Conditions, "Progressing")
		Expect(progressing.ObservedGeneration).To(Equal(second.Generation))
	})

	It("retries a 429 from observe-mode GetEnvironment without marking the model rate limited", func() {
		name := "rl-observe-env"
		model := newModel(name)
		model.Spec.Mode = modelsv1alpha1.ModeObserve
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		model.Status.ModelID = rlModelID
		model.Status.DeploymentStatus = baseten.DeploymentStatusActive
		model.Status.Message = "active: kept"
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(status.Message).To(Equal("active: kept"))
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
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
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(getCondition(status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(status.LastCleanupTime).To(BeNil())
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
	})

	It("keeps Ready when ListEnvironments is rate limited during orphan cleanup", func() {
		name := "rl-orphan-list-envs"
		model := newModel(name)
		model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
			ScaleToZero:     ptr(true),
			IntervalMinutes: ptr(int32(10)),
		}
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
			return []baseten.DeploymentDetail{{ID: "orphan-1", Name: "old-deploy", Status: baseten.DeploymentStatusInactive}}, nil
		}
		mockClient.ListEnvironmentsFunc = func(ctx context.Context, modelID string) ([]baseten.Environment, error) {
			return nil, rateLimited(0)
		}
		result, err := reconcile(model)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(status.Message).To(ContainSubstring("active:"))
		Expect(getCondition(status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(getCondition(status.Conditions, "Ready").Reason).NotTo(Equal(statusRateLimited))
		Expect(status.LastCleanupTime).To(BeNil())
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
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
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(getCondition(status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(status.LastCleanupTime).To(BeNil())
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
	})

	It("keeps orphan progress and LastCleanupTime when a later scale-in is rate limited", func() {
		name := "rl-orphan-partial"
		model := newModel(name)
		model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
			ScaleToZero:     ptr(true),
			IntervalMinutes: ptr(int32(10)),
		}
		defer cleanup(name)
		mockFound()
		mockEnv(&baseten.Deployment{Name: rlSource + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1})
		mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
			return []baseten.DeploymentDetail{
				{ID: "orphan-a", Name: "old-a", Status: baseten.DeploymentStatusActive, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 2}},
				{ID: "orphan-b", Name: "old-b", Status: baseten.DeploymentStatusActive, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 2}},
			}, nil
		}
		mockClient.ListEnvironmentsFunc = func(ctx context.Context, modelID string) ([]baseten.Environment, error) {
			return nil, nil
		}
		calls := 0
		mockClient.UpdateDeploymentAutoscalingFunc = func(ctx context.Context, modelID, deploymentID string, minReplica int32) error {
			calls++
			if deploymentID == "orphan-a" {
				return nil
			}
			return rateLimited(0)
		}
		result, err := reconcile(model)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		Expect(calls).To(Equal(2))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(getCondition(status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(status.LastCleanupTime).NotTo(BeNil())
		events := drainEvents()
		Expect(events).To(ContainElement(ContainSubstring("OrphanDeploymentsScaledIn")))
		Expect(events).To(ContainElement(ContainSubstring("old-a")))
		Expect(events).To(ContainElement(ContainSubstring("RateLimited")))
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
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		status := statusOf(name)
		Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(getCondition(status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(status.LastCleanupTime).To(BeNil())
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
	})

	It("caps how far one long Retry-After pushes the failure clock", func() {
		name := "rl-fold-cap"
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, newModel(name))).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		ft := metav1.NewTime(started)
		model := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
		model.Status.ModelID = rlModelID
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusActive, nil
		}
		mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
			return nil, &baseten.APIError{
				StatusCode: 429, Message: "slow down", HasRetryAfter: true, RetryAfter: time.Hour,
			}
		}
		_, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		status := statusOf(name)
		drift := status.FirstDeploymentFailureTime.Sub(started)
		Expect(drift).To(BeNumerically("~", rateLimitFoldCap, 2*time.Second))
		Expect(status.RateLimitWindowEnd).NotTo(BeNil())
		Expect(status.RateLimitWindowEnd.Time).To(BeTemporally(">", time.Now().Add(30*time.Minute)))
	})

	It("waits out an open rate-limit window without exhausting retries or calling Baseten", func() {
		name := "rl-window-open"
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, newModel(name))).To(Succeed())
		model := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
		past := metav1.NewTime(time.Now().Add(-2*time.Hour - time.Minute))
		windowEnd := time.Now().Add(50 * time.Minute)
		window := metav1.NewTime(windowEnd)
		model.Status.ModelID = rlModelID
		model.Status.FirstDeploymentFailureTime = &past
		model.Status.RateLimitWindowEnd = &window
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		called := false
		mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName, team string) (string, error) {
			called = true
			return "", fmt.Errorf("lookup")
		}
		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			called = true
			return nil, fmt.Errorf("getenv")
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			called = true
			return nil, fmt.Errorf("retry")
		}

		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(called).To(BeFalse())
		Expect(result.RequeueAfter).To(BeNumerically("~", time.Until(windowEnd), 2*time.Second))
		Expect(statusOf(name).Message).NotTo(ContainSubstring("retries exhausted"))
		Expect(drainEvents()).NotTo(ContainElement(ContainSubstring("DeploymentRetryExhausted")))
	})

	It("exhausts retries once a rate-limit window has passed and the deadline is exceeded", func() {
		name := "rl-window-passed"
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, newModel(name))).To(Succeed())
		model := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
		past := metav1.NewTime(time.Now().Add(-3 * time.Hour))
		window := metav1.NewTime(time.Now().Add(-time.Minute))
		model.Status.ModelID = rlModelID
		model.Status.FirstDeploymentFailureTime = &past
		model.Status.RateLimitWindowEnd = &window
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			return &baseten.Environment{
				Name: rlEnvName,
				CandidateDeployment: &baseten.Deployment{
					ID: "cand-123", Name: rlSource + ".123", Status: baseten.DeploymentStatusBuildFailed,
				},
				AutoscalingSettings: testAutoscalingSettings(),
			}, nil
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			Fail("retry API should not be called after the deadline")
			return nil, nil
		}

		_, err := reconcileByName(name)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("retries exhausted"))
	})

	It("waits at least one second when Retry-After is zero", func() {
		name := "rl-floor"
		defer cleanup(name)
		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusActive, nil
		}
		mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
			return nil, &baseten.APIError{StatusCode: http.StatusTooManyRequests, Message: "slow down", HasRetryAfter: true, RetryAfter: 0}
		}
		result, err := reconcile(newModel(name))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">=", time.Second))
		Expect(result.RequeueAfter).To(BeNumerically("<=", 5*time.Second))
		Expect(statusOf(name).DeploymentStatus).To(Equal(statusRateLimited))
	})

	It("resets the deployment failure clock when mode leaves Pause", func() {
		name := "rl-pause-resume"
		model := newModel(name)
		model.Spec.Mode = modelsv1alpha1.ModePause
		defer cleanup(name)
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		stale := metav1.NewTime(time.Now().Add(-3 * time.Hour))
		model.Status.FirstDeploymentFailureTime = &stale
		model.Status.DeploymentRetryCount = 4
		Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

		_, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		paused := statusOf(name)
		Expect(paused.DeploymentStatus).To(Equal(statusPaused))
		Expect(paused.FirstDeploymentFailureTime.Time).To(BeTemporally("~", stale.Time, time.Second))

		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
		got.Spec.Mode = modelsv1alpha1.ModeReconcile
		Expect(k8sClient.Update(ctx, got)).To(Succeed())

		mockFound()
		mockEnv(nil)
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return rlSourceID, baseten.DeploymentStatusFailed, nil
		}
		mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			return &baseten.RetryResponse{Retried: true}, nil
		}
		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		status := statusOf(name)
		Expect(status.FirstDeploymentFailureTime).NotTo(BeNil())
		Expect(status.FirstDeploymentFailureTime.Time).To(BeTemporally("~", time.Now(), time.Minute))
		Expect(status.Message).NotTo(ContainSubstring("retries exhausted"))
	})

	It("treats a truss push 429 as a rate limit instead of the push failure backoff", func() {
		name := "rl-truss-push"
		model := newModel(name)
		model.Spec.SourceDeploymentName = ""
		model.Spec.TrussConfig = &modelsv1alpha1.TrussConfig{
			PythonVersion: "py312",
			Resources:     modelsv1alpha1.TrussResources{Accelerator: "H100:1"},
			BaseImage:     modelsv1alpha1.TrussBaseImage{Image: "test:latest"},
		}
		defer cleanup(name)
		mockFound()
		var pushes int
		mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
			return "", "", nil
		}
		reconciler.TrussPusher = &truss.MockPusher{
			PushFromConfigFunc: func(ctx context.Context, configYAML, setupScript []byte, modelName, team, deploymentName string) (*truss.PushResult, error) {
				pushes++
				return nil, fmt.Errorf("truss push failed: %w", &trussgo.APIError{StatusCode: http.StatusTooManyRequests, Message: "slow down"})
			},
		}
		_, err := reconcile(model)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			status := statusOf(name)
			g.Expect(status.DeploymentStatus).To(Equal(statusRateLimited))
			g.Expect(status.TrussPushFailureCount).To(Equal(int32(0)))
			g.Expect(status.TrussPushNextRetryTime).NotTo(BeNil())
			g.Expect(time.Until(status.TrussPushNextRetryTime.Time)).To(BeNumerically(">", 0))
			g.Expect(time.Until(status.TrussPushNextRetryTime.Time)).To(BeNumerically("<=", time.Minute))
		}, 2*time.Second, 50*time.Millisecond).Should(Succeed())
		Expect(pushes).To(Equal(1))

		result, err := reconcileByName(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
		Expect(pushes).To(Equal(1))
		Expect(statusOf(name).TrussPushFailureCount).To(Equal(int32(0)))
		Expect(drainEvents()).To(ContainElement(ContainSubstring("RateLimited")))
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
