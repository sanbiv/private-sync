package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/vault"
)

// statusCommand: per file sync state, no changes.
func (c *cli) statusCommand() *cobra.Command {
	var fetch bool
	cmd := &cobra.Command{
		Use:   "status [project...]",
		Short: "Show the sync state of every tracked file (no changes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := c.openSession(ctx)
			if err != nil {
				return err
			}
			defer s.Close()
			ids, err := resolveProjects(s, args)
			if err != nil {
				return err
			}
			if fetch {
				if err := c.fetch(ctx, s); err != nil {
					return err
				}
			}
			opts := c.syncOptions(sync.ModeSync, ids)
			plan, err := s.Engine.Plan(ctx, opts)
			if err != nil {
				return err
			}
			return c.printStatus(plan)
		},
	}
	cmd.Flags().BoolVar(&fetch, "fetch", false, "fetch the remote first")
	return cmd
}

// printStatus renders a plan without applying it. An unreadable project is
// reported and fails the command: its files are not being synchronised.
func (c *cli) printStatus(p *sync.Plan) error {
	if c.g.json {
		if err := c.printJSON(struct {
			Projects []projectView `json:"projects"`
			Warnings []string      `json:"warnings,omitempty"`
		}{Projects: projectViews(p, nil, nil), Warnings: p.Warnings}); err != nil {
			return err
		}
		if n := unreadableProjects(p); n > 0 {
			return fmt.Errorf("%d project(s) could not be read: %s", n, unreadableHint)
		}
		return nil
	}
	for pi := range p.Projects {
		pp := &p.Projects[pi]
		fmt.Fprintln(c.out, c.projectHeader(pp)+"  "+c.paint(colorDim, summaryText(summarize(pp))))
		for i := range pp.Items {
			it := &pp.Items[i]
			fmt.Fprintln(c.out, "  "+c.statusLine(it))
		}
		for _, w := range pp.Warnings {
			fmt.Fprintln(c.out, "  "+c.paint(colorYellow, "warning: "+w))
		}
	}
	if len(p.Projects) == 0 {
		fmt.Fprintln(c.out, "no linked projects (use `private-sync add <path>` or `projects link`)")
	}
	for _, w := range p.Warnings {
		c.warn(w)
	}
	if n := unreadableProjects(p); n > 0 {
		return fmt.Errorf("%d project(s) could not be read: %s", n, unreadableHint)
	}
	return nil
}

// statusLine renders an item with its reason (nothing applied).
func (c *cli) statusLine(it *sync.Item) string {
	glyph, color, _ := describe(it, sync.ModeSync, nil, false)
	line := c.paint(color, glyph) + " " + it.Key.Path
	if it.Reason != "" {
		line += "  " + c.paint(colorDim, it.Reason)
	}
	return line
}

// syncCommand builds sync, push and pull.
func (c *cli) syncCommand(name string, mode sync.Mode, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.runSyncFlow(cmd.Context(), mode, args)
		},
	}
}

// syncOptions builds the engine options from the global flags.
func (c *cli) syncOptions(mode sync.Mode, ids []string) sync.Options {
	return sync.Options{
		Mode:             mode,
		Projects:         ids,
		Strategy:         c.strategy,
		PropagateDeletes: c.g.delete,
		AcceptRollback:   c.g.acceptRollback,
		Progress:         c.progress,
	}
}

// runSyncFlow is open → fetch → plan → strategy → resolver → apply → push → report.
func (c *cli) runSyncFlow(ctx context.Context, mode sync.Mode, refs []string) error {
	s, err := c.openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	ids, err := resolveProjects(s, refs)
	if err != nil {
		return err
	}
	if err := c.fetch(ctx, s); err != nil {
		return err
	}
	return c.syncWith(ctx, s, c.syncOptions(mode, ids), nil)
}

// syncWith plans and applies opts on an open session. review, when set, sees
// the plan before anything is applied and may refuse (restore's confirmation).
func (c *cli) syncWith(ctx context.Context, s *app.Session, opts sync.Options, review func(*sync.Plan) error) error {
	plan, err := s.Engine.Plan(ctx, opts)
	if err != nil {
		return err
	}
	for _, w := range plan.Warnings {
		c.warn(w)
	}
	if review != nil {
		if err := review(plan); err != nil {
			return err
		}
	}
	res := sync.Resolutions{}
	if opts.PropagateDeletes {
		// --delete is the explicit request (spec §13: the only way to
		// propagate a local deletion non-interactively), so its deletions are
		// confirmed before the strategy runs: ApplyStrategy never replaces a
		// resolution, whereas "abort" (the --yes default) would skip them.
		for _, it := range plan.Items() {
			if it.Action == sync.ActionDeleteRemote && it.NeedsResolution {
				res[it.Key] = sync.Resolution{Kind: sync.ChooseConfirm}
			}
		}
	}
	sync.ApplyStrategy(plan, opts.Strategy, res)
	if len(plan.Unresolved(res)) > 0 && c.isInteractive() {
		if c.fe == nil {
			return errors.New("conflicts need a resolution and no front end is available")
		}
		r, aborted, err := c.fe.ResolveConflicts(ctx, s, plan)
		if err != nil {
			return err
		}
		if aborted {
			return fmt.Errorf("%w: conflict resolution cancelled, nothing applied", errAborted)
		}
		for k, v := range r {
			res[k] = v
		}
	}
	rep, applyErr := s.Engine.Apply(ctx, plan, res, opts)
	if applyErr != nil {
		if rep != nil {
			_ = c.printReport(plan, res, rep)
		}
		return applyErr
	}
	pushErr := c.push(ctx, s)
	if err := c.printReport(plan, res, rep); err != nil {
		return err
	}
	if pushErr != nil {
		return pushErr
	}
	// Per item failures are printed by the report but must also fail the
	// command: a cron or CI wrapper only sees the exit code.
	if rep != nil && len(rep.Errors) > 0 {
		return fmt.Errorf("%d item(s) failed", len(rep.Errors))
	}
	if n := unreadableProjects(plan); n > 0 {
		return fmt.Errorf("%d project(s) could not be read: %s", n, unreadableHint)
	}
	if n := len(unresolvedKeys(plan, res, rep)); n > 0 {
		return fmt.Errorf("%w: %d (run again in a terminal, or pass --strategy local|remote)", errUnresolved, n)
	}
	return nil
}

// unreadableHint explains what an undecryptable journal means.
const unreadableHint = "their journal could not be decrypted, so nothing was planned for them (restore the vault copy, or run `private-sync status` for the file names)"

// unreadableProjects counts the projects whose journal could not be decrypted.
// Plan drops all their items (spec §5: the plan is aborted for that project),
// so an empty report for them means "not synchronised", never "up to date".
func unreadableProjects(p *sync.Plan) int {
	if p == nil {
		return 0
	}
	n := 0
	for i := range p.Projects {
		if p.Projects[i].Unreadable {
			n++
		}
	}
	return n
}

// restoreCommand: vault head → local for every tracked file of a project.
func (c *cli) restoreCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "restore <project> [--path <dir>]",
		Short: "Write the vault version of every tracked file of a project",
		Long: `restore writes the vault version of every tracked file of a project into its
local directory (existing local copies go to the encrypted trash). With --path
an unlinked vault project is linked to that directory first.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := c.openSession(ctx)
			if err != nil {
				return err
			}
			defer s.Close()
			if err := c.fetch(ctx, s); err != nil {
				return err
			}
			var id, name string
			if strings.TrimSpace(dir) != "" {
				abs, err := absDir(dir)
				if err != nil {
					return err
				}
				vp, err := findVaultProject(s, args[0])
				if err != nil {
					return err
				}
				if err := c.linkProject(ctx, s, vp, abs); err != nil {
					return err
				}
				id, name = vp.ID, vp.Name
				c.linked = &linkView{ID: id, Name: name, Path: display(abs)}
				if !c.g.json { // --json: stdout stays one document (the report carries "linked")
					fmt.Fprintf(c.out, "linked %s (%s) to %s\n", name, id, display(abs))
				}
			} else {
				p, err := s.ResolveProject(args[0])
				if err != nil {
					return err
				}
				id, name = p.ID, p.Name
			}
			opts := c.syncOptions(sync.ModeRestore, []string{id})
			review := func(p *sync.Plan) error {
				n := 0
				for _, it := range p.Items() {
					if it.Action == sync.ActionDownload {
						n++
					}
				}
				if n == 0 {
					return nil
				}
				label := name
				if label == "" {
					label = id
				}
				ok, err := c.confirm(ctx, fmt.Sprintf("restore %d file(s) of %s from the vault (local copies go to the trash)?", n, label), true)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("%w: restore cancelled", errAborted)
				}
				return nil
			}
			return c.syncWith(ctx, s, opts, review)
		},
	}
	cmd.Flags().StringVar(&dir, "path", "", "link the vault project to this directory first")
	return cmd
}

// findVaultProject resolves a vault project (linked or not) by id or name.
func findVaultProject(s *app.Session, ref string) (*vault.Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, usagef("empty project id or name")
	}
	projects, warnings, err := s.VaultProjects()
	for _, w := range warnings {
		if s.Warn != nil {
			s.Warn(w)
		}
	}
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].ID == ref {
			return &projects[i], nil
		}
	}
	var byName []*vault.Project
	for i := range projects {
		if strings.EqualFold(projects[i].Name, ref) {
			byName = append(byName, &projects[i])
		}
	}
	switch len(byName) {
	case 0:
		return nil, fmt.Errorf("%w: %q", vault.ErrNoProject, ref)
	case 1:
		return byName[0], nil
	}
	ids := make([]string, 0, len(byName))
	for _, p := range byName {
		ids = append(ids, p.ID)
	}
	return nil, fmt.Errorf("several vault projects are named %q: use an id (%s)", ref, strings.Join(ids, ", "))
}
