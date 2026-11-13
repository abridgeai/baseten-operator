package controller

import (
	"context"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	"github.com/abridgeai/baseten-operator/internal/truss"
)

var _ = Describe("Reconcile timeout", func() {
	It("gives every Baseten call a deadline", func() {
		const ns = "reconcile-timeout"
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue())
		var calls, withDeadline atomic.Int32
		var maxLeft atomic.Int64
		mock := &baseten.MockClient{
			FindModelIDByNameFunc: func(context.Context, string, string) (string, error) { return "model-123", nil },
			GetEnvironmentFunc: func(c context.Context, _, _ string) (*baseten.Environment, error) {
				calls.Add(1)
				if dl, ok := c.Deadline(); ok {
					withDeadline.Add(1)
					if left := int64(time.Until(dl)); left > maxLeft.Load() {
						maxLeft.Store(left)
					}
				}
				return &baseten.Environment{Name: "dev", AutoscalingSettings: testAutoscalingSettings(),
					CurrentDeployment: &baseten.Deployment{Name: "img-1.0-wgt-1.0-p-1.2.1", Status: baseten.DeploymentStatusActive}}, nil
			},
		}
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme,
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
			Controller: config.Controller{SkipNameValidation: ptr(true)}})
		Expect(err).NotTo(HaveOccurred())
		r := &BasetenModelReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), BasetenClient: mock,
			TrussPusher: &truss.MockPusher{}, Recorder: record.NewFakeRecorder(100)}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		mctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() { defer GinkgoRecover(); Expect(mgr.Start(mctx)).To(Succeed()) }()

		m := &modelsv1alpha1.BasetenModel{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: ns, Finalizers: []string{modelsv1alpha1.FinalizerName}},
			Spec: modelsv1alpha1.BasetenModelSpec{ModelName: "test-model", SourceDeploymentName: "img-1.0-wgt-1.0-p-1.2",
				Environment: modelsv1alpha1.EnvironmentConfig{Name: "dev"}},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		// Other specs share modelName test-model; a live leftover would block their DeleteWithGuardrails.
		defer func() {
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
			Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), m)) }, 5*time.Second).Should(BeTrue())
		}()
		Eventually(calls.Load, 5*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", 1))
		Expect(withDeadline.Load()).To(Equal(calls.Load()))
		left := time.Duration(maxLeft.Load())
		Expect(left).To(BeNumerically("<=", 4*time.Minute), "deadline within one pass budget")
		Expect(left).To(BeNumerically(">", 3*time.Minute), "deadline covers about 6 calls at the 30 s client timeout")
	})
})
