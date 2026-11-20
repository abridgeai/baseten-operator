package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

func TestWithLeaderElection(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		o := withLeaderElection(ctrl.Options{HealthProbeBindAddress: ":8081"}, enabled)
		if o.LeaderElection != enabled {
			t.Errorf("enabled=%v: LeaderElection = %v", enabled, o.LeaderElection)
		}
		if o.LeaderElectionID != "020848ba.baseten.com" || o.HealthProbeBindAddress != ":8081" {
			t.Errorf("enabled=%v: lease ID changed or other options lost: %+v", enabled, o)
		}
		want := map[string]struct {
			got  *time.Duration
			want time.Duration
		}{
			"LeaseDuration": {o.LeaseDuration, 90 * time.Second},
			"RenewDeadline": {o.RenewDeadline, 40 * time.Second},
			"RetryPeriod":   {o.RetryPeriod, 2 * time.Second},
		}
		for name, d := range want {
			if d.got == nil || *d.got != d.want {
				t.Errorf("enabled=%v: %s = %v, want %v", enabled, name, d.got, d.want)
			}
		}
		if !o.LeaderElectionReleaseOnCancel {
			t.Errorf("enabled=%v: LeaderElectionReleaseOnCancel = false, want true", enabled)
		}
	}
}

// The lease timings apply only when main() builds its manager options through withLeaderElection.
func TestMainWiresLeaderElection(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var calls []*ast.CallExpr
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != "main" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewManager" && isIdent(sel.X, "ctrl") {
				calls = append(calls, call)
			}
			return true
		})
	}
	if len(calls) != 1 {
		t.Fatalf("main() calls ctrl.NewManager %d times, want 1", len(calls))
	}
	opts := calls[0].Args[1]
	wrap, ok := opts.(*ast.CallExpr)
	if !ok || !isIdent(wrap.Fun, "withLeaderElection") {
		t.Fatalf("ctrl.NewManager options in main() = %s, want withLeaderElection(...)", types.ExprString(opts))
	}
	if arg := wrap.Args[1]; !isIdent(arg, "enableLeaderElection") {
		t.Errorf("withLeaderElection enabled argument in main() = %s, want enableLeaderElection",
			types.ExprString(arg))
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
