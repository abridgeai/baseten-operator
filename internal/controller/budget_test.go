package controller

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	"github.com/abridgeai/baseten-operator/internal/truss"
)

// countingClient counts Baseten API calls per method, for call-budget tests.
type countingClient struct {
	baseten.ClientInterface
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingClient) add(m string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[m]++
}

func (c *countingClient) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.calls))
	for k, v := range c.calls {
		out[k] = v
	}
	return out
}

func (c *countingClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = map[string]int{}
}

func (c *countingClient) FindModelIDByName(ctx context.Context, name, team string) (string, error) {
	c.add("FindModelIDByName")
	return c.ClientInterface.FindModelIDByName(ctx, name, team)
}

func (c *countingClient) DeleteModel(ctx context.Context, modelID string) error {
	c.add("DeleteModel")
	return c.ClientInterface.DeleteModel(ctx, modelID)
}

func (c *countingClient) GetEnvironment(ctx context.Context, modelID, env string) (*baseten.Environment, error) {
	c.add("GetEnvironment")
	return c.ClientInterface.GetEnvironment(ctx, modelID, env)
}

func (c *countingClient) ListEnvironments(ctx context.Context, modelID string) ([]baseten.Environment, error) {
	c.add("ListEnvironments")
	return c.ClientInterface.ListEnvironments(ctx, modelID)
}

func (c *countingClient) CreateEnvironment(ctx context.Context, modelID string, cfg *modelsv1alpha1.EnvironmentConfig) error {
	c.add("CreateEnvironment")
	return c.ClientInterface.CreateEnvironment(ctx, modelID, cfg)
}

func (c *countingClient) UpdateEnvironmentSettings(ctx context.Context, modelID, env string, a *modelsv1alpha1.AutoscalingConfig, p *modelsv1alpha1.PromotionSettingsConfig) error {
	c.add("UpdateEnvironmentSettings")
	return c.ClientInterface.UpdateEnvironmentSettings(ctx, modelID, env, a, p)
}

func (c *countingClient) UpdateAutoscalingSchedules(ctx context.Context, modelID, env string, s *modelsv1alpha1.AutoscalingScheduleConfig, o *baseten.AutoscalingSchedules) error {
	c.add("UpdateAutoscalingSchedules")
	return c.ClientInterface.UpdateAutoscalingSchedules(ctx, modelID, env, s, o)
}

func (c *countingClient) FindDeploymentIDByName(ctx context.Context, modelID, name string) (string, string, error) {
	c.add("FindDeploymentIDByName")
	return c.ClientInterface.FindDeploymentIDByName(ctx, modelID, name)
}

func (c *countingClient) ActivateDeployment(ctx context.Context, modelID, depID string) error {
	c.add("ActivateDeployment")
	return c.ClientInterface.ActivateDeployment(ctx, modelID, depID)
}

func (c *countingClient) Promote(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
	c.add("Promote")
	return c.ClientInterface.Promote(ctx, modelID, depID, env, s)
}

func (c *countingClient) ListDeployments(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
	c.add("ListDeployments")
	return c.ClientInterface.ListDeployments(ctx, modelID)
}

func (c *countingClient) UpdateDeploymentAutoscaling(ctx context.Context, modelID, depID string, minReplica int32) error {
	c.add("UpdateDeploymentAutoscaling")
	return c.ClientInterface.UpdateDeploymentAutoscaling(ctx, modelID, depID, minReplica)
}

func (c *countingClient) DeleteDeployment(ctx context.Context, modelID, depID string) error {
	c.add("DeleteDeployment")
	return c.ClientInterface.DeleteDeployment(ctx, modelID, depID)
}

func (c *countingClient) RetryDeployment(ctx context.Context, modelID, depID string) (*baseten.RetryResponse, error) {
	c.add("RetryDeployment")
	return c.ClientInterface.RetryDeployment(ctx, modelID, depID)
}

// statusCountingClient counts status subresource writes, including no-op ones the API server drops.
type statusCountingClient struct {
	client.Client
	writes *atomic.Int32
}

func (c statusCountingClient) Status() client.SubResourceWriter {
	return statusCountingWriter{c.Client.Status(), c.writes}
}

type statusCountingWriter struct {
	client.SubResourceWriter
	writes *atomic.Int32
}

func (w statusCountingWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.writes.Add(1)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w statusCountingWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.writes.Add(1)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// discardRecorder drops events; a manager run emits more than FakeRecorder buffers.
type discardRecorder struct{}

func (discardRecorder) Event(runtime.Object, string, string, string)          {}
func (discardRecorder) Eventf(runtime.Object, string, string, string, ...any) {}
func (discardRecorder) AnnotatedEventf(runtime.Object, map[string]string, string, string, string, ...any) {
}

var budgetSeq atomic.Int32

var _ = Describe("Call budgets", Ordered, func() {
	const (
		ns     = "budget"
		srcDep = "img-1.0-wgt-1.0-p-1.2"
	)
	var (
		mock  *baseten.MockClient
		calls *countingClient
	)

	BeforeAll(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue())
	})

	BeforeEach(func() {
		mock = &baseten.MockClient{
			FindModelIDByNameFunc: func(context.Context, string, string) (string, error) { return "model-123", nil },
			GetEnvironmentFunc: func(context.Context, string, string) (*baseten.Environment, error) {
				return &baseten.Environment{Name: "dev", AutoscalingSettings: testAutoscalingSettings(),
					CurrentDeployment: &baseten.Deployment{Name: srcDep + ".1", Status: baseten.DeploymentStatusActive}}, nil
			},
			DeleteModelFunc: func(context.Context, string) error { return nil },
		}
		calls = &countingClient{ClientInterface: mock, calls: map[string]int{}}
	})

	newModel := func() *modelsv1alpha1.BasetenModel {
		return &modelsv1alpha1.BasetenModel{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("budget-%d", budgetSeq.Add(1)), Namespace: ns,
				Finalizers: []string{modelsv1alpha1.FinalizerName}},
			Spec: modelsv1alpha1.BasetenModelSpec{ModelName: "test-model", SourceDeploymentName: srcDep,
				Environment: modelsv1alpha1.EnvironmentConfig{Name: "dev", Autoscaling: &modelsv1alpha1.AutoscalingConfig{
					MinReplicas: ptr(int32(0)), MaxReplicas: ptr(int32(5)), ConcurrencyTarget: ptr(int32(10))}}},
		}
	}
	get := func(name string) *modelsv1alpha1.BasetenModel {
		m := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, m)).To(Succeed())
		return m
	}
	remove := func(name string) {
		m := get(name)
		m.Finalizers = nil
		Expect(k8sClient.Update(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
	}

	DescribeTable("per Reconcile pass, on the second pass over the same state",
		func(setup func(*baseten.MockClient), want map[string]int) {
			setup(mock)
			writes := &atomic.Int32{}
			r := &BasetenModelReconciler{Client: statusCountingClient{k8sClient, writes}, Scheme: k8sClient.Scheme(), BasetenClient: calls,
				TrussPusher: &truss.MockPusher{}, Recorder: discardRecorder{}}
			m := newModel()
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer remove(m.Name)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: m.Name, Namespace: ns}}
			_, _ = r.Reconcile(ctx, req)
			rv := get(m.Name).ResourceVersion
			Expect(writes.Load()).To(BeNumerically(">", 0), "first-pass status write calls")
			calls.reset()
			writes.Store(0)
			_, _ = r.Reconcile(ctx, req)
			Expect(calls.snapshot()).To(Equal(want), "Baseten calls")
			Expect(writes.Load()).To(BeZero(), "status write calls")
			Expect(get(m.Name).ResourceVersion).To(Equal(rv), "status writes")
		},
		Entry("steady ACTIVE object", func(*baseten.MockClient) {}, map[string]int{"GetEnvironment": 1}),
	)

	// Known gap D7: drift correction has no convergence guard yet. Pending until that fix lands.
	It("a field Baseten never echoes: at most 2 updates in 4 passes", Pending, func() {
		mock.UpdateEnvironmentSettingsFunc = func(context.Context, string, string, *modelsv1alpha1.AutoscalingConfig, *modelsv1alpha1.PromotionSettingsConfig) error {
			return nil
		}
		r := &BasetenModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), BasetenClient: calls,
			TrussPusher: &truss.MockPusher{}, Recorder: discardRecorder{}}
		m := newModel()
		m.Spec.Environment.Autoscaling.MaxScaleDownRate = ptr(int32(50))
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		defer remove(m.Name)
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: m.Name, Namespace: ns}}
		for range 4 {
			_, _ = r.Reconcile(ctx, req)
		}
		Expect(calls.snapshot()["UpdateEnvironmentSettings"]).To(BeNumerically("<=", 2), "updates in 4 passes")
	})

	It("keeps a steady object quiet in a manager and reacts to a spec change", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme,
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
			Controller: config.Controller{SkipNameValidation: ptr(true)}})
		Expect(err).NotTo(HaveOccurred())
		r := &BasetenModelReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), BasetenClient: calls,
			TrussPusher: &truss.MockPusher{}, Recorder: discardRecorder{}}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		mctx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer GinkgoRecover(); defer close(done); Expect(mgr.Start(mctx)).To(Succeed()) }()
		defer func() { stop(); <-done }()

		m := newModel()
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Eventually(func() string { return get(m.Name).Status.DeploymentStatus }, 5*time.Second).
			Should(Equal(baseten.DeploymentStatusActive))
		time.Sleep(time.Second)
		calls.reset()
		Consistently(calls.snapshot, 2*time.Second, 100*time.Millisecond).Should(BeEmpty(), "steady window")

		latest := get(m.Name)
		latest.Spec.DeletionPolicy = modelsv1alpha1.DeletionPolicyDelete
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		Eventually(func() int { return calls.snapshot()["GetEnvironment"] }, 2*time.Second).
			Should(BeNumerically(">=", 1), "spec-change control")

		Expect(k8sClient.Delete(ctx, latest)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: ns}, latest))
		}, 5*time.Second).Should(BeTrue())
	})
})
