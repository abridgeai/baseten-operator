//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/abridgeai/baseten-operator/test/utils"
)

const (
	namespace          = "baseten-operator-system"
	serviceAccountName = "baseten-operator-controller-manager"
	metricsServiceName = "baseten-operator-controller-manager-metrics-service"
	metricsRoleBindingName = "baseten-operator-metrics-binding"
	mockServerURL      = "http://mock-baseten-api.baseten-operator-system.svc.cluster.local:8080/v1"
)

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("creating baseten-operator-api-key secret")
		cmd = exec.Command("kubectl", "create", "secret", "generic", "baseten-operator-api-key",
			"-n", namespace,
			"--from-literal=api-key=mock-api-key-for-e2e-tests")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create baseten-operator-api-key secret")

		By("deploying the mock Baseten API server")
		cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/mock-server-deployment.yaml")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy mock server")

		By("waiting for mock server to be ready")
		verifyMockServerReady := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "app=mock-baseten-api",
				"-n", namespace, "-o", "jsonpath={.items[0].status.conditions[?(@.type=='Ready')].status}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"), "Mock server not ready")
		}
		Eventually(verifyMockServerReady, 2*time.Minute, time.Second).Should(Succeed())

		By("deploying the controller-manager (Helm installs CRDs + operator)")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("patching the controller deployment with BASETEN_API_BASE_URL")
		patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"baseten-operator","env":[{"name":"BASETEN_API_KEY","valueFrom":{"secretKeyRef":{"name":"baseten-operator-api-key","key":"api-key"}}},{"name":"BASETEN_API_BASE_URL","value":"%s"}]}]}}}}`, mockServerURL)
		cmd = exec.Command("kubectl", "patch", "deployment", "baseten-operator-controller-manager",
			"-n", namespace, "-p", patch)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to patch deployment with mock server URL")

		By("waiting for the controller-manager pod to be ready after patch")
		verifyControllerUp := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-o", "go-template={{ range .items }}"+
					"{{ if not .metadata.deletionTimestamp }}"+
					"{{ .metadata.name }}"+
					"{{ \"\\n\" }}{{ end }}{{ end }}",
				"-n", namespace)
			podOutput, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			podNames := utils.GetNonEmptyLines(podOutput)
			g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
			controllerPodName = podNames[0]

			cmd = exec.Command("kubectl", "get", "pods", controllerPodName,
				"-o", "jsonpath={.status.phase}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}
		Eventually(verifyControllerUp, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("deleting test BasetenModel resources")
		cmd = exec.Command("kubectl", "delete", "basetenmodels", "--all", "-n", "default", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager (Helm uninstalls CRDs + operator)")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default", "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			}

			By("Fetching mock server pod logs")
			cmd = exec.Command("kubectl", "logs", "-l", "app=mock-baseten-api", "-n", namespace)
			mockLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Mock server logs:\n%s", mockLogs)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	resetMock := func() {
		cmd := exec.Command("kubectl", "exec", "-n", namespace,
			"deployment/mock-baseten-api", "--",
			"wget", "-q", "-O-", "--post-data", `{"action":"reset"}`,
			"--header", "Content-Type: application/json",
			"http://localhost:8080/v1/_control")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to reset mock server")
	}

	controlMock := func(action string) {
		cmd := exec.Command("kubectl", "exec", "-n", namespace,
			"deployment/mock-baseten-api", "--",
			"wget", "-q", "-O-", "--post-data", fmt.Sprintf(`{"action":"%s"}`, action),
			"--header", "Content-Type: application/json",
			"http://localhost:8080/v1/_control")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to send control command: "+action)
	}

	getStatus := func(name, jsonpath string) string {
		cmd := exec.Command("kubectl", "get", "bm", name,
			"-o", fmt.Sprintf("jsonpath={%s}", jsonpath), "-n", "default")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		return output
	}

	getConditionStatus := func(name, condType string) string {
		jsonpath := fmt.Sprintf(".status.conditions[?(@.type=='%s')].status", condType)
		return getStatus(name, jsonpath)
	}

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			Expect(controllerPodName).To(ContainSubstring("controller-manager"))
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=baseten-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})
	})

	Context("Reconciliation", func() {
		BeforeEach(func() {
			resetMock()
		})

		AfterEach(func() {
			cmd := exec.Command("kubectl", "delete", "bm", "--all", "-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
			time.Sleep(2 * time.Second)
		})

		It("should complete the full promotion lifecycle", func() {
			By("applying a BasetenModel CR")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-lifecycle
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the promotion to be in progress")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-lifecycle", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying Progressing condition is True during promotion")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-lifecycle", "Progressing")).To(Equal("True"))
				g.Expect(getConditionStatus("e2e-lifecycle", "Ready")).To(Equal("False"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("completing the promotion via mock control endpoint")
			controlMock("complete_promotion")

			By("waiting for Ready=True after promotion completes")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-lifecycle", "Ready")).To(Equal("True"))
				g.Expect(getConditionStatus("e2e-lifecycle", "Progressing")).To(Equal("False"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying status fields")
			Expect(getStatus("e2e-lifecycle", ".status.modelID")).To(Equal("model-001"))
			Expect(getStatus("e2e-lifecycle", ".status.activeDeploymentName")).NotTo(BeEmpty())

			By("verifying events were emitted")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-lifecycle",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("EnvironmentCreated"))
			Expect(output).To(ContainSubstring("DeploymentPromoted"))
			Expect(output).To(ContainSubstring("DeploymentActive"))
		})

		It("should report error for nonexistent model", func() {
			By("applying a CR with a model that doesn't exist")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-notfound
  namespace: default
spec:
  modelName: "nonexistent-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for status to become Failed")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-notfound", ".status.deploymentStatus")
				g.Expect(status).To(Equal("FAILED"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying conditions show Degraded (Ready=False, Progressing=False)")
			Expect(getConditionStatus("e2e-notfound", "Ready")).To(Equal("False"))
			Expect(getConditionStatus("e2e-notfound", "Progressing")).To(Equal("False"))

			By("verifying ModelNotFound warning event")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-notfound,type=Warning",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("ModelNotFound"))
		})

		It("should create environment with autoscaling and detect drift", func() {
			By("applying a CR with autoscaling settings")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-autoscaling
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "staging"
    autoscaling:
      minReplicas: 2
      maxReplicas: 10
      concurrencyTarget: 20`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for promotion to start (environment created first)")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-autoscaling", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying EnvironmentCreated event was emitted")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-autoscaling",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("EnvironmentCreated"))

			By("completing the promotion")
			// Control the mock for staging env
			cmd = exec.Command("kubectl", "exec", "-n", namespace,
				"deployment/mock-baseten-api", "--",
				"wget", "-q", "-O-", "--post-data", `{"action":"complete_promotion","env_name":"staging"}`,
				"--header", "Content-Type: application/json",
				"http://localhost:8080/v1/_control")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Ready=True")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-autoscaling", "Ready")).To(Equal("True"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("updating the CR autoscaling to trigger drift detection")
			crUpdated := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-autoscaling
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "staging"
    autoscaling:
      minReplicas: 4
      maxReplicas: 20
      concurrencyTarget: 30`
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(crUpdated)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying AutoscalingUpdated event was emitted after drift reconciliation")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events", "-n", "default",
					"--field-selector", "involvedObject.name=e2e-autoscaling",
					"-o", "jsonpath={.items[*].reason}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("AutoscalingUpdated"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should detect and reconcile promotion settings drift", func() {
			By("applying a CR with promotion settings")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-promo-drift
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "staging"
    autoscaling:
      minReplicas: 0
      maxReplicas: 5
      concurrencyTarget: 10
    promotionSettings:
      rollingDeploy: true
      rampUpDurationSeconds: 300`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for promotion to start")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-promo-drift", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("completing the promotion")
			cmd = exec.Command("kubectl", "exec", "-n", namespace,
				"deployment/mock-baseten-api", "--",
				"wget", "-q", "-O-", "--post-data", `{"action":"complete_promotion","env_name":"staging"}`,
				"--header", "Content-Type: application/json",
				"http://localhost:8080/v1/_control")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Ready=True")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-promo-drift", "Ready")).To(Equal("True"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("updating promotion settings to trigger drift")
			crUpdated := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-promo-drift
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "staging"
    autoscaling:
      minReplicas: 0
      maxReplicas: 5
      concurrencyTarget: 10
    promotionSettings:
      rollingDeploy: true
      rampUpDurationSeconds: 600
      promotionCleanupStrategy: "SCALE_TO_ZERO"`
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(crUpdated)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PromotionSettingsUpdated event was emitted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events", "-n", "default",
					"--field-selector", "involvedObject.name=e2e-promo-drift",
					"-o", "jsonpath={.items[*].reason}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("PromotionSettingsUpdated"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should trigger new promotion when sourceDeploymentName changes", func() {
			By("applying a BasetenModel CR")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-version-update
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for initial promotion")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-version-update", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("completing the first promotion")
			controlMock("complete_promotion")

			By("waiting for Ready=True")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-version-update", "Ready")).To(Equal("True"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying the active deployment from first promotion")
			activeV1 := getStatus("e2e-version-update", ".status.activeDeploymentName")
			Expect(activeV1).To(ContainSubstring("img-1.0-wgt-1.0-p-1.0"))

			By("updating the sourceDeploymentName to v2")
			crV2 := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-version-update
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-2.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(crV2)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for new promotion to start")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-version-update", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
				g.Expect(getConditionStatus("e2e-version-update", "Ready")).To(Equal("False"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying all expected events")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-version-update",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("EnvironmentCreated"))
			Expect(output).To(ContainSubstring("DeploymentPromoted"))
			Expect(output).To(ContainSubstring("DeploymentActive"))

			By("verifying v2 deployment was promoted")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-version-update,reason=DeploymentPromoted",
				"-o", "jsonpath={.items[*].message}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("img-2.0-wgt-1.0-p-1.0"))
		})

		It("should run orphan deployment cleanup after promotion completes", func() {
			By("applying a BasetenModel CR with cleanup enabled")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-cleanup
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"
  orphanDeploymentCleanup:
    scaleToZero: true
    delete: true
    deleteAfterDays: 30
    minToKeep: 1
    intervalMinutes: 10`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for promotion to start")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-cleanup", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("seeding orphan deployments via mock control BEFORE completing promotion")
			cmd = exec.Command("kubectl", "exec", "-n", namespace,
				"deployment/mock-baseten-api", "--",
				"wget", "-q", "-O-", "--post-data", `{"action":"setup_cleanup_test"}`,
				"--header", "Content-Type: application/json",
				"http://localhost:8080/v1/_control")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("completing the promotion")
			controlMock("complete_promotion")

			By("waiting for Ready=True and cleanup to run")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-cleanup", "Ready")).To(Equal("True"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("waiting for lastCleanupTime to be set (cleanup runs on first steady-state reconcile)")
			Eventually(func(g Gomega) {
				output := getStatus("e2e-cleanup", ".status.lastCleanupTime")
				g.Expect(output).NotTo(BeEmpty(), "lastCleanupTime should be set after cleanup runs")
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying orphan cleanup events were emitted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events", "-n", "default",
					"--field-selector", "involvedObject.name=e2e-cleanup",
					"-o", "jsonpath={.items[*].reason}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("OrphanDeploymentsScaledIn"))
				g.Expect(output).To(ContainSubstring("OrphanDeploymentsDeleted"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("verifying mock state: orphan-active-leak should have been scaled in (min_replica set to 0)")
			cmd = exec.Command("kubectl", "exec", "-n", namespace,
				"deployment/mock-baseten-api", "--",
				"wget", "-q", "-O-", "--post-data", `{"action":"get_deployments"}`,
				"--header", "Content-Type: application/json",
				"http://localhost:8080/v1/_control")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var depsResp struct {
				Deployments []struct {
					ID                  string `json:"id"`
					Name                string `json:"name"`
					Status              string `json:"status"`
					IsProduction        bool   `json:"is_production"`
					AutoscalingSettings *struct {
						MinReplica int32 `json:"min_replica"`
					} `json:"autoscaling_settings"`
				} `json:"deployments"`
			}
			Expect(json.Unmarshal([]byte(output), &depsResp)).To(Succeed())

			// Verify orphan-active-leak was scaled in
			for _, dep := range depsResp.Deployments {
				if dep.ID == "orphan-active-leak" {
					Expect(dep.AutoscalingSettings).NotTo(BeNil())
					Expect(dep.AutoscalingSettings.MinReplica).To(Equal(int32(0)), "orphan-active-leak should have min_replica=0 after scale-in")
				}
			}

			// Verify INACTIVE stale orphan was deleted (orphan-inactive-old, created 2025-06-01, > 30 days)
			deletedIDs := map[string]bool{}
			remainingIDs := map[string]bool{}
			for _, dep := range depsResp.Deployments {
				remainingIDs[dep.ID] = true
			}
			// orphan-inactive-old (2025-06-01) should be deleted — INACTIVE and older than 30 days
			Expect(remainingIDs).NotTo(HaveKey("orphan-inactive-old"), "INACTIVE stale orphan should have been deleted")
			// orphan-inactive-stale is the newest INACTIVE orphan (2025-09-01) — kept by minToKeep=1
			Expect(remainingIDs).To(HaveKey("orphan-inactive-stale"), "newest INACTIVE orphan should be kept by minToKeep")
			// orphan-scaled-zero should NOT be deleted — SCALED_TO_ZERO is not INACTIVE
			Expect(remainingIDs).To(HaveKey("orphan-scaled-zero"), "SCALED_TO_ZERO orphan should not be deleted")
			// protected-prod should NOT be touched — is_production flag
			Expect(remainingIDs).To(HaveKey("protected-prod"), "is_production deployment should never be touched")

			_ = deletedIDs // suppress unused warning
		})

		It("should compute trussConfig hash and attempt push with all fields", func() {
			resetMock()

			By("creating ConfigMap with setup script")
			cmCR := `apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-truss-setup
  namespace: default
data:
  setup.sh: |
    #!/bin/bash
    echo hello`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cmCR)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("applying a BasetenModel CR with full trussConfig (all fields populated)")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-trussconfig
  namespace: default
spec:
  modelName: "test-model"
  trussConfig:
    pythonVersion: "py312"
    resources:
      accelerator: "H100:1"
      useGpu: true
    secrets:
      docker-registry-secret: ""
      datadog-api-key: ""
    environmentVariables:
      DD_SITE: "us5.datadoghq.com"
      DD_ENV: "baseten"
      DD_SERVICE: "vllm-test"
    baseImage:
      image: "us-docker.pkg.dev/test/vllm:0.11.2.1"
      dockerAuth:
        authMethod: "GCP_SERVICE_ACCOUNT_JSON"
        secretName: "docker-registry-secret"
        registry: "us-docker.pkg.dev"
    dockerServer:
      startCommand: "sh -c 'bash /app/data/setup.sh'"
      readinessEndpoint: "/health"
      livenessEndpoint: "/health"
      predictEndpoint: "/v1/completions"
      serverPort: 8000
    runtime:
      predictConcurrency: 256
    modelMetadata:
      tags:
        - "openai-compatible"
    setupScript:
      configMapRef:
        name: e2e-truss-setup
        key: setup.sh
  environment:
    name: "dev"
    autoscaling:
      minReplicas: 0
      maxReplicas: 5
      concurrencyTarget: 10`
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment creation to be in progress")
			// The async push will fail (mock doesn't support truss-go GraphQL),
			// but the hash computation, ConfigMap reading, and status update should succeed.
			Eventually(func(g Gomega) {
				msg := getStatus("e2e-trussconfig", ".status.message")
				g.Expect(msg).To(ContainSubstring("truss push"), "should show truss push message")
				g.Expect(msg).To(ContainSubstring("depl-"), "should include deployment name with depl- prefix")
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying status is DEPLOYING (creating deployment)")
			status := getStatus("e2e-trussconfig", ".status.deploymentStatus")
			Expect(status).To(Equal("DEPLOYING"))

			By("cleanup")
			cmd = exec.Command("kubectl", "delete", "bm", "--all", "-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "configmap", "e2e-truss-setup", "-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should detect config change when image is upgraded", func() {
			resetMock()

			By("applying initial trussConfig CR (v1 image)")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-config-change
  namespace: default
spec:
  modelName: "test-model"
  trussConfig:
    resources:
      accelerator: "H100:1"
    baseImage:
      image: "us-docker.pkg.dev/test/vllm:0.11.2.1"
  environment:
    name: "dev"
    autoscaling:
      minReplicas: 0
      maxReplicas: 5
      concurrencyTarget: 10`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment creation and capturing initial deployment name")
			// Async push will run against mock (and fail), but status shows the deployment name
			Eventually(func(g Gomega) {
				msg := getStatus("e2e-config-change", ".status.message")
				g.Expect(msg).To(ContainSubstring("depl-"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			initialMsg := getStatus("e2e-config-change", ".status.message")

			By("updating the image tag to trigger config change")
			crV2 := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-config-change
  namespace: default
spec:
  modelName: "test-model"
  trussConfig:
    resources:
      accelerator: "H100:1"
    baseImage:
      image: "us-docker.pkg.dev/test/vllm:0.16.0.0"
  environment:
    name: "dev"
    autoscaling:
      minReplicas: 0
      maxReplicas: 5
      concurrencyTarget: 10`
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(crV2)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for message to change (different deployment name = different hash)")
			Eventually(func(g Gomega) {
				newMsg := getStatus("e2e-config-change", ".status.message")
				g.Expect(newMsg).To(ContainSubstring("depl-"))
				g.Expect(newMsg).NotTo(Equal(initialMsg), "message should change after image upgrade (different hash)")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("cleanup")
			cmd = exec.Command("kubectl", "delete", "bm", "--all", "-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should fail gracefully when ConfigMap not found", func() {
			resetMock()

			By("applying CR referencing nonexistent ConfigMap")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-missing-cm
  namespace: default
spec:
  modelName: "test-model"
  trussConfig:
    resources:
      accelerator: "H100:1"
    baseImage:
      image: "us-docker.pkg.dev/test/vllm:0.11.2.1"
    setupScript:
      configMapRef:
        name: nonexistent-configmap
        key: setup.sh
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Degraded status")
			Eventually(func(g Gomega) {
				ready := getConditionStatus("e2e-missing-cm", "Ready")
				g.Expect(ready).To(Equal("False"))
				progressing := getConditionStatus("e2e-missing-cm", "Progressing")
				g.Expect(progressing).To(Equal("False"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("verifying error message mentions ConfigMap")
			msg := getStatus("e2e-missing-cm", ".status.message")
			Expect(msg).To(ContainSubstring("ConfigMap"))

			By("verifying SetupScriptNotFound event")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-missing-cm,type=Warning",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("SetupScriptNotFound"))

			By("cleanup")
			cmd = exec.Command("kubectl", "delete", "bm", "--all", "-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should pause and resume reconciliation", func() {
			By("applying a BasetenModel CR")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-pause
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for promotion to start")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-pause", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("completing promotion")
			controlMock("complete_promotion")

			By("waiting for Ready=True")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-pause", "Ready")).To(Equal("True"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("pausing reconciliation")
			cmd = exec.Command("kubectl", "patch", "bm", "e2e-pause", "-n", "default",
				"--type", "merge", "-p", `{"spec":{"paused":true}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for PAUSED status with last status preserved")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-pause", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PAUSED"))
				msg := getStatus("e2e-pause", ".status.message")
				g.Expect(msg).To(ContainSubstring("reconciliation paused | last status:"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("verifying conditions show Degraded (Ready=False, Progressing=False)")
			Expect(getConditionStatus("e2e-pause", "Ready")).To(Equal("False"))
			Expect(getConditionStatus("e2e-pause", "Progressing")).To(Equal("False"))

			By("unpausing reconciliation")
			cmd = exec.Command("kubectl", "patch", "bm", "e2e-pause", "-n", "default",
				"--type", "merge", "-p", `{"spec":{"paused":false}}`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Ready=True after unpause")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-pause", "Ready")).To(Equal("True"))
				g.Expect(getConditionStatus("e2e-pause", "Progressing")).To(Equal("False"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying ReconciliationPaused event was emitted")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-pause",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("ReconciliationPaused"))
		})

		It("should retry failed candidate deployment and recover", func() {
			resetMock()
			By("applying a BasetenModel CR")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-retry
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for promotion to start")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-retry", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("failing the promotion with BUILD_FAILED")
			controlMock("fail_promotion")

			By("waiting for DeploymentRetried event (confirms retry API was called — includes 2m+ exponential backoff)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events", "-n", "default",
					"--field-selector", "involvedObject.name=e2e-retry",
					"-o", "jsonpath={.items[*].reason}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("DeploymentRetried"))
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By("completing the retry via mock")
			controlMock("complete_retry")

			By("waiting for Ready=True after retry succeeds")
			Eventually(func(g Gomega) {
				g.Expect(getConditionStatus("e2e-retry", "Ready")).To(Equal("True"))
				g.Expect(getConditionStatus("e2e-retry", "Progressing")).To(Equal("False"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying retry state is cleared")
			failureTime := getStatus("e2e-retry", ".status.firstDeploymentFailureTime")
			Expect(failureTime).To(BeEmpty(), "firstDeploymentFailureTime should be cleared on success")
			retryCount := getStatus("e2e-retry", ".status.deploymentRetryCount")
			Expect(retryCount).To(SatisfyAny(BeEmpty(), Equal("0")), "deploymentRetryCount should be cleared on success")
			nextRetry := getStatus("e2e-retry", ".status.nextRetryTime")
			Expect(nextRetry).To(BeEmpty(), "nextRetryTime should be cleared on success")
		})

		It("should handle promotion failure", func() {
			By("applying a BasetenModel CR")
			cr := `apiVersion: models.baseten.com/v1alpha1
kind: BasetenModel
metadata:
  name: e2e-fail
  namespace: default
spec:
  modelName: "test-model"
  sourceDeploymentName: "img-1.0-wgt-1.0-p-1.0"
  environment:
    name: "dev"`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(cr)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for promotion to start")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-fail", ".status.deploymentStatus")
				g.Expect(status).To(Equal("PROMOTING"))
			}, 60*time.Second, 2*time.Second).Should(Succeed())

			By("triggering non-retryable failure (BUILD_STOPPED) via mock")
			controlMock("fail_promotion_stopped")

			By("waiting for status to reflect BUILD_STOPPED")
			Eventually(func(g Gomega) {
				status := getStatus("e2e-fail", ".status.deploymentStatus")
				g.Expect(status).To(Equal("BUILD_STOPPED"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying conditions show Degraded")
			Expect(getConditionStatus("e2e-fail", "Ready")).To(Equal("False"))
			Expect(getConditionStatus("e2e-fail", "Progressing")).To(Equal("False"))

			By("verifying all expected events")
			cmd = exec.Command("kubectl", "get", "events", "-n", "default",
				"--field-selector", "involvedObject.name=e2e-fail",
				"-o", "jsonpath={.items[*].reason}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("EnvironmentCreated"))
			Expect(output).To(ContainSubstring("DeploymentPromoted"))
			Expect(output).To(ContainSubstring("PromotionFailed"))
		})
	})
})

func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

