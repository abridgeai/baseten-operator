package v1alpha1

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// Helm and Config Sync read only metadata.annotations, so a key under labels does not protect the CRD.
func TestCRDKeepsAnnotations(t *testing.T) {
	want := map[string]string{
		"helm.sh/resource-policy":                 "keep",
		"client.lifecycle.config.k8s.io/deletion": "detach",
	}
	for _, p := range []string{
		"../../config/crd/bases/models.baseten.com_basetenmodels.yaml",
		"../../charts/baseten-operator/files/basetenmodel-crd.yaml",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(b, &crd); err != nil {
			t.Fatal(err)
		}
		for k, v := range want {
			if got := crd.Metadata.Annotations[k]; got != v {
				t.Errorf("%s: annotation %s = %q, want %q", p, k, got, v)
			}
		}
	}
}
