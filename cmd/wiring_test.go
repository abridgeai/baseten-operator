package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The reconciler reads the setup-script ConfigMap through APIReader; a nil reader panics at runtime.
func TestMainWiresAPIReader(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, _ := kv.Key.(*ast.Ident)
		call, _ := kv.Value.(*ast.CallExpr)
		if key == nil || key.Name != "APIReader" || call == nil {
			return true
		}
		sel, _ := call.Fun.(*ast.SelectorExpr)
		if sel != nil && sel.Sel.Name == "GetAPIReader" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("main.go does not set APIReader: mgr.GetAPIReader() on the reconciler")
	}
}

// The chart ClusterRole is kept by hand; its rules must match the generated role (configmaps: get only).
func TestChartManagerRulesMatchGenerated(t *testing.T) {
	rules := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, after, _ := strings.Cut(string(b), "rules:\n")
		return strings.TrimSuffix(after, "{{- end -}}\n")
	}
	g := rules("../config/rbac/role.yaml")
	c := rules("../charts/baseten-operator/templates/clusterrole-manager.yaml")
	if g != c {
		t.Errorf("chart ClusterRole rules differ from config/rbac/role.yaml:\n--- generated\n%s\n--- chart\n%s", g, c)
	}
}
