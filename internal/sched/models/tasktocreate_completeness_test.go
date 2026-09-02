package models

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTaskToCreateCarriesEveryDefinitionField is the drift guard for issue #565:
// TaskToCreate is the single canonical Task→TaskCreate clone used by BOTH the
// recurrence path (scheduleNextRecurrence) and re-run/clone (#270). It is a
// hand-maintained struct literal, so a new per-task definition field added to
// TaskCreate is easy to forget here — and a forgotten field silently resets to
// its Go zero value on every recurrence occurrence #2+ (and on every re-run).
//
// This test fills a Task with an all-non-zero definition, runs it through
// TaskToCreate, and asserts every TaskCreate field is carried — except the
// handful that are deliberately server-set / per-spawn / runtime-latched. Adding
// a TaskCreate field without carrying it in TaskToCreate fails this test.
func TestTaskToCreateCarriesEveryDefinitionField(t *testing.T) {
	// Fields TaskToCreate must NOT carry: they are populated server-side per
	// spawn (lineage) or latched at runtime, so a clone/recurrence starts clean.
	notCarried := map[string]string{
		"CreatedByTaskID":       "per-spawn lineage, set by the create_task tool (#277)",
		"SLABreached":           "runtime-latched by the SLA monitor (#274)",
		"ActualDurationSeconds": "runtime, populated on the terminal transition (#274)",
		"A2ADelegationDepth":    "delegation provenance (#1368): stamped by the inbound A2A server from the peer's header; a clone/re-run/recurrence is operator work, not a hop in a peer chain",
	}

	var task Task
	fillNonZero(reflect.ValueOf(&task).Elem())

	tc := TaskToCreate(&task)

	tcv := reflect.ValueOf(tc)
	tct := tcv.Type()
	for i := 0; i < tct.NumField(); i++ {
		name := tct.Field(i).Name
		if _, ok := notCarried[name]; ok {
			if !tcv.Field(i).IsZero() {
				t.Errorf("TaskToCreate carried %q but it is server-set/runtime-only and must NOT be cloned (%s)", name, notCarried[name])
			}
			continue
		}
		if tcv.Field(i).IsZero() {
			t.Errorf("TaskToCreate did not carry definition field %q — a recurring task would silently lose it on occurrence #2+, and a re-run/clone would drop it too (issue #565). Add it to TaskToCreate.", name)
		}
	}
}

// fillNonZero recursively sets every settable field of v to a distinctive
// non-zero value so IsZero() on the result reliably detects a dropped field.
func fillNonZero(v reflect.Value) {
	switch v.Type() {
	case reflect.TypeOf(time.Time{}):
		v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
		return
	case reflect.TypeOf(uuid.UUID{}):
		v.Set(reflect.ValueOf(uuid.New()))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("nonzero")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(v.Elem())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZero(k)
		val := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(val)
		m.SetMapIndex(k, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				fillNonZero(f)
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fillNonZero(v.Index(i))
		}
	default:
		// interface / chan / func — leave zero; no TaskCreate definition field
		// uses these kinds.
	}
}
