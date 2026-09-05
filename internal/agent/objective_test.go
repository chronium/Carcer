package agent

import (
	"strings"
	"testing"
)

// Check the effective turn inputs captured by the fake app-server, not just
// the shared constant: both roles must receive the operator's full objective.
func assertDoomMilestoneInstructions(t *testing.T, prompt string) {
	t.Helper()
	for _, requirement := range []string{
		"Operator objective clarification (2026-09-05)",
		"A source-built Doom port, including DoomGeneric, can satisfy the Doom milestone",
		"Running the exact supplied DOS executable is not required",
		"its availability does not create a DOS/4G compatibility requirement",
		"without requiring any particular port or implementation approach",
		"Supplied original assets, including the supplied Doom executable and data, must remain immutable",
		"Guest-authored adaptations and build outputs are separate artifacts",
		"ordinary user workload launched through generic userland mechanisms",
		"no Doom-specific behavior or special scheduling treatment",
		"The same generic mechanisms must be capable of running unrelated programs",
		"must not prevent another runnable user workload from making progress",
		"Doom must run concurrently with an unrelated user workload that continues making progress without depending on Doom voluntarily yielding",
		"It supersedes contrary interpretations in inherited handoffs, guest notes, review requests or proposals, and older objective wording",
		"This clarification grants no additional trusted-environment capabilities",
	} {
		if !strings.Contains(prompt, requirement) {
			t.Errorf("effective instructions omit %q", requirement)
		}
	}
}
