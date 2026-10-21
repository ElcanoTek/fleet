package httpapi

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// manyEmptyStrings is a JSON list of n empty strings: about 3 bytes each, so
// a 16 MiB body holds millions of them.
func manyEmptyStrings(n int) []byte {
	return []byte("[" + strings.TrimSuffix(strings.Repeat(`"",`, n), ",") + "]")
}

// allocatedBy reports the bytes f allocates.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A list past its bound is refused while it is read: four million empty ids
// (about 12 MiB, inside the group body cap) are refused after the 101st, with
// a few kilobytes allocated, not the hundreds of megabytes a materialized
// slice would take.
func TestBoundedDecoding_RefusesPastTheBoundWhileReading(t *testing.T) {
	raw := manyEmptyStrings(4_000_000)
	var ids approvalIDList
	var err error
	if n := allocatedBy(func() { err = ids.UnmarshalJSON(raw) }); n > 4<<20 {
		t.Fatalf("decoding a refused 4M-id list allocated %d bytes, want it bounded", n)
	}
	if !errors.Is(err, errTooManyGroupDecisions) {
		t.Fatalf("err = %v, want too many approvals", err)
	}

	var narrow approvalGroupNarrow
	narrowRaw := []byte(`{"ap-1":` + string(raw) + `}`)
	if n := allocatedBy(func() { err = narrow.UnmarshalJSON(narrowRaw) }); n > 4<<20 {
		t.Fatalf("decoding a refused narrow list allocated %d bytes", n)
	}
	if !errors.Is(err, errTooManyGroupNarrowedValues) {
		t.Fatalf("narrow err = %v, want too many narrowed values", err)
	}

	var values approvedValuesField
	if n := allocatedBy(func() { err = values.UnmarshalJSON(raw) }); n > 4<<20 {
		t.Fatalf("decoding refused approved_values allocated %d bytes", n)
	}
	if !errors.Is(err, errTooManyApprovedValues) {
		t.Fatalf("approved_values err = %v, want too many", err)
	}
}

// Every count is bounded: the narrow map's cards, its values summed across
// cards, and narrow_args' entries.
func TestBoundedDecoding_Counts(t *testing.T) {
	keys := func(n int, value string) string {
		var b strings.Builder
		b.WriteString("{")
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"ap-` + itoa(i) + `":` + value)
		}
		b.WriteString("}")
		return b.String()
	}
	var narrow approvalGroupNarrow
	if err := narrow.UnmarshalJSON([]byte(keys(maxApprovalGroupDecisions+1, `["a"]`))); !errors.Is(err, errTooManyGroupDecisions) {
		t.Fatalf("101 narrowed cards: %v", err)
	}
	if err := narrow.UnmarshalJSON([]byte(keys(maxApprovalGroupDecisions, `["a"]`))); err != nil || len(narrow) != maxApprovalGroupDecisions {
		t.Fatalf("100 narrowed cards: %v (%d)", err, len(narrow))
	}
	// 60 values on each of 100 cards is 6000, past the 5000-value bound.
	sixty := string(manyEmptyStrings(60))
	if err := narrow.UnmarshalJSON([]byte(keys(maxApprovalGroupDecisions, sixty))); !errors.Is(err, errTooManyGroupNarrowedValues) {
		t.Fatalf("6000 narrowed values: %v", err)
	}
	var args approvalGroupNarrowArgs
	if err := args.UnmarshalJSON([]byte(keys(maxApprovalGroupDecisions+1, `"deal_ids"`))); !errors.Is(err, errTooManyGroupDecisions) {
		t.Fatalf("101 narrow_args: %v", err)
	}
	if err := args.UnmarshalJSON([]byte(keys(2, `7`))); err == nil {
		t.Fatal("a non-string narrow_args value must be refused")
	}
}

// Within the bounds the types decode as plain JSON would, null included.
func TestBoundedDecoding_MatchesPlainJSON(t *testing.T) {
	var req approvalGroupRequest
	body := `{"approve":["ap-1","ap-2"],"decline":null,"narrow":{"ap-1":["k1","k2"],"ap-2":null},"narrow_args":{"ap-1":"deal_ids"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Approve) != 2 || req.Decline != nil || len(req.Narrow["ap-1"]) != 2 || req.Narrow["ap-2"] != nil || req.NarrowArgs["ap-1"] != "deal_ids" {
		t.Fatalf("decoded %+v", req)
	}
	if _, ok := req.Narrow["ap-2"]; !ok {
		t.Fatal("a null narrow entry must stay present (checkGroupNarrowing refuses it)")
	}
	for _, bad := range []string{`{"approve":[1]}`, `{"approve":{}}`, `{"narrow":[]}`, `{"narrow":{"a":[1]}}`, `{"approve":["a"`} {
		if err := json.Unmarshal([]byte(bad), &approvalGroupRequest{}); err == nil {
			t.Errorf("%s: want refused", bad)
		}
	}
	var values approvedValuesField
	if err := json.Unmarshal([]byte(`["a","b"]`), &values); err != nil || !values.present || len(values.values) != 2 {
		t.Fatalf("approved_values: %v %+v", err, values)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
