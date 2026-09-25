package baseten

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
)

func specOvernight() modelsv1alpha1.AutoscalingSchedule {
	return modelsv1alpha1.AutoscalingSchedule{
		Name:      "overnight",
		Cadence:   "DAILY",
		Weekdays:  []modelsv1alpha1.Weekday{"MONDAY", "TUESDAY"},
		StartHour: ptr(int32(22)),
		EndHour:   ptr(int32(7)),
		Autoscaling: modelsv1alpha1.ScheduleAutoscalingConfig{
			MinReplicas: 1,
			MaxReplicas: 10,
		},
	}
}

func observedOvernight() AutoscalingSchedule {
	return AutoscalingSchedule{
		ID:        "sched-1",
		Name:      "overnight",
		Enabled:   true,
		Cadence:   "DAILY",
		Weekdays:  []string{"TUESDAY", "MONDAY"},
		StartHour: ptr(int32(22)),
		EndHour:   ptr(int32(7)),
		AutoscalingSettings: ScheduleAutoscalingSettings{
			MinReplica: 1,
			MaxReplica: 10,
		},
	}
}

func specOneTime() modelsv1alpha1.AutoscalingSchedule {
	return modelsv1alpha1.AutoscalingSchedule{
		Name:        "launch",
		Cadence:     CadenceOneTime,
		StartAt:     "2026-10-01T09:00:00-04:00",
		EndAt:       "2026-10-01T17:00:00-04:00",
		Autoscaling: modelsv1alpha1.ScheduleAutoscalingConfig{MinReplicas: 4, MaxReplicas: 20},
	}
}

func withID(s AutoscalingSchedule, id string) AutoscalingSchedule {
	s.ID = id
	return s
}

func hourly() modelsv1alpha1.AutoscalingSchedule {
	return modelsv1alpha1.AutoscalingSchedule{
		Name:        "hourly-burst",
		Cadence:     "HOURLY",
		Weekdays:    []modelsv1alpha1.Weekday{"MONDAY"},
		StartMinute: 0,
		EndMinute:   15,
		Autoscaling: modelsv1alpha1.ScheduleAutoscalingConfig{MinReplicas: 2, MaxReplicas: 4},
	}
}

func observedHourly() AutoscalingSchedule {
	return AutoscalingSchedule{
		ID: "sched-h", Name: "hourly-burst", Enabled: true, Cadence: "HOURLY",
		Weekdays: []string{"MONDAY"}, StartHour: ptr(int32(0)), EndHour: ptr(int32(0)), EndMinute: 15,
		AutoscalingSettings: ScheduleAutoscalingSettings{MinReplica: 2, MaxReplica: 4},
	}
}

func expiredOneTime() modelsv1alpha1.AutoscalingSchedule {
	s := specOneTime()
	s.Name, s.StartAt, s.EndAt = "past", "2020-01-01T09:00:00Z", "2020-01-01T17:00:00Z"
	return s
}

func TestBuildScheduleSettingsDedupesDeletes(t *testing.T) {
	a, b := observedOvernight(), withID(observedOvernight(), "sched-dup")
	settings := buildScheduleSettings(&modelsv1alpha1.AutoscalingScheduleConfig{}, &AutoscalingSchedules{Schedules: []AutoscalingSchedule{a, b}})
	if got := settings["delete_schedules"]; !reflect.DeepEqual(got, []string{"sched-dup", "sched-1"}) {
		t.Errorf("delete_schedules = %v, want each id once", got)
	}
}

func TestHasScheduleDrift(t *testing.T) {
	changedMin := specOvernight()
	changedMin.Autoscaling.MinReplicas = 2
	disabled := specOvernight()
	disabled.Enabled = ptr(false)
	withWindow := specOvernight()
	withWindow.Autoscaling.AutoscalingWindow = ptr(int32(60))

	tests := []struct {
		name        string
		spec        *modelsv1alpha1.AutoscalingScheduleConfig
		observed    *AutoscalingSchedules
		wantChanges []string
	}{
		{"nil spec is unmanaged", nil, &AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight()}}, nil},
		{
			"no-op when in sync, ignoring weekday order and defaulting enabled",
			&modelsv1alpha1.AutoscalingScheduleConfig{Timezone: ptr("America/New_York"), Schedules: []modelsv1alpha1.AutoscalingSchedule{specOvernight()}},
			&AutoscalingSchedules{Timezone: ptr("America/New_York"), Schedules: []AutoscalingSchedule{observedOvernight()}},
			nil,
		},
		{
			"create when missing, including nil observed",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{specOvernight()}},
			nil,
			[]string{"schedule overnight added"},
		},
		{
			"replace on autoscaling change",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{changedMin}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight()}},
			[]string{"schedule overnight changed"},
		},
		{
			"replace on enabled change",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{disabled}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight()}},
			[]string{"schedule overnight changed"},
		},
		{
			"spec nil optional compares as null",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{withWindow}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight()}},
			[]string{"schedule overnight changed"},
		},
		{
			"delete when not in spec",
			&modelsv1alpha1.AutoscalingScheduleConfig{},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight()}},
			[]string{"schedule overnight removed"},
		},
		{
			"timezone change",
			&modelsv1alpha1.AutoscalingScheduleConfig{Timezone: ptr("America/New_York")},
			&AutoscalingSchedules{Timezone: ptr("UTC")},
			[]string{"scheduleTimezone UTC→America/New_York"},
		},
		{
			"nil spec timezone keeps current",
			&modelsv1alpha1.AutoscalingScheduleConfig{},
			&AutoscalingSchedules{Timezone: ptr("UTC")},
			nil,
		},
		{
			"one-time equal instants in different offsets",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{specOneTime()}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{{
				ID: "sched-2", Name: "launch", Enabled: true, Cadence: CadenceOneTime,
				StartAt: "2026-10-01T13:00:00Z", EndAt: "2026-10-01T21:00:00Z",
				AutoscalingSettings: ScheduleAutoscalingSettings{MinReplica: 4, MaxReplica: 20},
			}}},
			nil,
		},
		{
			"duplicate observed names are removed",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{specOvernight()}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight(), withID(observedOvernight(), "sched-dup")}},
			[]string{"schedule overnight duplicate removed"},
		},
		{
			"hourly ignores hours echoed by the API",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{hourly()}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedHourly()}},
			nil,
		},
		{
			"expired one-time is neither created nor deleted",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{expiredOneTime(), specOvernight()}},
			&AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight(), {ID: "sched-exp", Name: "past", Cadence: CadenceOneTime}}},
			nil,
		},
		{
			"expired one-time missing from Baseten is not created",
			&modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{expiredOneTime()}},
			nil,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDrift, gotChanges := HasScheduleDrift(tt.spec, tt.observed)
			if gotDrift != (len(tt.wantChanges) > 0) {
				t.Errorf("HasScheduleDrift() drift = %v, want %v", gotDrift, len(tt.wantChanges) > 0)
			}
			if !reflect.DeepEqual(gotChanges, tt.wantChanges) {
				t.Errorf("HasScheduleDrift() changes = %v, want %v", gotChanges, tt.wantChanges)
			}
		})
	}
}

func TestUpdateAutoscalingSchedules(t *testing.T) {
	capture := func(t *testing.T, gotBody *map[string]interface{}, calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls++
			if r.Method != http.MethodPatch {
				t.Errorf("expected PATCH, got %s", r.Method)
			}
			if r.URL.Path != "/models/model1/environments/dev" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
				t.Fatalf("failed to decode body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		}))
	}

	t.Run("create, replace, and delete in one request", func(t *testing.T) {
		var body map[string]interface{}
		calls := 0
		srv := capture(t, &body, &calls)
		defer srv.Close()

		changed := specOvernight()
		changed.Autoscaling.MinReplicas = 3
		spec := &modelsv1alpha1.AutoscalingScheduleConfig{
			Timezone:  ptr("America/New_York"),
			Schedules: []modelsv1alpha1.AutoscalingSchedule{changed, specOneTime()},
		}
		stale := observedOvernight()
		stale.ID, stale.Name = "sched-old", "stale"
		observed := &AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight(), stale}}

		if err := newTestClient(srv.URL).UpdateAutoscalingSchedules(context.Background(), "model1", "dev", spec, observed); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		settings := body["autoscaling_schedule_settings"].(map[string]interface{})
		if settings["timezone"] != "America/New_York" {
			t.Errorf("timezone = %v", settings["timezone"])
		}
		if got := settings["delete_schedules"]; !reflect.DeepEqual(got, []interface{}{"sched-old"}) {
			t.Errorf("delete_schedules = %v", got)
		}
		schedules := settings["schedules"].([]interface{})
		if len(schedules) != 2 {
			t.Fatalf("expected 2 upserts, got %d", len(schedules))
		}

		replaced := schedules[0].(map[string]interface{})
		if replaced["id"] != "sched-1" {
			t.Errorf("replaced id = %v, want sched-1", replaced["id"])
		}
		if replaced["start_minute"] != float64(0) || replaced["end_hour"] != float64(7) {
			t.Errorf("recurring window not serialized: %v", replaced)
		}
		as := replaced["autoscaling_settings"].(map[string]interface{})
		wantKeys := []string{"min_replica", "max_replica", "autoscaling_window", "scale_down_delay", "concurrency_target", "target_utilization_percentage", "target_in_flight_tokens", "max_scale_down_rate"}
		if len(as) != len(wantKeys) {
			t.Errorf("autoscaling_settings has %d keys, want %d: %v", len(as), len(wantKeys), as)
		}
		for _, k := range wantKeys {
			v, ok := as[k]
			if !ok {
				t.Errorf("autoscaling_settings missing %s", k)
			}
			if k != "min_replica" && k != "max_replica" && v != nil {
				t.Errorf("%s = %v, want null", k, v)
			}
		}
		if as["min_replica"] != float64(3) {
			t.Errorf("min_replica = %v, want 3", as["min_replica"])
		}

		created := schedules[1].(map[string]interface{})
		if _, ok := created["id"]; ok {
			t.Error("created schedule must not send id")
		}
		for _, k := range []string{"weekdays", "start_hour", "start_minute", "end_hour", "end_minute"} {
			if _, ok := created[k]; ok {
				t.Errorf("ONE_TIME schedule must not send %s", k)
			}
		}
		if created["start_at"] != "2026-10-01T09:00:00-04:00" || created["enabled"] != true {
			t.Errorf("unexpected one-time schedule: %v", created)
		}
	})

	t.Run("no request when in sync", func(t *testing.T) {
		var body map[string]interface{}
		calls := 0
		srv := capture(t, &body, &calls)
		defer srv.Close()

		spec := &modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{specOvernight()}}
		observed := &AutoscalingSchedules{Schedules: []AutoscalingSchedule{observedOvernight()}}
		if err := newTestClient(srv.URL).UpdateAutoscalingSchedules(context.Background(), "model1", "dev", spec, observed); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls != 0 {
			t.Errorf("expected no request, got %d", calls)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusBadRequest, "overlap")
		}))
		defer srv.Close()

		spec := &modelsv1alpha1.AutoscalingScheduleConfig{Schedules: []modelsv1alpha1.AutoscalingSchedule{specOvernight()}}
		if err := newTestClient(srv.URL).UpdateAutoscalingSchedules(context.Background(), "model1", "dev", spec, nil); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestGetEnvironmentDecodesSchedules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"dev","autoscaling_schedules":{"timezone":"UTC","schedules":[{"id":"sched-1","name":"overnight","enabled":true,"cadence":"DAILY","weekdays":["MONDAY"],"start_hour":22,"start_minute":0,"end_hour":7,"end_minute":0,"autoscaling_settings":{"min_replica":1,"max_replica":10,"autoscaling_window":null,"scale_down_delay":null,"concurrency_target":null,"target_utilization_percentage":null,"target_in_flight_tokens":null,"max_scale_down_rate":null}}],"applied_state":{"schedule_id":"sched-1","autoscaling_settings":{"min_replica":1,"max_replica":10,"concurrency_target":1}}}}`))
	}))
	defer srv.Close()

	env, err := newTestClient(srv.URL).GetEnvironment(context.Background(), "model1", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ScheduleActive(env) {
		t.Error("expected schedule to be active")
	}
	if got := env.AutoscalingSchedules.Schedules[0]; got.ID != "sched-1" || *got.StartHour != 22 || got.AutoscalingSettings.MaxReplica != 10 {
		t.Errorf("unexpected decoded schedule: %+v", got)
	}
}

func TestScheduleActive(t *testing.T) {
	tests := []struct {
		name string
		env  *Environment
		want bool
	}{
		{"nil env", nil, false},
		{"no schedules", &Environment{}, false},
		{"no applied state", &Environment{AutoscalingSchedules: &AutoscalingSchedules{}}, false},
		{"baseline applied", &Environment{AutoscalingSchedules: &AutoscalingSchedules{AppliedState: &AutoscalingScheduleState{}}}, false},
		{"schedule applied", &Environment{AutoscalingSchedules: &AutoscalingSchedules{AppliedState: &AutoscalingScheduleState{ScheduleID: ptr("sched-1")}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScheduleActive(tt.env); got != tt.want {
				t.Errorf("ScheduleActive() = %v, want %v", got, tt.want)
			}
		})
	}
}
