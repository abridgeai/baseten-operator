package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"k8s.io/client-go/rest"
)

func TestWithClientRateLimit(t *testing.T) {
	cfg := withClientRateLimit(&rest.Config{QPS: -1})
	if cfg.QPS != 10 || cfg.Burst != 20 {
		t.Errorf("QPS, Burst = %v, %v; want 10, 20", cfg.QPS, cfg.Burst)
	}
}

func TestMainRateLimitsManagerConfig(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "NewManager" {
			return true
		}
		if types.ExprString(call.Args[0]) == "withClientRateLimit(ctrl.GetConfigOrDie())" {
			wired = true
		}
		return true
	})
	if !wired {
		t.Error("ctrl.NewManager in main.go does not take withClientRateLimit(...)")
	}
}
