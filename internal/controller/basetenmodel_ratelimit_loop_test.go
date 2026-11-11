package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	"github.com/abridgeai/baseten-operator/internal/truss"
)

// These tests run the real controller manager against envtest. A status write
// requeues immediately, so a 429 that updates status on every pass hot-loops
// and ignores RequeueAfter. A permanent 429 must stay O(1) calls per wait.
var _ = Describe("Baseten API rate limit manager loop", func() {
	const (
		loopModelID  = "model-loop"
		loopEnv      = "dev"
		loopSource   = "img-1.0-wgt-1.0-p-1.2"
		loopSourceID = "src-loop"
		// Short enough that a hot loop shows hundreds of calls, long enough
		// that a fixed RequeueAfter of ~2s+jitter still fits a couple of tries.
		loopWindow      = 2 * time.Second
		loopRetryAfter  = 2 * time.Second
		loopCallCeiling = 8
		loopKeptMessage = "active: kept"
	)

	rateLimited := func() error {
		return &baseten.APIError{
			StatusCode:    429,
			Message:       "slow down",
			HasRetryAfter: true,
			RetryAfter:    loopRetryAfter,
		}
	}

	newModel := func(ns string) *modelsv1alpha1.BasetenModel {
		return &modelsv1alpha1.BasetenModel{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "loop",
				Namespace:  ns,
				Finalizers: []string{modelsv1alpha1.FinalizerName},
			},
			Spec: modelsv1alpha1.BasetenModelSpec{
				ModelName:            "loop-model",
				SourceDeploymentName: loopSource,
				Environment: modelsv1alpha1.EnvironmentConfig{
					Name: loopEnv,
					Autoscaling: &modelsv1alpha1.AutoscalingConfig{
						MinReplicas:       ptr(int32(0)),
						MaxReplicas:       ptr(int32(5)),
						ConcurrencyTarget: ptr(int32(10)),
					},
				},
			},
		}
	}

	var managers atomic.Int32

	startManager := func(cancel context.CancelFunc, ctx context.Context, ns string, mc *baseten.MockClient, window time.Duration) {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(),
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
			HealthProbeBindAddress: "0",
			PprofBindAddress:       "0",
		})
		Expect(err).NotTo(HaveOccurred())

		rec := record.NewFakeRecorder(256)
		go func() {
			for {
				select {
				case <-rec.Events:
				case <-ctx.Done():
					return
				}
			}
		}()
		reconciler := &BasetenModelReconciler{
			Client:                  mgr.GetClient(),
			Scheme:                  mgr.GetScheme(),
			BasetenClient:           mc,
			TrussPusher:             &truss.MockPusher{},
			Recorder:                rec,
			MaxConcurrentReconciles: 5,
			watchNamespace:          ns,
			controllerName:          fmt.Sprintf("basetenmodel-%s-%d", ns, managers.Add(1)),
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		done := make(chan struct{})
		go func() {
			defer close(done)
			err := mgr.Start(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				Expect(err).NotTo(HaveOccurred())
			}
		}()
		Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
		time.Sleep(window)
		cancel()
		Eventually(done).WithTimeout(10 * time.Second).Should(BeClosed())
	}

	prepareNS := func(name string) string {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(k8sClient.Create(context.Background(), ns)).To(Succeed())
		return name
	}

	It("does not hot-loop a housekeeping GetEnvironment 429 while the failure clock is set", func() {
		ns := prepareNS("rl-loop-getenv")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		model.Status.DeploymentStatus = baseten.DeploymentStatusActive
		model.Status.Message = loopKeptMessage
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				calls.Add(1)
				return nil, rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "GetEnvironment calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(got.Status.Message).To(Equal(loopKeptMessage))
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally("~", started, 2*time.Second))
	})

	It("does not hot-loop a promote 429", func() {
		ns := prepareNS("rl-loop-promote")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				return &baseten.Environment{Name: loopEnv, AutoscalingSettings: testAutoscalingSettings()}, nil
			},
			FindDeploymentIDByNameFunc: func(ctx context.Context, modelID, depName string) (string, string, error) {
				return loopSourceID, baseten.DeploymentStatusActive, nil
			},
			PromoteFunc: func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
				calls.Add(1)
				return nil, rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "Promote calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(statusRateLimited))
		// At most one wait per Retry-After window, not hours of stacked delays.
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally(">", started.Add(-time.Second)))
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally("<", started.Add(30*time.Second)))
	})

	It("does not hot-loop a delete 429", func() {
		ns := prepareNS("rl-loop-delete")
		model := newModel(ns)
		model.Spec.DeletionPolicy = modelsv1alpha1.DeletionPolicyDelete
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())
		Expect(k8sClient.Delete(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			DeleteModelFunc: func(ctx context.Context, modelID string) error {
				calls.Add(1)
				return rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "DeleteModel calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(statusRateLimited))
		Expect(got.Status.DeploymentStatus).NotTo(Equal(statusDeleting))
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally("<", started.Add(30*time.Second)))
	})

	It("writes an initial status once when a new model lookup stays rate limited", func() {
		ns := prepareNS("rl-loop-lookup")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			FindModelIDByNameFunc: func(ctx context.Context, modelName, team string) (string, error) {
				calls.Add(1)
				return "", rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "FindModelID calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(statusRateLimited))
		Expect(got.Status.Message).To(Equal(rateLimitMessage))
		ready := getCondition(got.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.ObservedGeneration).To(Equal(got.Generation))
	})

	It("does not hot-loop a settings-drift 429 on a serving model", func() {
		ns := prepareNS("rl-loop-settings")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		model.Status.DeploymentStatus = baseten.DeploymentStatusActive
		model.Status.Message = loopKeptMessage
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				return &baseten.Environment{
					Name: loopEnv,
					CurrentDeployment: &baseten.Deployment{
						Name: loopSource + ".1", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1,
					},
					AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1, MaxReplica: 5, ConcurrencyTarget: 10},
				}, nil
			},
			UpdateEnvironmentSettingsFunc: func(ctx context.Context, modelID, envName string, a *modelsv1alpha1.AutoscalingConfig, p *modelsv1alpha1.PromotionSettingsConfig) error {
				calls.Add(1)
				return rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "UpdateEnvironmentSettings calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(got.Status.Message).To(Equal(loopKeptMessage))
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally("~", started, 2*time.Second))
	})

	It("does not hot-loop a schedule-update 429 on a serving model", func() {
		ns := prepareNS("rl-loop-schedule")
		model := newModel(ns)
		model.Spec.Environment.AutoscalingSchedule = &modelsv1alpha1.AutoscalingScheduleConfig{
			Timezone: ptr("UTC"),
			Schedules: []modelsv1alpha1.AutoscalingSchedule{{
				Name:        "overnight",
				Cadence:     "DAILY",
				Weekdays:    []modelsv1alpha1.Weekday{"MONDAY"},
				StartHour:   ptr(int32(22)),
				EndHour:     ptr(int32(7)),
				Autoscaling: modelsv1alpha1.ScheduleAutoscalingConfig{MinReplicas: 1, MaxReplicas: 10},
			}},
		}
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		model.Status.DeploymentStatus = baseten.DeploymentStatusActive
		model.Status.Message = loopKeptMessage
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				return &baseten.Environment{
					Name: loopEnv,
					CurrentDeployment: &baseten.Deployment{
						Name: loopSource + ".1", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1,
					},
					AutoscalingSettings: testAutoscalingSettings(),
				}, nil
			},
			UpdateAutoscalingSchedulesFunc: func(ctx context.Context, modelID, envName string, spec *modelsv1alpha1.AutoscalingScheduleConfig, observed *baseten.AutoscalingSchedules) error {
				calls.Add(1)
				return rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "UpdateAutoscalingSchedules calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally("~", started, 2*time.Second))
	})

	It("does not hot-loop a source-deployment lookup 429", func() {
		ns := prepareNS("rl-loop-find")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		model.Status.DeploymentStatus = baseten.DeploymentStatusActive
		model.Status.Message = loopKeptMessage
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				return &baseten.Environment{Name: loopEnv, AutoscalingSettings: testAutoscalingSettings()}, nil
			},
			FindDeploymentIDByNameFunc: func(ctx context.Context, modelID, depName string) (string, string, error) {
				calls.Add(1)
				return "", "", rateLimited()
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(cancel, ctx, ns, mc, loopWindow)

		Expect(calls.Load()).To(BeNumerically("<=", loopCallCeiling), "FindDeployment calls=%d", calls.Load())
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		Expect(got.Status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
		Expect(got.Status.FirstDeploymentFailureTime.Time).To(BeTemporally("~", started, 2*time.Second))
	})

	It("folds one rate-limit window when a second manager starts inside that window", func() {
		ns := prepareNS("rl-loop-restart")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-30 * time.Minute)
		model.Status.ModelID = loopModelID
		ft := metav1.NewTime(started)
		model.Status.FirstDeploymentFailureTime = &ft
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		promoteErr := &baseten.APIError{
			StatusCode: 429, Message: "slow down", HasRetryAfter: true, RetryAfter: 30 * time.Second,
		}
		newMock := func(calls *atomic.Int32) *baseten.MockClient {
			return &baseten.MockClient{
				GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{Name: loopEnv, AutoscalingSettings: testAutoscalingSettings()}, nil
				},
				FindDeploymentIDByNameFunc: func(ctx context.Context, modelID, depName string) (string, string, error) {
					return loopSourceID, baseten.DeploymentStatusActive, nil
				},
				PromoteFunc: func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					calls.Add(1)
					return nil, promoteErr
				},
			}
		}

		var firstCalls atomic.Int32
		ctx1, cancel1 := context.WithCancel(context.Background())
		startManager(cancel1, ctx1, ns, newMock(&firstCalls), loopWindow)
		Expect(firstCalls.Load()).To(BeNumerically(">=", 1))

		afterFirst := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, afterFirst)).To(Succeed())
		firstDrift := afterFirst.Status.FirstDeploymentFailureTime.Sub(started)

		var secondCalls atomic.Int32
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		startManager(cancel2, ctx2, ns, newMock(&secondCalls), loopWindow)
		Expect(secondCalls.Load()).To(BeNumerically(">=", 1))

		afterSecond := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, afterSecond)).To(Succeed())
		totalDrift := afterSecond.Status.FirstDeploymentFailureTime.Sub(started)
		extra := totalDrift - firstDrift
		Expect(extra).To(BeNumerically("<", 20*time.Second), "extra drift=%s total=%s first=%s", extra, totalDrift, firstDrift)
		Expect(totalDrift).To(BeNumerically("<", 55*time.Second), "total drift=%s", totalDrift)
		Expect(totalDrift).To(BeNumerically(">", 25*time.Second), "total drift=%s", totalDrift)
	})

	It("requeues after a Retry-After of zero", func() {
		ns := prepareNS("rl-loop-zero")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		model.Status.ModelID = loopModelID
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				calls.Add(1)
				return nil, &baseten.APIError{
					StatusCode: 429, Message: "slow down", HasRetryAfter: true, RetryAfter: 0,
				}
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// No status write on this housekeeping 429, so the only requeue is the timer.
		// Floor is 1s and header jitter is at most 5s; 12s must fire it at least once more.
		startManager(cancel, ctx, ns, mc, 12*time.Second)
		Expect(calls.Load()).To(BeNumerically(">=", 2), "GetEnvironment calls=%d", calls.Load())
		Expect(calls.Load()).To(BeNumerically("<=", 20), "GetEnvironment calls=%d", calls.Load())
	})
})
