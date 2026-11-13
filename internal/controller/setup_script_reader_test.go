package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
)

var _ = Describe("Setup script ConfigMap read", func() {
	It("reads a configMapRef without a ConfigMap informer", func() {
		const ns = "setup-script-reader"
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue())
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "setup", Namespace: ns},
			Data: map[string]string{"setup.sh": "echo hi"}})).To(Succeed())

		// The cache refuses to start informers it was not asked for, so a cached
		// ConfigMap Get fails instead of opening a cluster-wide ConfigMap watch.
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{ReaderFailOnMissingInformer: true}})
		Expect(err).NotTo(HaveOccurred())
		r := &BasetenModelReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}

		tc := &modelsv1alpha1.TrussConfig{SetupScript: &modelsv1alpha1.SetupScriptSource{
			ConfigMapRef: &modelsv1alpha1.ConfigMapKeyRef{Name: "setup", Key: "setup.sh"}}}
		script, err := r.readSetupScript(ctx, tc, ns)
		Expect(err).NotTo(HaveOccurred())
		Expect(script).To(Equal("echo hi"))
	})
})
