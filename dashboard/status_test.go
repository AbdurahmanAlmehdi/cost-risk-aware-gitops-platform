package main

// The dashboard makes one subtle correctness claim, and it is worth a test rather than a
// comment: a value the platform could not measure must reach the browser as *absent*,
// never as zero.
//
// This is not pedantry. A cost tile reading "$0.00" when the exporter has stopped says
// "this costs nothing", the single most misleading thing a cost dashboard can display,
// and precisely the failure M4's freshness metric exists to catch. The distinction lives
// in the JSON contract, so that is where it is tested.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnknownMetricIsNotZero(t *testing.T) {
	raw, err := json.Marshal(unknown(errNotCollected{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	if !strings.Contains(got, `"value":null`) {
		t.Errorf("an unmeasured metric must serialise its value as null, got %s", got)
	}
	// A zero anywhere in the value position would render as a real figure in the browser.
	if strings.Contains(got, `"value":0`) {
		t.Errorf("an unmeasured metric must never serialise as zero, got %s", got)
	}
	if !strings.Contains(got, `"error"`) {
		t.Errorf("an unmeasured metric must carry why it is unknown, got %s", got)
	}
}

func TestMeasuredZeroIsPreserved(t *testing.T) {
	// The converse matters too: a genuine zero, an empty queue, must survive as a
	// number, or a drained backlog would render as "unknown" and the load proof's most
	// important moment would disappear.
	raw, err := json.Marshal(measured(0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"value":0`) {
		t.Errorf("a measured zero must survive as zero, got %s", got)
	}
}

func TestMemoryLimitSelectsTheNamedContainer(t *testing.T) {
	// The drift patch addresses a container by index. If the lookup returned the wrong
	// index the patch would silently rewrite a different container's limit, and the test
	// would then wait for a revert that was never going to be about the field it changed.
	var d deployment
	d.Spec.Template.Spec.Containers = []struct {
		Name      string `json:"name"`
		Resources struct {
			Limits map[string]string `json:"limits"`
		} `json:"resources"`
	}{
		{Name: "sidecar"},
		{Name: "api"},
	}
	d.Spec.Template.Spec.Containers[1].Resources.Limits = map[string]string{"memory": "128Mi"}

	value, index, ok := d.memoryLimit("api")
	if !ok || value != "128Mi" || index != 1 {
		t.Errorf("memoryLimit(api) = (%q, %d, %v), want (\"128Mi\", 1, true)", value, index, ok)
	}

	if _, _, ok := d.memoryLimit("absent"); ok {
		t.Error("a container that is not present must not report a limit")
	}
}

// errNotCollected stands in for any reason a figure is missing. The dashboard does not
// care which one it was, only that it must not print a number.
type errNotCollected struct{}

func (errNotCollected) Error() string { return "no data" }
