/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1 "k8s.io/api/core/v1"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
	"github.com/abridgeai/baseten-operator/internal/baseten"
	"github.com/abridgeai/baseten-operator/internal/truss"
)

func ptr[T any](v T) *T {
	return &v
}

func getCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

func testAutoscalingSettings() *baseten.AutoscalingSettings {
	return &baseten.AutoscalingSettings{
		MinReplica:        0,
		MaxReplica:        5,
		ConcurrencyTarget: 10,
	}
}

var _ = Describe("BasetenModel Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		basetenmodel := &modelsv1alpha1.BasetenModel{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind BasetenModel")
			err := k8sClient.Get(ctx, typeNamespacedName, basetenmodel)
			if err != nil && errors.IsNotFound(err) {
				resource := &modelsv1alpha1.BasetenModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: modelsv1alpha1.BasetenModelSpec{
						ModelName:            "test-model",
						SourceDeploymentName: "test-deployment-v1",
						Environment: modelsv1alpha1.EnvironmentConfig{
							Name: "dev",
							Autoscaling: &modelsv1alpha1.AutoscalingConfig{
								MinReplicas:       ptr(int32(0)),
								MaxReplicas:       ptr(int32(5)),
								ConcurrencyTarget: ptr(int32(10)),
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &modelsv1alpha1.BasetenModel{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance BasetenModel")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully create the resource", func() {
			By("Verifying the created resource exists")
			found := &modelsv1alpha1.BasetenModel{}
			err := k8sClient.Get(ctx, typeNamespacedName, found)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the spec fields")
			Expect(found.Spec.ModelName).To(Equal("test-model"))
			Expect(found.Spec.SourceDeploymentName).To(Equal("test-deployment-v1"))
			Expect(found.Spec.Environment.Name).To(Equal("dev"))
			Expect(found.Spec.Environment.Autoscaling).NotTo(BeNil())
			Expect(*found.Spec.Environment.Autoscaling.MinReplicas).To(Equal(int32(0)))
			Expect(*found.Spec.Environment.Autoscaling.MaxReplicas).To(Equal(int32(5)))
			Expect(*found.Spec.Environment.Autoscaling.ConcurrencyTarget).To(Equal(int32(10)))
		})
	})

	// Reconciliation tests using mock client
	Context("Reconciliation", func() {
		var (
			reconciler *BasetenModelReconciler
			mockClient *baseten.MockClient
			mockPusher *truss.MockPusher
			recorder   *record.FakeRecorder
			ctx        context.Context
		)

		const (
			testModelID      = "model-123"
			testDeploymentID = "deploy-456"
			testSourceDepID  = "src-456"
			testEnvName      = "dev"
			testModelName    = "test-model"
			testSourceDep    = "img-1.0-wgt-1.0-p-1.2"
		)

		newTestModel := func(name string) *modelsv1alpha1.BasetenModel {
			return &modelsv1alpha1.BasetenModel{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
				},
				Spec: modelsv1alpha1.BasetenModelSpec{
					ModelName:            testModelName,
					SourceDeploymentName: testSourceDep,
					Environment: modelsv1alpha1.EnvironmentConfig{
						Name: testEnvName,
						Autoscaling: &modelsv1alpha1.AutoscalingConfig{
							MinReplicas:       ptr(int32(0)),
							MaxReplicas:       ptr(int32(5)),
							ConcurrencyTarget: ptr(int32(10)),
						},
					},
				},
			}
		}

		BeforeEach(func() {
			ctx = context.Background()
			mockClient = &baseten.MockClient{}
			mockPusher = &truss.MockPusher{}
			recorder = record.NewFakeRecorder(20)
			reconciler = &BasetenModelReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				BasetenClient: mockClient,
				TrussPusher:   mockPusher,
				Recorder:      recorder,
			}
		})

		reconcileModel := func(model *modelsv1alpha1.BasetenModel) (ctrl.Result, error) {
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			return reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace},
			})
		}

		reconcileByName := func(name string) (ctrl.Result, error) {
			return reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			})
		}

		cleanupModel := func(name string) {
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

		getModelStatus := func(name string) modelsv1alpha1.BasetenModelStatus {
			model := &modelsv1alpha1.BasetenModel{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
			return model.Status
		}

		mockModelFound := func() {
			mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
				return testModelID, nil
			}
		}

		mockEnvWithSettings := func(current, candidate *baseten.Deployment) {
			mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
				return &baseten.Environment{
					Name:                testEnvName,
					CurrentDeployment:   current,
					CandidateDeployment: candidate,
					AutoscalingSettings: testAutoscalingSettings(),
				}, nil
			}
		}

		setupThroughStep2 := func() {
			mockModelFound()
			mockEnvWithSettings(nil, nil)
		}

		Describe("Paused Reconciliation", func() {
			It("should skip all API calls and return empty result when paused", func() {
				name := "paused-no-api"
				model := newTestModel(name)
				model.Spec.Paused = true
				defer cleanupModel(name)

				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					Fail("should not call FindModelIDByName when paused")
					return "", nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}), "should not requeue when paused")

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal("PAUSED"))
				Expect(status.Message).To(Equal("reconciliation paused"))
			})

			It("should set Ready=False and Progressing=False when paused", func() {
				name := "paused-conditions"
				model := newTestModel(name)
				model.Spec.Paused = true
				defer cleanupModel(name)

				_, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())

				status := getModelStatus(name)
				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				Expect(ready.Reason).To(Equal("PAUSED"))

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
				Expect(progressing.Reason).To(Equal("PAUSED"))
			})

			It("should preserve last status in message when paused", func() {
				name := "paused-preserve-msg"
				model := newTestModel(name)
				defer cleanupModel(name)

				// First reconcile with active deployment to set a status message
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				model.Status.Message = "active: depl-test (3 replicas, min:1 max:5) in dev environment"
				model.Status.DeploymentStatus = baseten.DeploymentStatusActive
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				// Now pause
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
				model.Spec.Paused = true
				Expect(k8sClient.Update(ctx, model)).To(Succeed())

				_, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal("PAUSED"))
				Expect(status.Message).To(Equal("reconciliation paused | last status: active: depl-test (3 replicas, min:1 max:5) in dev environment"))
			})

			It("should emit ReconciliationPaused event only on first pause", func() {
				name := "paused-event-once"
				model := newTestModel(name)
				model.Spec.Paused = true
				defer cleanupModel(name)

				// First reconcile — should emit event
				_, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("ReconciliationPaused")))

				// Second reconcile — already PAUSED, should not emit again
				_, err = reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				events = drainEvents()
				Expect(events).NotTo(ContainElement(ContainSubstring("ReconciliationPaused")))
			})

			It("should resume normal reconciliation when unpaused", func() {
				name := "paused-then-resume"
				model := newTestModel(name)
				model.Spec.Paused = true
				defer cleanupModel(name)

				// Reconcile while paused
				_, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal("PAUSED"))

				// Unpause
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, model)).To(Succeed())
				model.Spec.Paused = false
				Expect(k8sClient.Update(ctx, model)).To(Succeed())

				// Set up mocks for normal reconciliation
				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1},
					nil,
				)

				// Reconcile again — should resume normally
				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5*time.Minute), "should requeue for steady state polling")

				status = getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
			})
		})

		Describe("Step 1: Model Lookup", func() {
			It("should fail when FindModelIDByName returns error", func() {
				name := "step1-model-err"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					return "", fmt.Errorf("API error")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("API error"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("ModelNotFound")))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
				Expect(status.Message).To(ContainSubstring("Failed to lookup model"))

				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse))

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionFalse), "FAILED should not be progressing")
			})

			It("should fail when model is not found (empty ID)", func() {
				name := "step1-model-not-found"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					return "", nil
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not found"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("ModelNotFound")))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
			})

			It("should use cached modelID from status and skip API call", func() {
				name := "step1-cached-model-id"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Pre-populate status.ModelID to simulate a previous reconciliation
				model.Status.ModelID = testModelID
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				getModelIDCalled := false
				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					getModelIDCalled = true
					return testModelID, nil
				}
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1},
					nil,
				)

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(getModelIDCalled).To(BeFalse(), "FindModelIDByName should not be called when status.ModelID is cached")
			})

			It("should invalidate cached modelID on error and requeue", func() {
				name := "step1-invalidate-model-id"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Pre-populate status.ModelID to simulate a previous reconciliation
				model.Status.ModelID = testModelID
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				// Environment succeeds but FindDeploymentIDByName fails (e.g., stale model ID)
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, deploymentName string) (string, string, error) {
					return "", "", &baseten.APIError{StatusCode: 404, Message: "not found"}
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(time.Second), "should requeue quickly to re-resolve model ID")

				status := getModelStatus(name)
				Expect(status.ModelID).To(BeEmpty(), "cached model ID should be cleared")
				Expect(drainEvents()).To(ContainElement(ContainSubstring("ModelNotFound")))
			})

			It("should resolve modelID from API and persist it on first reconcile", func() {
				name := "step1-resolve-model-id"
				model := newTestModel(name)
				defer cleanupModel(name)

				getModelIDCalled := false
				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					getModelIDCalled = true
					return testModelID, nil
				}
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1},
					nil,
				)

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(getModelIDCalled).To(BeTrue(), "FindModelIDByName should be called on first reconcile")

				status := getModelStatus(name)
				Expect(status.ModelID).To(Equal(testModelID), "ModelID should be persisted in status")
			})
		})

		Describe("Step 2: Environment Reconciliation", func() {
			It("should create environment when not found (404)", func() {
				name := "step2-env-create"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return nil, &baseten.APIError{StatusCode: 404, Message: "not found"}
				}
				mockClient.CreateEnvironmentFunc = func(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error {
					return nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("EnvironmentCreated")))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(statusPending))
				Expect(status.ModelID).To(Equal(testModelID))

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionTrue), "Pending should be progressing")
			})

			It("should emit warning when environment creation fails", func() {
				name := "step2-env-create-fail"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return nil, &baseten.APIError{StatusCode: 404, Message: "not found"}
				}
				mockClient.CreateEnvironmentFunc = func(ctx context.Context, modelID string, envConfig *modelsv1alpha1.EnvironmentConfig) error {
					return fmt.Errorf("creation failed")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("creation failed"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("EnvironmentCreateFailed")))
			})

			It("should fail when GetEnvironment returns non-404 error", func() {
				name := "step2-env-500"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return nil, &baseten.APIError{StatusCode: 500, Message: "internal error"}
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
				Expect(status.Message).To(ContainSubstring("Failed to get environment"))
			})

			It("should detect and reconcile autoscaling drift", func() {
				name := "step2-drift"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: testEnvName,
						AutoscalingSettings: &baseten.AutoscalingSettings{
							MinReplica:        1, // drift: spec says 0
							MaxReplica:        5,
							ConcurrencyTarget: 10,
						},
					}, nil
				}
				mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
					return nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("AutoscalingUpdated")))
			})

			It("should emit warning when autoscaling update fails", func() {
				name := "step2-drift-fail"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name:                testEnvName,
						AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1},
					}, nil
				}
				mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
					return fmt.Errorf("update failed")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(drainEvents()).To(ContainElement(ContainSubstring("AutoscalingUpdateFailed")))
			})

			It("should emit warning when promotion settings update fails", func() {
				name := "step2-promo-drift-fail"
				model := newTestModel(name)
				model.Spec.Environment.PromotionSettings = &modelsv1alpha1.PromotionSettingsConfig{
					RollingDeploy: ptr(true),
				}
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name:                testEnvName,
						AutoscalingSettings: testAutoscalingSettings(),
						PromotionSettings: &baseten.PromotionSettings{
							RollingDeploy: ptr(false),
						},
					}, nil
				}
				mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
					return fmt.Errorf("update failed")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("PromotionSettingsUpdateFailed")))
				Expect(events).NotTo(ContainElement(ContainSubstring("AutoscalingUpdateFailed")))
			})

			It("should emit both failure events when combined drift update fails", func() {
				name := "step2-combined-drift-fail"
				model := newTestModel(name)
				model.Spec.Environment.PromotionSettings = &modelsv1alpha1.PromotionSettingsConfig{
					RollingDeploy: ptr(true),
				}
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: testEnvName,
						AutoscalingSettings: &baseten.AutoscalingSettings{
							MinReplica:        1, // drift: spec says 0
							MaxReplica:        5,
							ConcurrencyTarget: 10,
						},
						PromotionSettings: &baseten.PromotionSettings{
							RollingDeploy: ptr(false), // drift: spec says true
						},
					}, nil
				}
				mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
					return fmt.Errorf("update failed")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("AutoscalingUpdateFailed")))
				Expect(events).To(ContainElement(ContainSubstring("PromotionSettingsUpdateFailed")))
			})

			It("should detect and reconcile promotion settings drift", func() {
				name := "step2-promo-drift"
				model := newTestModel(name)
				model.Spec.Environment.PromotionSettings = &modelsv1alpha1.PromotionSettingsConfig{
					RollingDeploy:         ptr(true),
					RampUpDurationSeconds: ptr(int32(600)),
				}
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name:                testEnvName,
						AutoscalingSettings: testAutoscalingSettings(),
						PromotionSettings: &baseten.PromotionSettings{
							RollingDeploy:         ptr(false),      // drift: spec says true
							RampUpDurationSeconds: ptr(int32(300)), // drift: spec says 600
						},
					}, nil
				}
				mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
					Expect(autoscalingConfig).To(BeNil(), "no autoscaling drift, should be nil")
					Expect(promotionConfig).NotTo(BeNil())
					return nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))
				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("PromotionSettingsUpdated")))
			})

			It("should detect combined autoscaling and promotion settings drift", func() {
				name := "step2-combined-drift"
				model := newTestModel(name)
				model.Spec.Environment.PromotionSettings = &modelsv1alpha1.PromotionSettingsConfig{
					RollingDeploy: ptr(true),
				}
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: testEnvName,
						AutoscalingSettings: &baseten.AutoscalingSettings{
							MinReplica:        1, // drift: spec says 0
							MaxReplica:        5,
							ConcurrencyTarget: 10,
						},
						PromotionSettings: &baseten.PromotionSettings{
							RollingDeploy: ptr(false), // drift: spec says true
						},
					}, nil
				}
				updateCalled := false
				mockClient.UpdateEnvironmentSettingsFunc = func(ctx context.Context, modelID, envName string, autoscalingConfig *modelsv1alpha1.AutoscalingConfig, promotionConfig *modelsv1alpha1.PromotionSettingsConfig) error {
					updateCalled = true
					Expect(autoscalingConfig).NotTo(BeNil(), "autoscaling has drift")
					Expect(promotionConfig).NotTo(BeNil(), "promotion has drift")
					return nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))
				Expect(updateCalled).To(BeTrue())
				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("AutoscalingUpdated")))
				Expect(events).To(ContainElement(ContainSubstring("PromotionSettingsUpdated")))
			})

			It("should proceed when environment exists and no drift", func() {
				name := "step2-no-drift"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return &baseten.Deployment{ID: "promoted-123", Name: testSourceDep + ".123", Status: "DEPLOYING"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("DeploymentPromoted")))
			})
		})

		Describe("Step 3: Current Deployment Matches", func() {
			It("should be Ready when current deployment matches prefix (ACTIVE)", func() {
				name := "step3-active"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".1768269232", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 3},
					nil,
				)

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusActive))
				Expect(status.ActiveDeploymentName).To(Equal(testSourceDep + ".1768269232"))
				Expect(status.ActiveReplicaCount).To(Equal(int32(3)))

				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionTrue))

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionFalse), "ACTIVE should not be progressing")
			})

			It("should be Ready when current deployment matches (SCALED_TO_ZERO)", func() {
				name := "step3-scaled-to-zero"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep, Status: baseten.DeploymentStatusScaledToZero},
					nil,
				)

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusScaledToZero))
			})

			It("should clear candidate and emit DeploymentActive when promotion completes", func() {
				name := "step3-clear-candidate"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				model.Status.CandidateDeploymentID = "old-candidate-id"
				model.Status.CandidateDeploymentName = "old-candidate"
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".1768269232", Status: baseten.DeploymentStatusActive},
					nil,
				)

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("DeploymentActive")))

				status := getModelStatus(name)
				Expect(status.CandidateDeploymentID).To(BeEmpty())
				Expect(status.CandidateDeploymentName).To(BeEmpty())
			})
		})

		Describe("Step 4: Candidate Deployment In Progress", func() {
			It("should requeue when candidate is deploying", func() {
				name := "step4-deploying"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(
					nil,
					&baseten.Deployment{Name: testSourceDep + ".1768269232", Status: baseten.DeploymentStatusDeploying},
				)

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(statusPromoting))
				Expect(status.CandidateDeploymentName).To(Equal(testSourceDep + ".1768269232"))

				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse))

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionTrue), "PROMOTING should be progressing")
			})

			It("should fail with warning event when candidate has non-retryable terminal failure", func() {
				name := "step4-terminal"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(
					nil,
					&baseten.Deployment{Name: testSourceDep + ".1768269232", Status: baseten.DeploymentStatusBuildStopped},
				)

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed with status BUILD_STOPPED"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("PromotionFailed")))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusBuildStopped))

				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse))

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionFalse), "BUILD_STOPPED should not be progressing")
			})
		})

		Describe("Step 5: Promote", func() {
			It("should promote when source is ACTIVE", func() {
				name := "step5-promote-active"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil
				}
				promoteCalled := false
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					promoteCalled = true
					Expect(modelID).To(Equal(testModelID))
					Expect(depID).To(Equal(testDeploymentID))
					Expect(env).To(Equal(testEnvName))
					return &baseten.Deployment{ID: "promoted-dep-id", Name: testSourceDep + ".1768269232", Status: "DEPLOYING"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(promoteCalled).To(BeTrue())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("DeploymentPromoted")))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(statusPromoting))
				Expect(status.CandidateDeploymentID).To(Equal("promoted-dep-id"))
				Expect(status.CandidateDeploymentName).To(Equal(testSourceDep + ".1768269232"))
				Expect(status.SourceDeploymentID).To(Equal(testDeploymentID))
				Expect(status.ModelID).To(Equal(testModelID))

				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse), "PROMOTING should not be Ready")

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
			})

			It("should promote when source is SCALED_TO_ZERO", func() {
				name := "step5-promote-scaled"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusScaledToZero, nil
				}
				promoteCalled := false
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					promoteCalled = true
					return &baseten.Deployment{ID: "dep-id", Name: testSourceDep + ".123", Status: "DEPLOYING"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(promoteCalled).To(BeTrue())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			})

			It("should requeue when source is BUILDING", func() {
				name := "step5-building"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusBuilding, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusBuilding))
			})

			It("should requeue when source is DEPLOYING", func() {
				name := "step5-deploying"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusDeploying, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusDeploying))
			})

			It("should requeue when source has unknown status", func() {
				name := "step5-unknown-status"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, "SOME_NEW_STATUS", nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal("SOME_NEW_STATUS"))
			})

			It("should activate and requeue when source is INACTIVE", func() {
				name := "step5-inactive"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusInactive, nil
				}
				activateCalled := false
				mockClient.ActivateDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					activateCalled = true
					return nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(activateCalled).To(BeTrue())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusActivating))
			})

			It("should requeue when activate fails", func() {
				name := "step5-activate-fail"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusInactive, nil
				}
				mockClient.ActivateDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					return fmt.Errorf("activation failed")
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
				Expect(status.Message).To(ContainSubstring("failed to activate"))
			})

			It("should retry when source is FAILED", func() {
				name := "step5-failed"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusFailed, nil
				}
				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred(), "retry should requeue, not error")
				Expect(retryCalled).To(BeTrue())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("DeploymentRetried")))
			})

			It("should emit warning when source deployment not found (empty)", func() {
				name := "step5-src-not-found"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return "", "", nil
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(drainEvents()).To(ContainElement(ContainSubstring("SourceDeploymentNotFound")))
			})

			It("should emit warning when FindDeploymentIDByName returns API error", func() {
				name := "step5-depid-apierr"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return "", "", fmt.Errorf("API request failed")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(drainEvents()).To(ContainElement(ContainSubstring("SourceDeploymentNotFound")))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
			})

			It("should emit warning when promotion fails", func() {
				name := "step5-promote-fail"
				model := newTestModel(name)
				defer cleanupModel(name)

				setupThroughStep2()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return nil, fmt.Errorf("promotion API error")
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("promotion API error"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("PromotionFailed")))
				Expect(getModelStatus(name).DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
			})

			It("should block promotion when another promotion is in progress (double-promote guard)", func() {
				name := "step5-double-promote"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				model.Status.CandidateDeploymentID = "other-candidate-id"
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					nil,
					&baseten.Deployment{Name: "other-deployment.123", Status: baseten.DeploymentStatusDeploying},
				)

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("PromotionBlocked")))

				status := getModelStatus(name)
				Expect(status.Message).To(ContainSubstring("waiting for completion before promoting"))
				Expect(status.DeploymentStatus).To(Equal(statusPromoting))

				ready := getCondition(status.Conditions, "Ready")
				Expect(ready).NotTo(BeNil())
				Expect(ready.Status).To(Equal(metav1.ConditionFalse), "double-promote guard must NOT set Ready=True")

				progressing := getCondition(status.Conditions, "Progressing")
				Expect(progressing).NotTo(BeNil())
				Expect(progressing.Status).To(Equal(metav1.ConditionTrue), "blocked promotion is still progressing")
			})

			It("should clear stale candidateDeploymentID when source changes", func() {
				name := "step5-stale-clear"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				model.Status.CandidateDeploymentID = "old-stale-candidate"
				model.Status.CandidateDeploymentName = "old-deployment.123"
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return &baseten.Deployment{ID: "new-id", Name: testSourceDep + ".999", Status: "DEPLOYING"}, nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("DeploymentPromoted")))
				Expect(getModelStatus(name).CandidateDeploymentID).To(Equal("new-id"))
			})
		})

		Describe("Deployment Retry", func() {
			It("should compute exponential backoff with jitter", func() {
				// retryCount=1: base=2m, max jitter=1m → [2m, 3m]
				b1 := retryBackoff(1)
				Expect(b1).To(BeNumerically(">=", 2*time.Minute))
				Expect(b1).To(BeNumerically("<=", 3*time.Minute))

				// retryCount=2: base=4m, max jitter=2m → [4m, 6m]
				b2 := retryBackoff(2)
				Expect(b2).To(BeNumerically(">=", 4*time.Minute))
				Expect(b2).To(BeNumerically("<=", 6*time.Minute))

				// retryCount=3: base=8m → [8m, 12m]
				b3 := retryBackoff(3)
				Expect(b3).To(BeNumerically(">=", 8*time.Minute))
				Expect(b3).To(BeNumerically("<=", 12*time.Minute))

				// retryCount=4: base=16m → [16m, 24m]
				b4 := retryBackoff(4)
				Expect(b4).To(BeNumerically(">=", 16*time.Minute))
				Expect(b4).To(BeNumerically("<=", 24*time.Minute))

				// retryCount=5: base=30m (capped) → [30m, 45m]
				b5 := retryBackoff(5)
				Expect(b5).To(BeNumerically(">=", 30*time.Minute))
				Expect(b5).To(BeNumerically("<=", 45*time.Minute))

				// retryCount=10: still capped at 30m
				b10 := retryBackoff(10)
				Expect(b10).To(BeNumerically(">=", 30*time.Minute))
				Expect(b10).To(BeNumerically("<=", 45*time.Minute))
			})

			It("should retry candidate deployment on BUILD_FAILED", func() {
				name := "retry-candidate-build-failed"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: testEnvName,
						CandidateDeployment: &baseten.Deployment{
							ID: "cand-123", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusBuildFailed,
						},
						AutoscalingSettings: testAutoscalingSettings(),
					}, nil
				}
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return "cand-123", baseten.DeploymentStatusBuildFailed, nil
				}

				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					Expect(deploymentID).To(Equal("cand-123"))
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(retryCalled).To(BeTrue())

				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("DeploymentRetried")))

				status := getModelStatus(name)
				Expect(status.FirstDeploymentFailureTime).NotTo(BeNil())
				Expect(status.DeploymentRetryCount).To(Equal(int32(1)))
				Expect(status.NextRetryTime).NotTo(BeNil())
			})

			It("should return TerminalError when retry deadline exceeded for candidate", func() {
				name := "retry-candidate-exhausted"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Set firstDeploymentFailureTime to 3 hours ago (past 2h deadline)
				past := metav1.NewTime(time.Now().Add(-3 * time.Hour))
				model.Status.FirstDeploymentFailureTime = &past
				model.Status.CandidateDeploymentID = "cand-123"
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: testEnvName,
						CandidateDeployment: &baseten.Deployment{
							ID: "cand-123", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusBuildFailed,
						},
						AutoscalingSettings: testAutoscalingSettings(),
					}, nil
				}

				_, err := reconcileByName(name)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("retries exhausted"))

				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("DeploymentRetryExhausted")))
			})

			It("should not retry BUILD_STOPPED", func() {
				name := "retry-build-stopped-skip"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: testEnvName,
						CandidateDeployment: &baseten.Deployment{
							ID: "cand-123", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusBuildStopped,
						},
						AutoscalingSettings: testAutoscalingSettings(),
					}, nil
				}

				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					Fail("should not call RetryDeployment for BUILD_STOPPED")
					return nil, nil
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("BUILD_STOPPED"))
			})

			It("should retry source deployment on FAILED", func() {
				name := "retry-source-failed"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusFailed, nil
				}

				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					Expect(deploymentID).To(Equal(testSourceDepID))
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(retryCalled).To(BeTrue())
			})

			It("should retry source deployment on BUILD_FAILED", func() {
				name := "retry-source-build-failed"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusBuildFailed, nil
				}

				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(retryCalled).To(BeTrue())
			})

			It("should retry source deployment on DEPLOY_FAILED", func() {
				name := "retry-source-deploy-failed"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusDeployFailed, nil
				}

				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(retryCalled).To(BeTrue())
			})

			It("should handle retry API returning retried=false", func() {
				name := "retry-declined"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusFailed, nil
				}
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					return &baseten.RetryResponse{Retried: false, Reason: "deployment not retryable"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))

				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("DeploymentRetryFailed")))
			})

			It("should handle retry API HTTP error", func() {
				name := "retry-api-error"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusFailed, nil
				}
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					return nil, fmt.Errorf("connection refused")
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))

				events := drainEvents()
				Expect(events).To(ContainElement(ContainSubstring("DeploymentRetryFailed")))
			})

			It("should skip retry when backoff has not elapsed", func() {
				name := "retry-backoff-skip"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Set nextRetryTime to 5 minutes from now
				future := metav1.NewTime(time.Now().Add(5 * time.Minute))
				past := metav1.NewTime(time.Now().Add(-30 * time.Minute))
				model.Status.NextRetryTime = &future
				model.Status.FirstDeploymentFailureTime = &past
				model.Status.DeploymentRetryCount = 2
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusFailed, nil
				}
				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(retryCalled).To(BeFalse(), "should not call retry API during backoff")
				// Should requeue for the remaining backoff time (approximately 5 minutes)
				Expect(result.RequeueAfter).To(BeNumerically(">", 4*time.Minute))
				Expect(result.RequeueAfter).To(BeNumerically("<=", 5*time.Minute))
			})

			It("should skip retry when deployment is no longer in failed state", func() {
				name := "retry-concurrency-guard"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				// FindDeploymentIDByName initially returns FAILED (from validateSourceDeployment),
				// then BUILDING on the concurrency guard check
				callCount := 0
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					callCount++
					if callCount == 1 {
						return testSourceDepID, baseten.DeploymentStatusFailed, nil
					}
					return testSourceDepID, baseten.DeploymentStatusBuilding, nil
				}
				retryCalled := false
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					retryCalled = true
					return &baseten.RetryResponse{Retried: true}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(retryCalled).To(BeFalse(), "should not retry when deployment is already BUILDING")
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			})

			It("should clear all retry state on ACTIVE status", func() {
				name := "retry-clear-all-on-active"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Set retry state
				past := metav1.NewTime(time.Now().Add(-1 * time.Hour))
				future := metav1.NewTime(time.Now().Add(5 * time.Minute))
				model.Status.FirstDeploymentFailureTime = &past
				model.Status.DeploymentRetryCount = 3
				model.Status.NextRetryTime = &future
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1},
					nil,
				)

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

				status := getModelStatus(name)
				Expect(status.FirstDeploymentFailureTime).To(BeNil(), "should be cleared on ACTIVE")
				Expect(status.DeploymentRetryCount).To(Equal(int32(0)), "should be cleared on ACTIVE")
				Expect(status.NextRetryTime).To(BeNil(), "should be cleared on ACTIVE")
			})

			It("should preserve firstDeploymentFailureTime on subsequent failures", func() {
				name := "retry-preserve-failure-time"
				model := newTestModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Set failure time to 1 hour ago
				past := metav1.NewTime(time.Now().Add(-1 * time.Hour))
				model.Status.FirstDeploymentFailureTime = &past
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testSourceDepID, baseten.DeploymentStatusFailed, nil
				}
				mockClient.RetryDeploymentFunc = func(ctx context.Context, modelID, deploymentID string) (*baseten.RetryResponse, error) {
					return &baseten.RetryResponse{Retried: true}, nil
				}

				_, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())

				status := getModelStatus(name)
				Expect(status.FirstDeploymentFailureTime).NotTo(BeNil())
				// Should be approximately 1 hour ago, not reset to now
				Expect(time.Since(status.FirstDeploymentFailureTime.Time)).To(BeNumerically(">", 50*time.Minute))
			})
		})

		Describe("Orphan Deployment Cleanup", func() {
			mockCleanupDeps := func(deployments []baseten.DeploymentDetail, environments []baseten.Environment) {
				mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
					return deployments, nil
				}
				mockClient.ListEnvironmentsFunc = func(ctx context.Context, modelID string) ([]baseten.Environment, error) {
					return environments, nil
				}
			}

			It("should skip cleanup when neither scaleIn nor deleteStale is enabled", func() {
				name := "cleanup-skip-no-ops"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					// Both scaleToZero and delete default to nil (disabled)
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)
				listCalled := false
				mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
					listCalled = true
					return nil, nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(listCalled).To(BeFalse(), "should not list deployments when no cleanup operations are enabled")
			})

			It("should skip cleanup when intervalMinutes is not set", func() {
				name := "cleanup-skip-no-interval"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero: ptr(true),
					// IntervalMinutes intentionally nil
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 2},
					nil,
				)
				listCalled := false
				mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
					listCalled = true
					return nil, nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(listCalled).To(BeFalse(), "ListDeployments should not be called when intervalMinutes is nil")
			})

			It("should skip cleanup when interval has not elapsed", func() {
				name := "cleanup-skip-interval"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero:     ptr(true),
					IntervalMinutes: ptr(int32(60)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				// Set lastCleanupTime to now so interval hasn't elapsed
				now := metav1.Now()
				model.Status.LastCleanupTime = &now
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 2},
					nil,
				)
				listCalled := false
				mockClient.ListDeploymentsFunc = func(ctx context.Context, modelID string) ([]baseten.DeploymentDetail, error) {
					listCalled = true
					return nil, nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(listCalled).To(BeFalse(), "ListDeployments should not be called when interval hasn't elapsed")
			})

			It("should scale in orphan deployments with non-zero min_replica", func() {
				name := "cleanup-scale-in"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero:     ptr(true),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 2},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2025-01-01T00:00:00Z"},
						{ID: "source-dep", Name: testSourceDep, Status: "ACTIVE", CreatedAt: "2025-01-10T00:00:00Z"},
						{ID: "orphan-with-replicas", Name: "img-old-wgt-old-p-old", Status: "ACTIVE", CreatedAt: "2025-01-05T00:00:00Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 2}},
						{ID: "orphan-already-zero", Name: "img-old2-wgt-old-p-old", Status: "SCALED_TO_ZERO", CreatedAt: "2025-01-04T00:00:00Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
					},
					[]baseten.Environment{
						{
							Name:              testEnvName,
							CurrentDeployment: &baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123"},
						},
					},
				)

				scaledIDs := []string{}
				mockClient.UpdateDeploymentAutoscalingFunc = func(ctx context.Context, modelID, depID string, minReplica int32) error {
					scaledIDs = append(scaledIDs, depID)
					Expect(minReplica).To(Equal(int32(0)))
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				// Only orphan-with-replicas should be scaled, orphan-already-zero should be skipped
				Expect(scaledIDs).To(ConsistOf("orphan-with-replicas"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("OrphanDeploymentsScaledIn")))

				status := getModelStatus(name)
				Expect(status.LastCleanupTime).NotTo(BeNil())
			})

			It("should delete INACTIVE and terminal failure stale orphans respecting minToKeep", func() {
				name := "cleanup-delete-stale"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					Delete:          ptr(true),
					DeleteAfterDays: ptr(int32(7)),
					MinToKeep:       ptr(int32(1)),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 2},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2026-02-16T00:00:00Z"},
						{ID: "orphan-new", Name: "img-new-wgt-new-p-new", Status: "INACTIVE", CreatedAt: "2026-02-15T00:00:00Z"},
						{ID: "orphan-old-inactive", Name: "img-old1-wgt-old-p-old", Status: "INACTIVE", CreatedAt: "2025-12-01T00:00:00Z"},
						{ID: "orphan-old-scaled", Name: "img-old2-wgt-old-p-old", Status: "SCALED_TO_ZERO", CreatedAt: "2025-11-01T00:00:00Z"},
						{ID: "orphan-old-failed", Name: "img-old4-wgt-old-p-old", Status: "FAILED", CreatedAt: "2025-11-15T00:00:00Z"},
						{ID: "orphan-oldest", Name: "img-old3-wgt-old-p-old", Status: "INACTIVE", CreatedAt: "2025-10-01T00:00:00Z"},
					},
					[]baseten.Environment{
						{
							Name:              testEnvName,
							CurrentDeployment: &baseten.Deployment{ID: "current-dep"},
						},
					},
				)

				deletedIDs := []string{}
				mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					deletedIDs = append(deletedIDs, depID)
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				// Sorted by created_at desc: orphan-new, orphan-old-inactive, orphan-old-failed, orphan-old-scaled, orphan-oldest
				// orphan-new (index 0) → kept by minToKeep=1
				// orphan-old-inactive (index 1) → eligible, INACTIVE, old → deleted
				// orphan-old-failed (index 2) → eligible, FAILED (terminal), old → deleted
				// orphan-old-scaled (index 3) → eligible, but SCALED_TO_ZERO → skipped
				// orphan-oldest (index 4) → eligible, INACTIVE, old → deleted
				Expect(deletedIDs).To(ConsistOf("orphan-old-inactive", "orphan-old-failed", "orphan-oldest"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("OrphanDeploymentsDeleted")))
			})

			It("should delete all terminal failure statuses (FAILED, DEPLOY_FAILED, BUILD_FAILED, BUILD_STOPPED)", func() {
				name := "cleanup-delete-terminal"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					Delete:          ptr(true),
					DeleteAfterDays: ptr(int32(7)),
					MinToKeep:       ptr(int32(0)),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive, ActiveReplicaCount: 1},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2026-02-16T00:00:00Z"},
						{ID: "orphan-failed", Name: "img-f1-wgt-1-p-1", Status: "FAILED", CreatedAt: "2025-01-01T00:00:00Z"},
						{ID: "orphan-deploy-failed", Name: "img-f2-wgt-1-p-1", Status: "DEPLOY_FAILED", CreatedAt: "2025-01-02T00:00:00Z"},
						{ID: "orphan-build-failed", Name: "img-f3-wgt-1-p-1", Status: "BUILD_FAILED", CreatedAt: "2025-01-03T00:00:00Z"},
						{ID: "orphan-build-stopped", Name: "img-f4-wgt-1-p-1", Status: "BUILD_STOPPED", CreatedAt: "2025-01-04T00:00:00Z"},
						{ID: "orphan-deploying", Name: "img-f5-wgt-1-p-1", Status: "DEPLOYING", CreatedAt: "2025-01-05T00:00:00Z"},
						{ID: "orphan-building", Name: "img-f6-wgt-1-p-1", Status: "BUILDING", CreatedAt: "2025-01-06T00:00:00Z"},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
					},
				)

				deletedIDs := []string{}
				mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					deletedIDs = append(deletedIDs, depID)
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				// All 4 terminal failure statuses should be deleted (old enough, minToKeep=0)
				// DEPLOYING and BUILDING should NOT be deleted (not INACTIVE or terminal)
				Expect(deletedIDs).To(ConsistOf("orphan-failed", "orphan-deploy-failed", "orphan-build-failed", "orphan-build-stopped"))
			})

			It("should not delete orphans when all are within deleteAfterDays", func() {
				name := "cleanup-no-delete-young"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					Delete:          ptr(true),
					DeleteAfterDays: ptr(int32(30)),
					MinToKeep:       ptr(int32(0)),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)

				recentOrphan := time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339)
				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: recentOrphan},
						{ID: "orphan-recent", Name: "img-recent-wgt-1-p-1", Status: "INACTIVE", CreatedAt: recentOrphan},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
					},
				)

				deleteCalled := false
				mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					deleteCalled = true
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(deleteCalled).To(BeFalse())
			})

			It("should use days (not minutes/hours) for deleteAfterDays cutoff boundary", func() {
				// Validates that deleteAfterDays=30 means 30 calendar days, not 30 minutes or 30 hours.
				// Uses time.Now()-relative timestamps to make this test deterministic.
				name := "cleanup-stale-days-boundary"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					Delete:          ptr(true),
					DeleteAfterDays: ptr(int32(30)),
					MinToKeep:       ptr(int32(0)),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)

				now := time.Now()
				// 29 days ago — should NOT be deleted (within deleteAfterDays=30)
				twentyNineDaysAgo := now.AddDate(0, 0, -29).Format(time.RFC3339)
				// 31 days ago — should be deleted (exceeds deleteAfterDays=30)
				thirtyOneDaysAgo := now.AddDate(0, 0, -31).Format(time.RFC3339)
				// 2 hours ago — should NOT be deleted (very recent)
				twoHoursAgo := now.Add(-2 * time.Hour).Format(time.RFC3339)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: now.Format(time.RFC3339)},
						{ID: "test-orphan-29d", Name: "img-29d-wgt-1-p-1", Status: "INACTIVE", CreatedAt: twentyNineDaysAgo},
						{ID: "test-orphan-31d", Name: "img-31d-wgt-1-p-1", Status: "INACTIVE", CreatedAt: thirtyOneDaysAgo},
						{ID: "test-orphan-2h", Name: "img-2h-wgt-1-p-1", Status: "INACTIVE", CreatedAt: twoHoursAgo},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
					},
				)

				deletedIDs := []string{}
				mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					deletedIDs = append(deletedIDs, depID)
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				// Only the 31-day-old orphan should be deleted
				Expect(deletedIDs).To(ConsistOf("test-orphan-31d"))
			})

			It("should never treat is_production or is_development deployments as orphans", func() {
				name := "cleanup-protect-flags"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero:     ptr(true),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2026-02-16T00:00:00Z"},
						{ID: "prod-flag", Name: "img-prod-wgt-1-p-1", Status: "ACTIVE", CreatedAt: "2026-02-10T00:00:00Z",
							IsProduction: true, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 2}},
						{ID: "dev-flag", Name: "img-dev-wgt-1-p-1", Status: "ACTIVE", CreatedAt: "2026-02-09T00:00:00Z",
							IsDevelopment: true, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1}},
						{ID: "real-orphan", Name: "img-orphan-wgt-1-p-1", Status: "ACTIVE", CreatedAt: "2026-02-08T00:00:00Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1}},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
					},
				)

				scaledIDs := []string{}
				mockClient.UpdateDeploymentAutoscalingFunc = func(ctx context.Context, modelID, depID string, minReplica int32) error {
					scaledIDs = append(scaledIDs, depID)
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				// prod-flag and dev-flag should be protected despite not being in env or matching source
				Expect(scaledIDs).To(ConsistOf("real-orphan"))
			})

			It("should protect deployments in candidate slots across environments", func() {
				name := "cleanup-protect-candidate"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero:     ptr(true),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2026-02-16T00:00:00Z"},
						{ID: "staging-candidate", Name: "img-staging-wgt-1-p-1", Status: "DEPLOYING", CreatedAt: "2026-02-15T00:00:00Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1}},
						{ID: "real-orphan", Name: "img-orphan-wgt-1-p-1", Status: "ACTIVE", CreatedAt: "2026-02-10T00:00:00Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 1}},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
						{Name: "staging", CandidateDeployment: &baseten.Deployment{ID: "staging-candidate"}},
					},
				)

				scaledIDs := []string{}
				mockClient.UpdateDeploymentAutoscalingFunc = func(ctx context.Context, modelID, depID string, minReplica int32) error {
					scaledIDs = append(scaledIDs, depID)
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				// staging-candidate should be protected, only real-orphan should be scaled
				Expect(scaledIDs).To(ConsistOf("real-orphan"))
			})

			It("should skip deletion when deleteAfterDays or minToKeep is nil", func() {
				name := "cleanup-skip-nil-params"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					Delete:          ptr(true),
					DeleteAfterDays: ptr(int32(7)),
					// MinToKeep intentionally nil
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2026-02-16T00:00:00Z"},
						{ID: "orphan-old", Name: "img-old-wgt-1-p-1", Status: "INACTIVE", CreatedAt: "2025-01-01T00:00:00Z"},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
					},
				)

				deleteCalled := false
				mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					deleteCalled = true
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))
				Expect(deleteCalled).To(BeFalse(), "delete should be skipped when minToKeep is nil")
			})

			It("should correctly classify orphans with multi-env multi-version deployments", func() {
				// This test models a realistic scenario: 4 environments, 2 deployment versions (p-1.2 and p-1.3),
				// many old promoted copies. The CR manages production with source p-1.3.
				name := "cleanup-multi-env"
				model := newTestModel(name)
				model.Spec.SourceDeploymentName = "img-1.0-wgt-1.0-p-1.3"
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero:     ptr(true),
					Delete:          ptr(true),
					DeleteAfterDays: ptr(int32(30)),
					MinToKeep:       ptr(int32(2)),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockClient.GetEnvironmentFunc = func(ctx context.Context, modelID, envName string) (*baseten.Environment, error) {
					return &baseten.Environment{
						Name: "production",
						CurrentDeployment: &baseten.Deployment{
							ID: "test-dep-prod-current", Name: "img-1.0-wgt-1.0-p-1.3.1770676206",
							Status: baseten.DeploymentStatusScaledToZero,
						},
						AutoscalingSettings: testAutoscalingSettings(),
					}, nil
				}

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						// Environment-protected: current deployments across 4 envs
						{ID: "test-dep-staging-current", Name: "img-1.0-wgt-1.0-p-1.2.1771285997", Status: "SCALED_TO_ZERO", CreatedAt: "2026-02-16T23:53:17Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-dep-prod-current", Name: "img-1.0-wgt-1.0-p-1.3.1770676206", Status: "SCALED_TO_ZERO", CreatedAt: "2026-02-09T22:30:05Z",
							IsProduction: true, AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-dep-test-env-current", Name: "img-1.0-wgt-1.0-p-1.2.1770674083", Status: "SCALED_TO_ZERO", CreatedAt: "2026-02-09T21:54:44Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-dep-dev-current", Name: "img-1.0-wgt-1.0-p-1.2", Status: "SCALED_TO_ZERO", CreatedAt: "2026-01-12T23:35:08Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						// Source prefix-protected (matches img-1.0-wgt-1.0-p-1.3)
						{ID: "test-dep-source-original", Name: "img-1.0-wgt-1.0-p-1.3", Status: "SCALED_TO_ZERO", CreatedAt: "2026-01-12T23:40:29Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-dep-source-promoted-old", Name: "img-1.0-wgt-1.0-p-1.3.1768582860", Status: "INACTIVE", CreatedAt: "2026-01-16T17:01:00Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						// Orphans: old p-1.2 promoted copies not in any env and not matching p-1.3 prefix
						{ID: "test-orphan-p12-recent", Name: "img-1.0-wgt-1.0-p-1.2.1769590891", Status: "INACTIVE", CreatedAt: "2026-01-28T09:01:31Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-orphan-p12-mid", Name: "img-1.0-wgt-1.0-p-1.2.1769178296", Status: "INACTIVE", CreatedAt: "2026-01-23T14:24:56Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-orphan-p12-old", Name: "img-1.0-wgt-1.0-p-1.2.1768354305", Status: "INACTIVE", CreatedAt: "2026-01-14T01:31:44Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
						{ID: "test-orphan-p10-ancient", Name: "img-1.0-wgt-1.0-p-1.0", Status: "INACTIVE", CreatedAt: "2026-01-12T23:15:04Z",
							AutoscalingSettings: &baseten.AutoscalingSettings{MinReplica: 0}},
					},
					[]baseten.Environment{
						{Name: "production", CurrentDeployment: &baseten.Deployment{ID: "test-dep-prod-current"}},
						{Name: "dev", CurrentDeployment: &baseten.Deployment{ID: "test-dep-dev-current"}},
						{Name: "staging", CurrentDeployment: &baseten.Deployment{ID: "test-dep-staging-current"}},
						{Name: "testenv", CurrentDeployment: &baseten.Deployment{ID: "test-dep-test-env-current"}},
					},
				)

				deletedIDs := []string{}
				mockClient.DeleteDeploymentFunc = func(ctx context.Context, modelID, depID string) error {
					deletedIDs = append(deletedIDs, depID)
					return nil
				}
				mockClient.UpdateDeploymentAutoscalingFunc = func(ctx context.Context, modelID, depID string, minReplica int32) error {
					return nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

				// Orphans sorted by created_at desc:
				// test-orphan-p12-recent (2026-01-28) — newest INACTIVE orphan → kept by minToKeep=2
				// test-orphan-p12-mid (2026-01-23) — 2nd newest → kept by minToKeep=2
				// test-orphan-p12-old (2026-01-14) — INACTIVE, >30 days old → DELETED
				// test-orphan-p10-ancient (2026-01-12) — INACTIVE, >30 days old → DELETED
				Expect(deletedIDs).To(ConsistOf("test-orphan-p12-old", "test-orphan-p10-ancient"))
			})

			It("should handle no orphans gracefully", func() {
				name := "cleanup-no-orphans"
				model := newTestModel(name)
				model.Spec.OrphanDeploymentCleanup = &modelsv1alpha1.OrphanDeploymentCleanupConfig{
					ScaleToZero:     ptr(true),
					IntervalMinutes: ptr(int32(10)),
				}
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				mockModelFound()
				mockEnvWithSettings(
					&baseten.Deployment{ID: "current-dep", Name: testSourceDep + ".123", Status: baseten.DeploymentStatusActive},
					nil,
				)

				mockCleanupDeps(
					[]baseten.DeploymentDetail{
						{ID: "current-dep", Name: testSourceDep + ".123", Status: "ACTIVE", CreatedAt: "2026-02-16T00:00:00Z"},
					},
					[]baseten.Environment{
						{Name: testEnvName, CurrentDeployment: &baseten.Deployment{ID: "current-dep"}},
					},
				)

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

				status := getModelStatus(name)
				Expect(status.LastCleanupTime).NotTo(BeNil())
			})
		})

		Describe("TrussConfig: Deployment via truss push", func() {
			newTrussConfigModel := func(name string) *modelsv1alpha1.BasetenModel {
				return &modelsv1alpha1.BasetenModel{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: modelsv1alpha1.BasetenModelSpec{
						ModelName: testModelName,
						TrussConfig: &modelsv1alpha1.TrussConfig{
							PythonVersion: "py312",
							Resources:     modelsv1alpha1.TrussResources{Accelerator: "H100:1"},
							BaseImage:     modelsv1alpha1.TrussBaseImage{Image: "test:latest"},
						},
						Environment: modelsv1alpha1.EnvironmentConfig{
							Name: testEnvName,
							Autoscaling: &modelsv1alpha1.AutoscalingConfig{
								MinReplicas:       ptr(int32(0)),
								MaxReplicas:       ptr(int32(5)),
								ConcurrencyTarget: ptr(int32(10)),
							},
						},
					},
				}
			}

			It("should launch async push and requeue when trussConfig is specified", func() {
				name := "truss-push-and-promote"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return "", "", nil // Not found — triggers push
				}

				pushCalled := false
				mockPusher.PushFromConfigFunc = func(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*truss.PushResult, error) {
					pushCalled = true
					Expect(modelName).To(Equal(testModelName))
					Expect(deploymentName).To(HavePrefix("depl-"))
					return &truss.PushResult{ModelID: testModelID, DeploymentID: "new-dep-id"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10*time.Second), "should requeue quickly to check push result")

				// Check status before waiting for goroutine — reconcile sets TRUSS_PUSHING synchronously
				status := getModelStatus(name)
				Expect(status.TrussConfigHash).NotTo(BeEmpty())
				Expect(drainEvents()).To(ContainElement(ContainSubstring(EventTrussPushStarted)))

				// Async push runs in background — give it a moment
				Eventually(func() bool { return pushCalled }, 2*time.Second, 100*time.Millisecond).Should(BeTrue(), "async push should be called")
			})

			It("should emit TrussPushCompleted when deployment appears after push", func() {
				name := "truss-push-completed-event"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				// Pre-set status to simulate a previous push in flight
				mockModelFound()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil // Deployment now exists
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return &baseten.Deployment{ID: "promoted-123", Name: "depl-test.123", Status: "DEPLOYING"}, nil
				}
				mockEnvWithSettings(nil, nil)

				// Create model and manually set push status to PUSHING
				Expect(k8sClient.Create(ctx, model)).To(Succeed())
				model.Status.TrussPushStatus = statusTrussPushing
				model.Status.SourceDeploymentName = "depl-test-placeholder"
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				_, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())

				Expect(drainEvents()).To(ContainElement(ContainSubstring(EventTrussPushCompleted)))
			})

			It("should skip push when deployment already exists", func() {
				name := "truss-skip-existing"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil // Already exists
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return &baseten.Deployment{ID: "promoted-123", Name: "depl-test.123", Status: "DEPLOYING"}, nil
				}
				mockEnvWithSettings(nil, nil)

				pushCalled := false
				mockPusher.PushFromConfigFunc = func(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*truss.PushResult, error) {
					pushCalled = true
					return nil, fmt.Errorf("should not be called")
				}

				_, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(pushCalled).To(BeFalse(), "push should be skipped when deployment exists")

				status := getModelStatus(name)
				Expect(status.TrussPushStatus).To(Equal(statusTrussPushDone))
				Expect(status.TrussConfigHash).NotTo(BeEmpty())
			})

			It("should requeue and retry when async push fails", func() {
				name := "truss-push-fail"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				mockModelFound()
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return "", "", nil // Not found — push will fail, next reconcile retries
				}

				mockPusher.PushFromConfigFunc = func(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*truss.PushResult, error) {
					return nil, fmt.Errorf("upload failed: connection timeout")
				}

				// First reconcile launches async push and requeues
				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred(), "async push failure should not return error")
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))

				status := getModelStatus(name)
				Expect(status.TrussConfigHash).NotTo(BeEmpty())
				Expect(drainEvents()).To(ContainElement(ContainSubstring(EventTrussPushStarted)))
			})

			It("should read setup script from ConfigMap", func() {
				name := "truss-configmap-setup"
				model := newTrussConfigModel(name)
				model.Spec.TrussConfig.SetupScript = &modelsv1alpha1.SetupScriptSource{
					ConfigMapRef: &modelsv1alpha1.ConfigMapKeyRef{
						Name: "test-setup-script",
						Key:  "setup.sh",
					},
				}
				defer cleanupModel(name)

				// Create the ConfigMap
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "test-setup-script", Namespace: "default"},
					Data:       map[string]string{"setup.sh": "#!/bin/bash\necho hello"},
				}
				Expect(k8sClient.Create(ctx, cm)).To(Succeed())
				defer func() { _ = k8sClient.Delete(ctx, cm) }()

				mockModelFound()
				findCalls := 0
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					findCalls++
					if findCalls <= 1 {
						return "", "", nil // Not found in reconcileTrussDeployment
					}
					return testDeploymentID, baseten.DeploymentStatusActive, nil // Found in validateSourceDeployment
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return &baseten.Deployment{ID: "promoted-123", Name: "depl-test.123", Status: "DEPLOYING"}, nil
				}
				mockEnvWithSettings(nil, nil)

				var capturedSetupScript []byte
				mockPusher.PushFromConfigFunc = func(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*truss.PushResult, error) {
					capturedSetupScript = setupScript
					return &truss.PushResult{ModelID: testModelID, DeploymentID: "new-dep"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10*time.Second), "should requeue to check async push result")

				// Async push runs in background — verify setup script was passed
				Eventually(func() []byte { return capturedSetupScript }, 2*time.Second, 100*time.Millisecond).ShouldNot(BeEmpty())
				Expect(string(capturedSetupScript)).To(Equal("#!/bin/bash\necho hello"))

				Expect(drainEvents()).To(ContainElement(ContainSubstring(EventTrussPushStarted)))
			})

			It("should fail when ConfigMap not found", func() {
				name := "truss-configmap-missing"
				model := newTrussConfigModel(name)
				model.Spec.TrussConfig.SetupScript = &modelsv1alpha1.SetupScriptSource{
					ConfigMapRef: &modelsv1alpha1.ConfigMapKeyRef{
						Name: "nonexistent-cm",
						Key:  "setup.sh",
					},
				}
				defer cleanupModel(name)

				mockModelFound()

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())

				status := getModelStatus(name)
				Expect(status.Message).To(ContainSubstring("ConfigMap"))

				Expect(drainEvents()).To(ContainElement(ContainSubstring(EventSetupScriptNotFound)))
			})

			It("should produce different hash when config changes", func() {
				tc1 := &modelsv1alpha1.TrussConfig{
					Resources: modelsv1alpha1.TrussResources{Accelerator: "H100:1"},
					BaseImage: modelsv1alpha1.TrussBaseImage{Image: "test:v1"},
				}
				tc2 := &modelsv1alpha1.TrussConfig{
					Resources: modelsv1alpha1.TrussResources{Accelerator: "H100:1"},
					BaseImage: modelsv1alpha1.TrussBaseImage{Image: "test:v2"},
				}
				hash1 := truss.HashTrussConfig(tc1, "")
				hash2 := truss.HashTrussConfig(tc2, "")
				Expect(hash1).NotTo(Equal(hash2), "different image should produce different hash")
			})

			It("should auto-create model via truss push when model does not exist", func() {
				name := "truss-auto-create-model"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				// Model does not exist
				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					return "", nil
				}

				pushCalled := false
				mockPusher.PushFromConfigFunc = func(ctx context.Context, configYAML, setupScript []byte, modelName, deploymentName string) (*truss.PushResult, error) {
					pushCalled = true
					Expect(modelName).To(Equal(testModelName))
					return &truss.PushResult{ModelID: "new-model-id", DeploymentID: "new-dep-id"}, nil
				}

				result, err := reconcileModel(model)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))

				Eventually(func() bool { return pushCalled }, 2*time.Second, 100*time.Millisecond).Should(BeTrue(), "async push should be called to create model+deployment")

				events := drainEvents()
				Expect(events).NotTo(ContainElement(ContainSubstring("ModelNotFound")), "should not emit ModelNotFound for trussConfig")
				Expect(events).To(ContainElement(ContainSubstring("Creating model")), "should emit TrussPushStarted with model creation message")
			})

			It("should proceed normally after push creates model and writes modelID to status", func() {
				name := "truss-model-created-by-push"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				// Simulate: push already completed and wrote modelID to status
				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				configHash := truss.HashTrussConfig(model.Spec.TrussConfig, "")
				deploymentName := truss.DeploymentName(configHash, model.Spec.TrussConfig.BaseImage.Image)

				model.Status.ModelID = testModelID
				model.Status.TrussPushStatus = statusTrussPushDone
				model.Status.TrussConfigHash = configHash
				model.Status.SourceDeploymentName = deploymentName
				Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

				// Now reconcile — should use cached modelID and proceed to environment + promotion
				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					Fail("should not call FindModelIDByName when modelID is cached")
					return "", nil
				}
				mockEnvWithSettings(nil, nil)
				mockClient.FindDeploymentIDByNameFunc = func(ctx context.Context, modelID, depName string) (string, string, error) {
					return testDeploymentID, baseten.DeploymentStatusActive, nil
				}
				mockClient.PromoteFunc = func(ctx context.Context, modelID, depID, env string, s *modelsv1alpha1.PromotionSettingsConfig) (*baseten.Deployment, error) {
					return &baseten.Deployment{ID: "promoted-123", Name: deploymentName + ".123", Status: "DEPLOYING"}, nil
				}

				result, err := reconcileByName(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(30*time.Second), "should requeue for promotion polling")

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(statusPromoting))
			})

			It("should still fail for sourceDeploymentName when model not found", func() {
				name := "source-dep-model-not-found"
				model := newTestModel(name)
				defer cleanupModel(name)

				mockClient.FindModelIDByNameFunc = func(ctx context.Context, modelName string) (string, error) {
					return "", nil
				}

				_, err := reconcileModel(model)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not found"))
				Expect(drainEvents()).To(ContainElement(ContainSubstring("ModelNotFound")))

				status := getModelStatus(name)
				Expect(status.DeploymentStatus).To(Equal(baseten.DeploymentStatusFailed))
			})

			It("should write modelID from push result via updatePushStatus", func() {
				name := "truss-push-writes-model-id"
				model := newTrussConfigModel(name)
				defer cleanupModel(name)

				Expect(k8sClient.Create(ctx, model)).To(Succeed())

				// Directly call updatePushStatus with a modelID
				reconciler.updatePushStatus(name, "default", statusTrussPushDone, "push-created-model-id")

				status := getModelStatus(name)
				Expect(status.ModelID).To(Equal("push-created-model-id"))
				Expect(status.TrussPushStatus).To(Equal(statusTrussPushDone))
			})
		})

		Describe("Resource not found", func() {
			It("should return no error when resource is deleted", func() {
				result, err := reconcileByName("non-existent-resource")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
			})
		})
	})
})
