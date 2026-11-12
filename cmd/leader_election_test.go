package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Needs KUBEBUILDER_ASSETS (set by make test).
func TestLeaderLeaseHeldFor90sAndReleasedOnShutdown(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()
	cs := kubernetes.NewForConfigOrDie(cfg)
	const ns, leaseID = "leader-election", "020848ba.baseten.com"
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, withLeaderElection(ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		LeaderElectionNamespace: ns,
	}, true))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case <-time.After(30 * time.Second):
		t.Fatal("manager was not elected leader within 30s")
	}

	lease, err := cs.CoordinationV1().Leases(ns).Get(context.Background(), leaseID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if d := lease.Spec.LeaseDurationSeconds; d == nil || *d != 90 {
		t.Errorf("leaseDurationSeconds while leader = %v, want 90", ptrStr(d))
	}
	if h := lease.Spec.HolderIdentity; h == nil || *h == "" {
		t.Errorf("holderIdentity while leader = %v, want this manager", ptrStr(h))
	}

	cancel()
	select {
	case err := <-done:
		t.Logf("manager stopped: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not stop within 30s")
	}
	lease, err = cs.CoordinationV1().Leases(ns).Get(context.Background(), leaseID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease after shutdown: %v", err)
	}
	if h := lease.Spec.HolderIdentity; h != nil && *h != "" {
		t.Errorf("holderIdentity after shutdown = %q, want empty (lease released)", *h)
	}
}

func ptrStr[T any](p *T) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
