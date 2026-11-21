package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteIssueRecordTSVKeepsOneLinePerRecord(t *testing.T) {
	seq := 1
	var buf bytes.Buffer
	writeIssueRecordTSV(&buf, []issueRecord{{
		Seq: 2, Kind: "run", Verdict: "pass", SupersedesSeq: &seq,
		Summary: "a\tb", Rationale: "line1\nline2",
		Evidence: []string{"ci-run/1", "job/2"},
		Fields:   map[string]any{"z": 1.0, "a": "x"},
	}})
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected header and one row, got %q", buf.String())
	}
	cells := strings.Split(lines[1], "\t")
	if len(cells) != 10 {
		t.Fatalf("expected 10 cells, got %d: %q", len(cells), lines[1])
	}
	if cells[3] != "1" || cells[4] != "a b" || cells[5] != "line1 line2" || cells[6] != "ci-run/1 job/2" || cells[7] != "a=x z=1" {
		t.Fatalf("unexpected cells: %q", cells)
	}
}

func TestBuildIssueRecordFieldsTypesAndRejectsDuplicates(t *testing.T) {
	fields, err := buildIssueRecordFields([]string{"gpus=8", "protocol=v1", "ok=true"})
	if err != nil {
		t.Fatal(err)
	}
	if string(fields["gpus"]) != "8" || string(fields["protocol"]) != `"v1"` || string(fields["ok"]) != "true" {
		t.Fatalf("unexpected fields: %v", fields)
	}
	if _, err := buildIssueRecordFields([]string{"a=1", "a=2"}); err == nil {
		t.Fatal("expected duplicate key error")
	}
	if _, err := buildIssueRecordFields([]string{"novalue"}); err == nil {
		t.Fatal("expected key=value error")
	}
}
