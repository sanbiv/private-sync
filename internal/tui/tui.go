// Package tui is the Bubble Tea front end (spec §2.2).
package tui

import (
	"context"
	"errors"

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

// RunSetup runs the standalone setup wizard and returns the config to save.
func RunSetup(ctx context.Context, dirs paths.Dirs, existing *config.Config) (*config.Config, error) {
	return nil, errors.New("tui.RunSetup: not implemented")
}

// Run launches the dashboard program on an opened session.
func Run(ctx context.Context, s *app.Session) error { return errors.New("tui.Run: not implemented") }

// RunAddProject runs the add-project wizard as its own program.
func RunAddProject(ctx context.Context, s *app.Session, dir string) error {
	return errors.New("tui.RunAddProject: not implemented")
}

// ResolveConflicts runs the conflict resolver for a plan; aborted is true when the user aborted.
func ResolveConflicts(ctx context.Context, s *app.Session, p *sync.Plan) (res sync.Resolutions, aborted bool, err error) {
	return nil, false, errors.New("tui.ResolveConflicts: not implemented")
}
