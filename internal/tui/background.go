package tui

import (
	"os"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// BackgroundEnv pins the palette instead of asking the terminal: "dark" or
// "light". Useful on terminals that never answer the background query.
const BackgroundEnv = "PRIVATE_SYNC_BACKGROUND"

var backgroundOnce sync.Once

// initBackground resolves the terminal background colour once, before any
// tea.Program is started.
//
// Lip Gloss resolves AdaptiveColor lazily, on the first render. Inside a
// running Bubble Tea program that render happens while the program owns
// stdin, so the terminal's answer to the background query races the program's
// own input reader; a terminal that stays silent costs a five second stall
// before termenv falls back. Resolving it here, while stdin is still ours,
// keeps the first frame immediate.
func initBackground() {
	backgroundOnce.Do(func() {
		dark, decided := backgroundChoice(os.Getenv(BackgroundEnv), os.Getenv("TERM"), interactiveStdio())
		if !decided {
			// Queries the terminal, then caches the answer on the renderer.
			dark = lipgloss.HasDarkBackground()
		}
		lipgloss.SetHasDarkBackground(dark)
	})
}

// backgroundChoice decides the background without touching the terminal.
// decided is false when the terminal has to be asked.
func backgroundChoice(env, termName string, interactive bool) (dark, decided bool) {
	switch env {
	case "dark":
		return true, true
	case "light":
		return false, true
	}
	if !interactive || termName == "" || termName == "dumb" {
		return true, true // dark is the common default
	}
	return false, false
}

// interactiveStdio reports whether both ends of the terminal are usable.
func interactiveStdio() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
}
