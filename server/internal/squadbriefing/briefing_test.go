package squadbriefing

import (
	"strings"
	"testing"
)

// A current daemon's runtime brief states no built-in moves, so the protocol
// forbids none; only an older daemon's brief, which still states them, gets
// the override (MUL-8145).
func TestOwningLeaderInWorkflowOverridesBuiltInMovesOnlyForLegacyDaemons(t *testing.T) {
	current := strings.Join(strings.Fields(ProtocolFor(true, WorkflowCurrentDaemon)), " ")
	legacy := strings.Join(strings.Fields(ProtocolFor(true, WorkflowLegacyDaemon)), " ")
	for _, word := range []string{"in_progress", "in_review"} {
		if strings.Contains(current, word) {
			t.Errorf("current-daemon protocol names %q, which nothing in its brief asks for", word)
		}
	}
	if !strings.Contains(legacy, "This overrides the in_progress / in_review rules in your runtime instructions") {
		t.Errorf("legacy-daemon protocol must override the built-in moves:\n%s", legacy)
	}
	if !strings.Contains(legacy, strings.Join(strings.Fields(squadParentStatusOwnedInWorkflow), " ")) {
		t.Errorf("legacy-daemon protocol must keep the workflow responsibility it extends")
	}
}
