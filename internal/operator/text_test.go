package operator

import "testing"

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
