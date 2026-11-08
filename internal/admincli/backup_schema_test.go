package admincli

import (
	"reflect"
	"testing"
)

func TestParseTOCSchemas(t *testing.T) {
	t.Parallel()

	toc := `;
; Archive created at 2026-11-07 10:00:00 UTC
;     dbname: fleet
;
7; 2615 16390 SCHEMA - chat fleet
215; 1259 16391 TABLE chat users fleet
216; 1259 16400 SEQUENCE chat users_id_seq fleet
3340; 0 16391 TABLE DATA chat users fleet
217; 1259 16410 TABLE chat conversations fleet
`
	if got := parseTOCSchemas(toc); !reflect.DeepEqual(got, []string{"chat"}) {
		t.Errorf("schemas = %v, want [chat]", got)
	}
	mixed := "1; 1259 1 TABLE public a o\n2; 1259 2 TABLE sched b o\n"
	if got := parseTOCSchemas(mixed); !reflect.DeepEqual(got, []string{"public", "sched"}) {
		t.Errorf("schemas = %v, want [public sched]", got)
	}
	// An empty schema's dump: its SCHEMA entry alone names it (#2055 review).
	if got := parseTOCSchemas("8; 2615 5450663 SCHEMA - sched fleet\n"); !reflect.DeepEqual(got, []string{"sched"}) {
		t.Errorf("empty-schema dump: schemas = %v, want [sched]", got)
	}
	// Nothing to establish a schema from: restore refuses (runPgRestore).
	if got := parseTOCSchemas(";\n; Archive created\n3; 0 0 COMMENT - EXTENSION x\n"); len(got) != 0 {
		t.Errorf("schemas = %v, want none", got)
	}
}
