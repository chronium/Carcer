package operator

import (
	"strings"
	"testing"

	"codexos/internal/experiment"
)

func TestEscapeTerminalTextMakesUntrustedControlsInert(t *testing.T) {
	hostile := "Capability λ\nFake status\r\x1b[2J\t\x00\u0085\u009b"
	if got, want := EscapeTerminalText(hostile, false), `Capability λ\nFake status\r\x1b[2J\t\x00\x85\x9b`; got != want {
		t.Fatalf("escaped text = %q, want %q", got, want)
	}
	if got, want := EscapeTerminalText(hostile, true), "Capability λ\nFake status\\r\\x1b[2J\\t\\x00\\x85\\x9b"; got != want {
		t.Fatalf("multiline escaped text = %q, want %q", got, want)
	}
	invalid := string([]byte{'a', 0xff, 'b'})
	if got, want := EscapeTerminalText(invalid, false), `a\xffb`; got != want {
		t.Fatalf("invalid UTF-8 escaped text = %q, want %q", got, want)
	}
}

func TestExitInterviewQuestionPreservesQuestionText(t *testing.T) {
	tests := []struct {
		line string
		want string
		ok   bool
	}{
		{"ask Why this design?", "Why this design?", true},
		{"  ask\t  Why  this design?\r\n", "Why  this design?", true},
		{"ask\u00a0Unicode separator", "Unicode separator", true},
		{"ask", "", false},
		{"ask   ", "", false},
		{"asker question", "", false},
		{"status ask question", "", false},
		{"", "", false},
	}
	for _, test := range tests {
		t.Run(test.line, func(t *testing.T) {
			got, ok := ExitInterviewQuestion(test.line)
			if got != test.want || ok != test.ok {
				t.Fatalf("ExitInterviewQuestion(%q) = (%q, %v), want (%q, %v)", test.line, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestAbortReasonPreservesTextAndEnforcesBounds(t *testing.T) {
	tests := []struct {
		line string
		want string
		ok   bool
	}{
		{"abort guest stopped after λ", "guest stopped after λ", true},
		{"  abort\t  preserve  spacing  ", "preserve  spacing  ", true},
		{"abort", "", false},
		{"abort   ", "", false},
		{"aborted reason", "", false},
	}
	for _, test := range tests {
		got, ok := AbortReason(test.line)
		if got != test.want || ok != test.ok {
			t.Fatalf("AbortReason(%q) = (%q, %t), want (%q, %t)", test.line, got, ok, test.want, test.ok)
		}
	}
	if err := experiment.ValidateAbortReason(strings.Repeat("x", experiment.MaxAbortReasonBytes)); err != nil {
		t.Fatalf("largest valid reason: %v", err)
	}
	for _, invalid := range []string{"", " \t", strings.Repeat("x", experiment.MaxAbortReasonBytes+1), string([]byte{0xff})} {
		if err := experiment.ValidateAbortReason(invalid); err == nil {
			t.Fatalf("invalid abort reason accepted: %q", invalid)
		}
	}
}
