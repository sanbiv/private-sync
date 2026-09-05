// Package tui is the Bubble Tea front end (spec §2.2).
package tui

import (
	"context"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/sync"
)

// Frontend implements cli.Frontend with the Bubble Tea screens.
type Frontend struct{}

// RunSetup runs the standalone setup wizard (Huh form) and returns the config to save.
// existing may be nil (first run). Nothing is written to disk here.
func (Frontend) RunSetup(ctx context.Context, dirs paths.Dirs, existing *config.Config) (*config.Config, error) {
	return RunSetup(ctx, dirs, existing)
}

// Run launches the dashboard program on an opened session.
func (Frontend) Run(ctx context.Context, s *app.Session) error { return Run(ctx, s) }

// RunAddProject runs the add-project wizard as its own program (used by `add`).
func (Frontend) RunAddProject(ctx context.Context, s *app.Session, dir string) error {
	return RunAddProject(ctx, s, dir)
}

// ResolveConflicts runs the conflict resolver for a plan.
func (Frontend) ResolveConflicts(ctx context.Context, s *app.Session, p *sync.Plan) (sync.Resolutions, bool, error) {
	return ResolveConflicts(ctx, s, p)
}
