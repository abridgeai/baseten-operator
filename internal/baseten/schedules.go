package baseten

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	modelsv1alpha1 "github.com/abridgeai/baseten-operator/api/v1alpha1"
)

const CadenceOneTime = "ONE_TIME"

// AutoscalingSchedules represents the environment's schedule collection from API response (EnvironmentAutoscalingSchedulesV1).
type AutoscalingSchedules struct {
	Timezone     *string                   `json:"timezone"`
	Schedules    []AutoscalingSchedule     `json:"schedules"`
	AppliedState *AutoscalingScheduleState `json:"applied_state"`
}

// AutoscalingScheduleState reports which schedule, if any, is currently driving autoscaling.
type AutoscalingScheduleState struct {
	ScheduleID          *string              `json:"schedule_id"`
	AutoscalingSettings *AutoscalingSettings `json:"autoscaling_settings"`
}

// AutoscalingSchedule represents a single schedule from API response (AutoscalingScheduleV1 / OneTimeAutoscalingScheduleV1).
type AutoscalingSchedule struct {
	ID                  string                      `json:"id"`
	Name                string                      `json:"name"`
	Enabled             bool                        `json:"enabled"`
	Cadence             string                      `json:"cadence"`
	Weekdays            []string                    `json:"weekdays"`
	StartHour           *int32                      `json:"start_hour"`
	StartMinute         int32                       `json:"start_minute"`
	EndHour             *int32                      `json:"end_hour"`
	EndMinute           int32                       `json:"end_minute"`
	StartAt             string                      `json:"start_at"`
	EndAt               string                      `json:"end_at"`
	AutoscalingSettings ScheduleAutoscalingSettings `json:"autoscaling_settings"`
}

// ScheduleAutoscalingSettings mirrors AutoscalingScheduleSettingsRequestV1, where every key is required and nil means inherit.
type ScheduleAutoscalingSettings struct {
	MinReplica                  int32  `json:"min_replica"`
	MaxReplica                  int32  `json:"max_replica"`
	AutoscalingWindow           *int32 `json:"autoscaling_window"`
	ScaleDownDelay              *int32 `json:"scale_down_delay"`
	ConcurrencyTarget           *int32 `json:"concurrency_target"`
	TargetUtilizationPercentage *int32 `json:"target_utilization_percentage"`
	TargetInFlightTokens        *int32 `json:"target_in_flight_tokens"`
	MaxScaleDownRate            *int32 `json:"max_scale_down_rate"`
}

// ScheduleActive reports whether a schedule window is currently applied to the environment.
func ScheduleActive(env *Environment) bool {
	return env != nil && env.AutoscalingSchedules != nil && env.AutoscalingSchedules.AppliedState != nil &&
		env.AutoscalingSchedules.AppliedState.ScheduleID != nil
}

// HasScheduleDrift compares the spec schedule collection with the environment's and returns drift details.
// A nil spec means schedules are unmanaged and never drift.
func HasScheduleDrift(spec *modelsv1alpha1.AutoscalingScheduleConfig, observed *AutoscalingSchedules) (bool, []string) {
	d := diffSchedules(spec, observed)
	return len(d.changes) > 0, d.changes
}

// UpdateAutoscalingSchedules PATCHes the environment so its schedules match spec: creates missing
// schedules, replaces changed ones by id, and deletes schedules not in spec.
func (c *Client) UpdateAutoscalingSchedules(ctx context.Context, modelID, envName string, spec *modelsv1alpha1.AutoscalingScheduleConfig, observed *AutoscalingSchedules) error {
	settings := buildScheduleSettings(spec, observed)
	if settings == nil {
		return nil
	}

	body, err := json.Marshal(map[string]interface{}{"autoscaling_schedule_settings": settings})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := c.newRequest(ctx, "PATCH", fmt.Sprintf("%s/models/%s/environments/%s", c.baseURL, modelID, envName), bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

type scheduleDiff struct {
	upserts []AutoscalingSchedule
	deletes []string
	changes []string
}

func diffSchedules(spec *modelsv1alpha1.AutoscalingScheduleConfig, observed *AutoscalingSchedules) scheduleDiff {
	var d scheduleDiff
	if spec == nil {
		return d
	}
	if observed == nil {
		observed = &AutoscalingSchedules{}
	}

	if spec.Timezone != nil && *spec.Timezone != derefString(observed.Timezone) {
		d.changes = append(d.changes, fmt.Sprintf("scheduleTimezone %s→%s", derefString(observed.Timezone), *spec.Timezone))
	}

	deleted := map[string]bool{}
	remove := func(o AutoscalingSchedule, change string) {
		if deleted[o.ID] {
			return
		}
		deleted[o.ID] = true
		d.deletes = append(d.deletes, o.ID)
		d.changes = append(d.changes, change)
	}

	byName := make(map[string]AutoscalingSchedule, len(observed.Schedules))
	for _, o := range observed.Schedules {
		if _, dup := byName[o.Name]; dup {
			remove(o, fmt.Sprintf("schedule %s duplicate removed", o.Name))
			continue
		}
		byName[o.Name] = o
	}

	inSpec := make(map[string]bool, len(spec.Schedules))
	for _, s := range spec.Schedules {
		inSpec[s.Name] = true
		if expired(s) {
			continue
		}
		want := scheduleFromSpec(s)
		o, ok := byName[s.Name]
		switch {
		case !ok:
			d.upserts = append(d.upserts, want)
			d.changes = append(d.changes, fmt.Sprintf("schedule %s added", s.Name))
		case !reflect.DeepEqual(normalizeSchedule(want), normalizeSchedule(o)):
			want.ID = o.ID
			d.upserts = append(d.upserts, want)
			d.changes = append(d.changes, fmt.Sprintf("schedule %s changed", s.Name))
		}
	}

	for _, o := range observed.Schedules {
		if !inSpec[o.Name] {
			remove(o, fmt.Sprintf("schedule %s removed", o.Name))
		}
	}

	return d
}

func buildScheduleSettings(spec *modelsv1alpha1.AutoscalingScheduleConfig, observed *AutoscalingSchedules) map[string]interface{} {
	d := diffSchedules(spec, observed)
	if len(d.changes) == 0 {
		return nil
	}

	settings := map[string]interface{}{}
	if spec.Timezone != nil {
		settings["timezone"] = *spec.Timezone
	}
	if len(d.upserts) > 0 {
		schedules := make([]map[string]interface{}, 0, len(d.upserts))
		for _, s := range d.upserts {
			schedules = append(schedules, scheduleRequest(s))
		}
		settings["schedules"] = schedules
	}
	if len(d.deletes) > 0 {
		settings["delete_schedules"] = d.deletes
	}
	return settings
}

func scheduleFromSpec(s modelsv1alpha1.AutoscalingSchedule) AutoscalingSchedule {
	enabled := true
	if s.Enabled != nil {
		enabled = *s.Enabled
	}
	weekdays := make([]string, 0, len(s.Weekdays))
	for _, w := range s.Weekdays {
		weekdays = append(weekdays, string(w))
	}
	a := s.Autoscaling
	return AutoscalingSchedule{
		Name:        s.Name,
		Enabled:     enabled,
		Cadence:     s.Cadence,
		Weekdays:    weekdays,
		StartHour:   s.StartHour,
		StartMinute: s.StartMinute,
		EndHour:     s.EndHour,
		EndMinute:   s.EndMinute,
		StartAt:     s.StartAt,
		EndAt:       s.EndAt,
		AutoscalingSettings: ScheduleAutoscalingSettings{
			MinReplica:                  a.MinReplicas,
			MaxReplica:                  a.MaxReplicas,
			AutoscalingWindow:           a.AutoscalingWindow,
			ScaleDownDelay:              a.ScaleDownDelay,
			ConcurrencyTarget:           a.ConcurrencyTarget,
			TargetUtilizationPercentage: a.TargetUtilizationPercentage,
			TargetInFlightTokens:        a.TargetInFlightTokens,
			MaxScaleDownRate:            a.MaxScaleDownRate,
		},
	}
}

// normalizeSchedule strips fields that don't participate in comparison for the schedule's cadence.
func normalizeSchedule(s AutoscalingSchedule) AutoscalingSchedule {
	s.ID = ""
	if s.Cadence == CadenceOneTime {
		s.Weekdays, s.StartHour, s.StartMinute, s.EndHour, s.EndMinute = nil, nil, 0, nil, 0
		s.StartAt, s.EndAt = canonicalTime(s.StartAt), canonicalTime(s.EndAt)
		return s
	}
	s.StartAt, s.EndAt = "", ""
	if s.Cadence == "HOURLY" {
		s.StartHour, s.EndHour = nil, nil
	}
	if len(s.Weekdays) == 0 {
		s.Weekdays = nil
	} else {
		s.Weekdays = slices.Sorted(slices.Values(s.Weekdays))
	}
	return s
}

// expired reports a ONE_TIME schedule whose window has already ended; Baseten rejects writing those.
func expired(s modelsv1alpha1.AutoscalingSchedule) bool {
	if s.Cadence != CadenceOneTime {
		return false
	}
	end, err := time.Parse(time.RFC3339, s.EndAt)
	return err == nil && end.Before(time.Now())
}

// canonicalTime lets equivalent instants in different offsets compare equal, since the API may echo a different offset.
func canonicalTime(v string) string {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return v
	}
	return t.UTC().Format(time.RFC3339)
}

func scheduleRequest(s AutoscalingSchedule) map[string]interface{} {
	req := map[string]interface{}{
		"name":                 s.Name,
		"enabled":              s.Enabled,
		"cadence":              s.Cadence,
		"autoscaling_settings": s.AutoscalingSettings,
	}
	if s.ID != "" {
		req["id"] = s.ID
	}
	if s.Cadence == CadenceOneTime {
		req["start_at"] = s.StartAt
		req["end_at"] = s.EndAt
		return req
	}
	req["weekdays"] = s.Weekdays
	req["start_hour"] = s.StartHour
	req["start_minute"] = s.StartMinute
	req["end_hour"] = s.EndHour
	req["end_minute"] = s.EndMinute
	return req
}
