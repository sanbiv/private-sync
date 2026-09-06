package tui

import "testing"

func TestBackgroundChoice(t *testing.T) {
	tests := []struct {
		name             string
		env, termName    string
		interactive      bool
		wantDark, wantOK bool
	}{
		{"env dark wins", "dark", "xterm-256color", true, true, true},
		{"env light wins", "light", "xterm-256color", true, false, true},
		{"env light on a pipe", "light", "", false, false, true},
		{"unknown env value asks the terminal", "blue", "xterm-256color", true, false, false},
		{"empty env asks the terminal", "", "xterm-256color", true, false, false},
		{"dumb terminal defaults to dark", "", "dumb", true, true, true},
		{"empty TERM defaults to dark", "", "", true, true, true},
		{"non interactive defaults to dark", "", "xterm-256color", false, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dark, ok := backgroundChoice(tc.env, tc.termName, tc.interactive)
			if dark != tc.wantDark || ok != tc.wantOK {
				t.Errorf("backgroundChoice(%q, %q, %v) = (%v, %v), want (%v, %v)",
					tc.env, tc.termName, tc.interactive, dark, ok, tc.wantDark, tc.wantOK)
			}
		})
	}
}
