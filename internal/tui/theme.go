package tui

import "github.com/charmbracelet/lipgloss"

// theme centralises the styling used across every screen (spec §2.2).
type theme struct {
	Title      lipgloss.Style
	Subtitle   lipgloss.Style
	Muted      lipgloss.Style
	Help       lipgloss.Style
	Error      lipgloss.Style
	Warning    lipgloss.Style
	Success    lipgloss.Style
	Selected   lipgloss.Style
	Normal     lipgloss.Style
	Box        lipgloss.Style
	DiffAdd    lipgloss.Style
	DiffDel    lipgloss.Style
	DiffCtx    lipgloss.Style
	BadgeGood  lipgloss.Style
	BadgeInfo  lipgloss.Style
	BadgeWarn  lipgloss.Style
	BadgeBad   lipgloss.Style
	BadgeMuted lipgloss.Style
	Spinner    lipgloss.Style
}

// styles is the single shared theme instance used by every screen.
var styles = newTheme()

func newTheme() theme {
	return theme{
		Title: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#C4B5FD"}),
		Subtitle: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#374151", Dark: "#D1D5DB"}),
		Muted: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#6B7280"}),
		Help: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#6B7280"}),
		Error: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}),
		Warning: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}),
		Success: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}),
		Selected: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#FFFFFF"}).
			Background(lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#7C3AED"}),
		Normal: lipgloss.NewStyle(),
		Box: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"}).
			Padding(0, 1),
		DiffAdd: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}),
		DiffDel: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}),
		DiffCtx: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#374151", Dark: "#D1D5DB"}),
		BadgeGood: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#052E16"}).
			Background(lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}).
			Padding(0, 1),
		BadgeInfo: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#082F49"}).
			Background(lipgloss.AdaptiveColor{Light: "#2563EB", Dark: "#60A5FA"}).
			Padding(0, 1),
		BadgeWarn: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#451A03"}).
			Background(lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}).
			Padding(0, 1),
		BadgeBad: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#450A0A"}).
			Background(lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}).
			Padding(0, 1),
		BadgeMuted: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#374151", Dark: "#D1D5DB"}).
			Background(lipgloss.AdaptiveColor{Light: "#E5E7EB", Dark: "#374151"}).
			Padding(0, 1),
		Spinner: lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#C4B5FD"}),
	}
}
