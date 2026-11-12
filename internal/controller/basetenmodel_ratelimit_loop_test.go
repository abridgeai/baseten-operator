package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// launchManager starts a real controller for ns and returns a wait that
	// blocks until ctx is canceled and the manager has stopped. onEvent may be nil.
	launchManager := func(ctx context.Context, ns string, mc *baseten.MockClient, onEvent func(string)) func() {
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
				case e := <-rec.Events:
					if onEvent != nil {
						onEvent(e)
					}
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
		return func() {
			Eventually(done).WithTimeout(10 * time.Second).Should(BeClosed())
		}
	}

	startManager := func(cancel context.CancelFunc, ctx context.Context, ns string, mc *baseten.MockClient, window time.Duration) {
		wait := launchManager(ctx, ns, mc, nil)
		time.Sleep(window)
		cancel()
		wait()
	}

	failedCandidate := func(calls *atomic.Int32) *baseten.MockClient {
		return &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				calls.Add(1)
				return &baseten.Environment{
					Name: loopEnv,
					CandidateDeployment: &baseten.Deployment{
						ID: "cand-loop", Name: loopSource + ".1", Status: baseten.DeploymentStatusBuildFailed,
					},
					AutoscalingSettings: testAutoscalingSettings(),
				}, nil
			},
			RetryDeploymentFunc: func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
				calls.Add(1)
				return &baseten.RetryResponse{Retried: true}, nil
			},
		}
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
		// The saved window is still open, so this manager waits it out instead of
		// calling Promote again. A tail fold of a couple of seconds is still one window.
		Expect(secondCalls.Load()).To(BeNumerically("<=", 1))

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

	seedExhausted := func(ns string, windowEnd time.Time) {
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		ft := metav1.NewTime(time.Now().Add(-2*time.Hour - time.Minute))
		we := metav1.NewTime(windowEnd)
		model.Status.ModelID = loopModelID
		model.Status.FirstDeploymentFailureTime = &ft
		model.Status.RateLimitWindowEnd = &we
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())
	}

	It("requeues to the window end when the 2h deadline is already exceeded", func() {
		ns := prepareNS("rl-loop-window")
		// Long enough that a 2s sample is still inside it, short enough that the
		// same manager reaches the deadline check once it has passed.
		windowEnd := time.Now().Add(6 * time.Second)
		seedExhausted(ns, windowEnd)

		var calls atomic.Int32
		var firstCall atomic.Int64
		var mu sync.Mutex
		var events []string
		mc := failedCandidate(&calls)
		mc.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
			firstCall.CompareAndSwap(0, time.Now().UnixNano())
			calls.Add(1)
			return &baseten.Environment{
				Name: loopEnv,
				CandidateDeployment: &baseten.Deployment{
					ID: "cand-loop", Name: loopSource + ".1", Status: baseten.DeploymentStatusBuildFailed,
				},
				AutoscalingSettings: testAutoscalingSettings(),
			}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, mc, func(e string) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		})

		time.Sleep(2 * time.Second)
		Expect(calls.Load()).To(Equal(int32(0)))
		mu.Lock()
		seen := append([]string(nil), events...)
		mu.Unlock()
		Expect(seen).NotTo(ContainElement(ContainSubstring("DeploymentRetryExhausted")))

		Eventually(func(g Gomega) {
			g.Expect(time.Now()).To(BeTemporally(">", windowEnd))
			g.Expect(calls.Load()).To(BeNumerically(">=", 1))
			mu.Lock()
			defer mu.Unlock()
			g.Expect(events).To(ContainElement(ContainSubstring("DeploymentRetryExhausted")))
		}).WithTimeout(12 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		// The deadline fired from the requeue at the window, not from a hot loop.
		Expect(calls.Load()).To(BeNumerically("<=", 4), "calls=%d", calls.Load())
		Expect(time.Unix(0, firstCall.Load())).To(BeTemporally(">=", windowEnd.Add(-time.Second)))

		cancel()
		wait()
	})

	It("exhausts retries when the rate-limit window has already passed", func() {
		ns := prepareNS("rl-loop-window-past")
		seedExhausted(ns, time.Now().Add(-time.Minute))

		var calls atomic.Int32
		var mu sync.Mutex
		var events []string
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, failedCandidate(&calls), func(e string) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		})
		Eventually(func(g Gomega) {
			g.Expect(calls.Load()).To(BeNumerically(">=", 1))
			mu.Lock()
			defer mu.Unlock()
			g.Expect(events).To(ContainElement(ContainSubstring("DeploymentRetryExhausted")))
		}).WithTimeout(8 * time.Second).Should(Succeed())
		cancel()
		wait()
	})

	It("does not exhaust retries when a watch fires during an open rate-limit window", func() {
		ns := prepareNS("rl-loop-window-watch")
		seedExhausted(ns, time.Now().Add(50*time.Minute))

		var calls atomic.Int32
		var mu sync.Mutex
		var events []string
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, failedCandidate(&calls), func(e string) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		})

		time.Sleep(2 * time.Second)
		Expect(calls.Load()).To(Equal(int32(0)))

		fresh := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, fresh)).To(Succeed())
		base := fresh.DeepCopy()
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations["baseten.ai/poke"] = "1"
		Expect(k8sClient.Patch(context.Background(), fresh, client.MergeFrom(base))).To(Succeed())

		time.Sleep(2 * time.Second)
		Expect(calls.Load()).To(Equal(int32(0)))
		mu.Lock()
		seen := append([]string(nil), events...)
		mu.Unlock()
		Expect(seen).NotTo(ContainElement(ContainSubstring("DeploymentRetryExhausted")))

		cancel()
		wait()
	})

	stampReady := func(model *modelsv1alpha1.BasetenModel) {
		model.Status.Conditions = []metav1.Condition{{
			Type:               conditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             statusRateLimited,
			Message:            rateLimitMessage,
			ObservedGeneration: model.Generation,
			LastTransitionTime: metav1.Now(),
		}}
	}

	serving := func(calls *atomic.Int32) *baseten.MockClient {
		return &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				calls.Add(1)
				return &baseten.Environment{
					Name: loopEnv,
					CurrentDeployment: &baseten.Deployment{
						Name: loopSource + ".1", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1,
					},
					AutoscalingSettings: testAutoscalingSettings(),
				}, nil
			},
		}
	}

	It("folds a 15m cap for a one-hour Retry-After and still retries while budget remains", func() {
		ns := prepareNS("rl-loop-fold15")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		started := time.Now().Add(-70 * time.Minute)
		ft := metav1.NewTime(started)
		model.Status.ModelID = loopModelID
		model.Status.FirstDeploymentFailureTime = &ft
		stampReady(model)
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		failedEnv := func() (*baseten.Environment, error) {
			return &baseten.Environment{
				Name: loopEnv,
				CandidateDeployment: &baseten.Deployment{
					ID: "cand-loop", Name: loopSource + ".1", Status: baseten.DeploymentStatusBuildFailed,
				},
				AutoscalingSettings: testAutoscalingSettings(),
			}, nil
		}
		var retries atomic.Int32
		mc := &baseten.MockClient{
			GetEnvironmentFunc: func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				return failedEnv()
			},
			FindDeploymentIDByNameFunc: func(ctx context.Context, modelID, depName string) (string, string, error) {
				return "cand-loop", baseten.DeploymentStatusBuildFailed, nil
			},
			RetryDeploymentFunc: func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
				retries.Add(1)
				return nil, &baseten.APIError{
					StatusCode: 429, Message: "slow down", HasRetryAfter: true, RetryAfter: time.Hour,
				}
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		wait := launchManager(ctx, ns, mc, nil)
		Eventually(func(g Gomega) {
			g.Expect(retries.Load()).To(Equal(int32(1)))
			got := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
			g.Expect(got.Status.Message).NotTo(ContainSubstring("retries exhausted"))
			g.Expect(got.Status.RateLimitWindowEnd).NotTo(BeNil())
			g.Expect(got.Status.RateLimitWindowEnd.Time).To(BeTemporally("~", time.Now().Add(baseten.MaxRetryAfter), 5*time.Second))
			g.Expect(got.Status.FirstDeploymentFailureTime.Sub(started)).To(BeNumerically("~", baseten.MaxRetryAfter, 5*time.Second))
		}).WithTimeout(8 * time.Second).Should(Succeed())
		time.Sleep(time.Second)
		Expect(retries.Load()).To(Equal(int32(1)))
		cancel()
		wait()

		// The capped wait has elapsed and about 55m of the 2h budget is left.
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
		passed := metav1.NewTime(time.Now().Add(-time.Second))
		got.Status.RateLimitWindowEnd = &passed
		Expect(k8sClient.Status().Update(context.Background(), got)).To(Succeed())

		var again atomic.Int32
		mc.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
			again.Add(1)
			return &baseten.RetryResponse{Retried: true}, nil
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		wait2 := launchManager(ctx2, ns, mc, nil)
		Eventually(func(g Gomega) {
			g.Expect(again.Load()).To(BeNumerically(">=", 1))
			fresh := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, fresh)).To(Succeed())
			g.Expect(fresh.Status.Message).NotTo(ContainSubstring("retries exhausted"))
		}).WithTimeout(8 * time.Second).Should(Succeed())
		cancel2()
		wait2()
	})

	It("clamps a pre-existing far-future rate-limit window to 15m", func() {
		ns := prepareNS("rl-loop-far")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		model.Status.ModelID = loopModelID
		far := metav1.NewTime(time.Now().Add(365 * 24 * time.Hour))
		model.Status.RateLimitWindowEnd = &far
		stampReady(model)
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, serving(&calls), nil)
		Eventually(func(g Gomega) {
			g.Expect(calls.Load()).To(Equal(int32(0)))
			got := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
			g.Expect(got.Status.RateLimitWindowEnd).NotTo(BeNil())
			g.Expect(got.Status.RateLimitWindowEnd.Time).To(BeTemporally("~", time.Now().Add(baseten.MaxRetryAfter), 5*time.Second))
		}).WithTimeout(8 * time.Second).Should(Succeed())
		cancel()
		wait()
	})

	It("clears the rate-limit window and calls when the spec generation changes", func() {
		ns := prepareNS("rl-loop-spec")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		model.Status.ModelID = loopModelID
		window := metav1.NewTime(time.Now().Add(10 * time.Minute))
		model.Status.RateLimitWindowEnd = &window
		stampReady(model)
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, serving(&calls), nil)
		time.Sleep(2 * time.Second)
		Expect(calls.Load()).To(Equal(int32(0)))

		Eventually(func(g Gomega) {
			fresh := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, fresh)).To(Succeed())
			fresh.Spec.Team = "Speech"
			g.Expect(k8sClient.Update(context.Background(), fresh)).To(Succeed())
		}).WithTimeout(5 * time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(calls.Load()).To(BeNumerically(">=", 1))
			got := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
			g.Expect(got.Status.RateLimitWindowEnd).To(BeNil())
		}).WithTimeout(8 * time.Second).Should(Succeed())
		cancel()
		wait()
	})

	It("clears the rate-limit window on pause and calls on resume", func() {
		ns := prepareNS("rl-loop-pause")
		model := newModel(ns)
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		model.Status.ModelID = loopModelID
		window := metav1.NewTime(time.Now().Add(10 * time.Minute))
		model.Status.RateLimitWindowEnd = &window
		stampReady(model)
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, serving(&calls), nil)
		time.Sleep(2 * time.Second)
		Expect(calls.Load()).To(Equal(int32(0)))

		Eventually(func(g Gomega) {
			fresh := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, fresh)).To(Succeed())
			fresh.Spec.Mode = modelsv1alpha1.ModePause
			g.Expect(k8sClient.Update(context.Background(), fresh)).To(Succeed())
		}).WithTimeout(5 * time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			got := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)).To(Succeed())
			g.Expect(got.Status.DeploymentStatus).To(Equal(statusPaused))
			g.Expect(got.Status.RateLimitWindowEnd).To(BeNil())
		}).WithTimeout(8 * time.Second).Should(Succeed())
		Expect(calls.Load()).To(Equal(int32(0)))
		cancel()
		wait()

		// Resume with a window and no failure clock must not stay PAUSED.
		resumeNS := prepareNS("rl-loop-resume")
		resume := newModel(resumeNS)
		Expect(k8sClient.Create(context.Background(), resume)).To(Succeed())
		resume.Status.ModelID = loopModelID
		resume.Status.DeploymentStatus = statusPaused
		held := metav1.NewTime(time.Now().Add(10 * time.Minute))
		resume.Status.RateLimitWindowEnd = &held
		stampReady(resume)
		Expect(k8sClient.Status().Update(context.Background(), resume)).To(Succeed())

		var resumeCalls atomic.Int32
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		wait2 := launchManager(ctx2, resumeNS, serving(&resumeCalls), nil)
		Eventually(func(g Gomega) {
			g.Expect(resumeCalls.Load()).To(BeNumerically(">=", 1))
			got := &modelsv1alpha1.BasetenModel{}
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: resumeNS}, got)).To(Succeed())
			g.Expect(got.Status.DeploymentStatus).NotTo(Equal(statusPaused))
			g.Expect(got.Status.RateLimitWindowEnd).To(BeNil())
		}).WithTimeout(8 * time.Second).Should(Succeed())
		cancel2()
		wait2()
	})

	It("does not hold delete or the finalizer for a rate-limit window", func() {
		ns := prepareNS("rl-loop-delete-window")
		model := newModel(ns)
		model.Spec.DeletionPolicy = modelsv1alpha1.DeletionPolicyDelete
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		model.Status.ModelID = loopModelID
		window := metav1.NewTime(time.Now().Add(10 * time.Minute))
		model.Status.RateLimitWindowEnd = &window
		stampReady(model)
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		var calls atomic.Int32
		mc := &baseten.MockClient{
			DeleteModelFunc: func(ctx context.Context, modelID string) error {
				calls.Add(1)
				return nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := launchManager(ctx, ns, mc, nil)
		Eventually(func(g Gomega) {
			fresh := &modelsv1alpha1.BasetenModel{}
			err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, fresh)
			if apierrors.IsNotFound(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(k8sClient.Delete(context.Background(), fresh)).To(Succeed())
		}).WithTimeout(5 * time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(calls.Load()).To(BeNumerically(">=", 1))
			got := &modelsv1alpha1.BasetenModel{}
			err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "loop", Namespace: ns}, got)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).WithTimeout(8 * time.Second).Should(Succeed())
		cancel()
		wait()
	})
})
