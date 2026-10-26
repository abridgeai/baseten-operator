package controller

import (
	"context"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("Promotion recovery schema", func() {
	It("preserves pinned recovery and rejects an active writer or unidentified candidate", func() {
		ctx := context.Background()
		model := &modelsv1alpha1.BasetenModel{ObjectMeta: metav1.ObjectMeta{Name: "recovery-schema", Namespace: "default"}, Spec: modelsv1alpha1.BasetenModelSpec{
			ModelName: "model", SourceDeploymentName: "source", Mode: modelsv1alpha1.ModeReconcile,
			Environment:       modelsv1alpha1.EnvironmentConfig{Name: "dev", Autoscaling: &modelsv1alpha1.AutoscalingConfig{MinReplicas: ptr(int32(0))}},
			PromotionRecovery: &modelsv1alpha1.PromotionRecovery{ModelID: "model1", EnvironmentName: "dev", CandidateDeploymentID: "candidate"},
		}}
		Expect(k8sClient.Create(ctx, model)).NotTo(Succeed())
		model.Spec.Mode = modelsv1alpha1.ModePause
		model.Spec.PromotionRecovery.CandidateDeploymentID = ""
		Expect(k8sClient.Create(ctx, model)).NotTo(Succeed())
		model.Spec.PromotionRecovery.ObserveOnly = true
		Expect(k8sClient.Create(ctx, model)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, model)).To(Succeed()) })
		got := &modelsv1alpha1.BasetenModel{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: model.Name, Namespace: model.Namespace}, got)).To(Succeed())
		Expect(got.Spec.PromotionRecovery).To(Equal(model.Spec.PromotionRecovery))
	})
})
