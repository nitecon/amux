package taskplatform

import (
	"reflect"
	"testing"
)

// Ported from src/task_platform.rs::explicit_requirements_fail_closed_and_retagging_preserves_other_labels
// and task_store_client_assignment_and_notifier_agree_on_platform_eligibility.
func TestRequiredOS(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		want    string
		invalid bool
	}{
		{name: "missing labels"},
		{name: "ordinary Unicode labels", labels: []string{"ordinary", "é", "🚀"}},
		{name: "linux", labels: []string{"os:linux"}, want: "linux"},
		{name: "windows", labels: []string{"os:windows"}, want: "windows"},
		{name: "macos", labels: []string{"os:macos"}, want: "macos"},
		{name: "unknown", labels: []string{"os:plan9"}, invalid: true},
		{name: "value case", labels: []string{"os:Linux"}, invalid: true},
		{name: "prefix case", labels: []string{"OS:linux"}, invalid: true},
		{name: "conflicting", labels: []string{"os:linux", "os:windows"}, invalid: true},
		{name: "duplicate", labels: []string{"os:linux", "os:linux"}, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RequiredOS(tt.labels)
			if (err != nil) != tt.invalid || got != tt.want {
				t.Fatalf("RequiredOS(%q) = %q, %v; want %q, invalid=%v", tt.labels, got, err, tt.want, tt.invalid)
			}
			for _, host := range []string{"", "linux", "windows", "macos"} {
				want := !tt.invalid && (tt.want == "" || tt.want == host)
				if Matches(tt.labels, host) != want {
					t.Fatalf("Matches(%q,%q) != %v", tt.labels, host, want)
				}
			}
		})
	}
}

func TestLabelsWithOS(t *testing.T) {
	original := []string{"autoreview-finding", "os:windows", "project"}
	got, err := LabelsWithOS(original, "linux")
	if err != nil || !reflect.DeepEqual(got, []string{"autoreview-finding", "project", "os:linux"}) {
		t.Fatalf("retag: %q, %v", got, err)
	}
	if !reflect.DeepEqual(original, []string{"autoreview-finding", "os:windows", "project"}) {
		t.Fatalf("input mutated: %q", original)
	}
	got, err = LabelsWithOS(original, "")
	if err != nil || !reflect.DeepEqual(got, []string{"autoreview-finding", "project"}) {
		t.Fatalf("clear: %q, %v", got, err)
	}
	if _, err = LabelsWithOS(original, "plan9"); err == nil {
		t.Fatal("unsupported OS accepted")
	}
	got, err = LabelsWithOS([]string{"OS:Windows", "os:plan9", "keep"}, "macos")
	if err != nil || !reflect.DeepEqual(got, []string{"keep", "os:macos"}) {
		t.Fatalf("repair: %q, %v", got, err)
	}
}

// Ported from the malformed JSON label cases in src/task_platform.rs.
func TestJSONLabelsFailClosed(t *testing.T) {
	for _, input := range []string{
		`{"title":"WINDOWS ONLY"}`, `{"labels":null}`, `{"labels":[]}`,
	} {
		labels, err := ParseLabels([]byte(input))
		if err != nil || !Matches(labels, "") {
			t.Fatalf("unrestricted task %s: %q, %v", input, labels, err)
		}
	}
	for _, input := range []string{
		`{"labels":[12]}`, `{"labels":["os:linux",12]}`, `{"labels":"os:linux"}`,
		`{"labels":[null]}`, `{"labels":["os:linux","os:linux"]}`, `{"labels":["OS:linux"]}`,
		`{"labels":{}}`, `{"labels":["os:plan9"]}`, `{"labels":true}`, `[]`, `null`, `{`,
	} {
		if labels, err := ParseLabels([]byte(input)); err == nil {
			t.Fatalf("malformed task accepted: %s => %q", input, labels)
		}
	}
	labels, err := ParseLabels([]byte(`{"labels":["ordinary","os:linux"]}`))
	if err != nil || !Matches(labels, "linux") || Matches(labels, "windows") {
		t.Fatalf("linux task: %q, %v", labels, err)
	}
}
