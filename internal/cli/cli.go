// Package cli implements the cobra commands (spec §2.1).
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/sync"
)

// Frontend is the interactive UI injected by main (implemented by package tui).
// Keeping it an interface lets cli and tui build and test independently.
type Frontend interface {
	RunSetup(ctx context.Context, dirs paths.Dirs, existing *config.Config) (*config.Config, error)
	Run(ctx context.Context, s *app.Session) error
	RunAddProject(ctx context.Context, s *app.Session, dir string) error
	ResolveConflicts(ctx context.Context, s *app.Session, p *sync.Plan) (res sync.Resolutions, aborted bool, err error)
}

// Main runs the CLI with the given arguments and returns the process exit code.
func Main(args []string, fe Frontend) int {
	fmt.Fprintln(os.Stderr, "private-sync: not implemented")
	return 1
}
