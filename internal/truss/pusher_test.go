package truss

import (
	"reflect"
	"strconv"
	"testing"

	truss "github.com/basetenlabs/truss-go"
)

// applyPushOptions runs the SDK option closures against a fresh truss-go
// pushOptions struct (unexported there, hence reflection) and returns its
// string and bool fields by name.
func applyPushOptions(t *testing.T, opts []truss.PushOption) map[string]string {
	t.Helper()
	if len(opts) == 0 {
		t.Fatal("no push options")
	}
	target := reflect.New(reflect.TypeOf(opts[0]).In(0).Elem())
	for _, opt := range opts {
		reflect.ValueOf(opt).Call([]reflect.Value{target})
	}
	fields := map[string]string{}
	s := target.Elem()
	for i := 0; i < s.NumField(); i++ {
		switch f := s.Field(i); f.Kind() {
		case reflect.String:
			fields[s.Type().Field(i).Name] = f.String()
		case reflect.Bool:
			fields[s.Type().Field(i).Name] = strconv.FormatBool(f.Bool())
		}
	}
	return fields
}

func TestPushOptions(t *testing.T) {
	tests := []struct {
		name        string
		team        string
		environment string
		want        map[string]string
	}{
		{
			name: "standalone push",
			want: map[string]string{"publish": "true", "promote": "false", "deploymentName": "depl-vllm-0.16.0-a3f7c2b1", "teamName": "", "environment": ""},
		},
		{
			name: "team-scoped push",
			team: "Single Tenant (Evals)",
			want: map[string]string{"publish": "true", "teamName": "Single Tenant (Evals)", "environment": ""},
		},
		{
			name:        "push into an environment",
			environment: "staging-ori-dfw",
			want:        map[string]string{"publish": "true", "promote": "false", "deploymentName": "depl-vllm-0.16.0-a3f7c2b1", "teamName": "", "environment": "staging-ori-dfw"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPushOptions(t, pushOptions("depl-vllm-0.16.0-a3f7c2b1", tc.team, tc.environment))
			for field, want := range tc.want {
				if got[field] != want {
					t.Errorf("%s = %q, want %q (all: %v)", field, got[field], want, got)
				}
			}
		})
	}
}
