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

// initBackground pins the palette Lip Gloss uses for AdaptiveColor.
//
// It does not avoid the terminal query: Bubble Tea's own package init already
// calls lipgloss.HasDarkBackground() (bubbletea/tea_init.go), so the OSC 11
// query is issued before main runs, and its answer is cached on the default
// renderer. What this adds is an override for terminals whose answer is wrong
// or absent, and a definite value when stdio is not a terminal at all.
//
// A terminal that claims a normal TERM and then ignores the query costs
// termenv its full five second timeout, once, at process start — every
// command pays it, including ones that open no UI. termenv skips the query
// for TERM=dumb and for TERM starting with screen or tmux, so those are the
// only levers, and PRIVATE_SYNC_BACKGROUND is not one of them.
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
