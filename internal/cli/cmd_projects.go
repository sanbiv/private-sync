package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/scan"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/vault"
)

// scanCommand: dry run over a directory, config only (no vault, no key).
func (c *cli) scanCommand() *cobra.Command {
	var showLow bool
	cmd := &cobra.Command{
		Use:   "scan <path>",
		Short: "Print the candidate files of a directory with score and reason (no changes)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := absDir(args[0])
			if err != nil {
				return err
			}
			cfg, _, err := c.loadConfig()
			if err != nil {
				return err
			}
			opts, err := scanOptions(cfg)
			if err != nil {
				return err
			}
			res, err := scan.Scan(cmd.Context(), dir, opts, c.runner)
			if err != nil {
				return err
			}
			return c.printScan(dir, res, showLow)
		},
	}
	cmd.Flags().BoolVar(&showLow, "all", false, "also list low-score candidates (committed to git)")
	return cmd
}

// scanOptions builds scanner options from the config alone: include/exclude
// lists, size limit and the hard excludes (key file, vault directory), like
// app.Session.ScanOptions but without a vault (no tracked set).
func scanOptions(cfg *config.Config) (scan.Options, error) {
	opts := scan.Options{
		Include:      append([]string(nil), cfg.Scan.Include...),
		ExcludeDirs:  append([]string(nil), cfg.Scan.ExcludeDirs...),
		ExcludeFiles: append([]string(nil), cfg.Scan.ExcludeFiles...),
	}
	if len(opts.Include) == 0 {
		opts.Include = scan.DefaultInclude()
	}
	if len(opts.ExcludeDirs) == 0 {
		opts.ExcludeDirs = scan.DefaultExcludeDirs()
	}
	if len(opts.ExcludeFiles) == 0 {
		opts.ExcludeFiles = scan.DefaultExcludeFiles()
	}
	maxSize, err := cfg.MaxFileSize()
	if err != nil {
		return scan.Options{}, fmt.Errorf("scan.max_file_size: %w", err)
	}
	if maxSize <= 0 {
		maxSize = scan.DefaultMaxFileSize
	}
	opts.MaxFileSize = maxSize

	var hard []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		for _, h := range hard {
			if h == p {
				return
			}
		}
		hard = append(hard, p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			resolved = filepath.Clean(resolved)
			for _, h := range hard {
				if h == resolved {
					return
				}
			}
			hard = append(hard, resolved)
		}
	}
	keyPath, err := cfg.KeyFilePath()
	if err != nil {
		return scan.Options{}, fmt.Errorf("key file path: %w", err)
	}
	add(keyPath)
	vaultDir, err := cfg.VaultPath()
	if err != nil {
		return scan.Options{}, fmt.Errorf("vault path: %w", err)
	}
	add(vaultDir)
	opts.HardExclude = hard
	return opts, nil
}

// candidateView is the JSON shape of a scan candidate.
type candidateView struct {
	Path        string   `json:"path"`
	Size        int64    `json:"size"`
	Score       string   `json:"score"`
	Reasons     []string `json:"reasons"`
	GitTracked  bool     `json:"git_tracked"`
	GitIgnored  bool     `json:"git_ignored"`
	SecretName  bool     `json:"secret_name"`
	Tracked     bool     `json:"tracked"`
	Preselected bool     `json:"preselected"`
}

// printScan renders a scan result as a table or JSON.
func (c *cli) printScan(dir string, res *scan.Result, showLow bool) error {
	if c.g.json {
		views := make([]candidateView, 0, len(res.Candidates))
		for _, cand := range res.Candidates {
			views = append(views, candidateView{
				Path: cand.Path, Size: cand.Size, Score: cand.Score.String(),
				Reasons: nonNil(cand.Reasons), GitTracked: cand.GitTracked, GitIgnored: cand.GitIgnored,
				SecretName: cand.SecretName, Tracked: cand.Tracked, Preselected: cand.Preselected,
			})
		}
		return c.printJSON(struct {
			Dir         string          `json:"dir"`
			Candidates  []candidateView `json:"candidates"`
			NestedRepos []string        `json:"nested_repos"`
			GitInfo     bool            `json:"git_info"`
			Truncated   bool            `json:"truncated"`
			Warnings    []string        `json:"warnings"`
		}{dir, views, nonNil(res.NestedRepos), res.GitInfo, res.Truncated, nonNil(res.Warnings)})
	}
	rows := [][]string{{"SCORE", "SIZE", "PATH", "REASONS"}}
	hidden := 0
	for _, cand := range res.Candidates {
		if cand.Score == scan.ScoreLow && !showLow && !cand.Tracked {
			hidden++
			continue
		}
		score := cand.Score.String()
		switch cand.Score {
		case scan.ScoreHigh:
			score = c.paint(colorGreen, score)
		case scan.ScoreLow:
			score = c.paint(colorDim, score)
		}
		rows = append(rows, []string{score, formatSize(cand.Size), cand.Path, strings.Join(cand.Reasons, ", ")})
	}
	if len(rows) == 1 {
		fmt.Fprintf(c.out, "no candidates in %s\n", display(dir))
	} else {
		c.table(rows)
	}
	if hidden > 0 {
		fmt.Fprintf(c.out, "%d low-score candidate(s) committed to git hidden (--all shows them)\n", hidden)
	}
	if !res.GitInfo {
		fmt.Fprintln(c.out, "no git information: every match scored medium (secret-like names high)")
	}
	for _, r := range res.NestedRepos {
		fmt.Fprintf(c.out, "nested repository %s not scanned: add it as its own project\n", r)
	}
	if res.Truncated {
		c.warn("scan truncated: too many files or candidates")
	}
	for _, w := range res.Warnings {
		c.warn(w)
	}
	return nil
}

// nonNil turns a nil slice into an empty one (stable JSON).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// projectsCommand groups list | link | unlink.
func (c *cli) projectsCommand() *cobra.Command {
	cmd := groupCommand("projects", "List, link and unlink projects")
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List linked projects with their state and unlinked vault projects",
			Args:  noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				s, err := c.openSession(cmd.Context())
				if err != nil {
					return err
				}
				defer s.Close()
				return c.listProjects(cmd, s)
			},
		},
		&cobra.Command{
			Use:   "link <id|name> <path>",
			Short: "Map a vault project to a local directory (then pull restores it)",
			Args:  exactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				dir, err := absDir(args[1])
				if err != nil {
					return err
				}
				s, err := c.openSession(ctx)
				if err != nil {
					return err
				}
				defer s.Close()
				vp, err := findVaultProject(s, args[0])
				if err != nil {
					return err
				}
				if err := c.linkProject(ctx, s, vp, dir); err != nil {
					return err
				}
				if c.g.json {
					return c.printJSON(linkView{ID: vp.ID, Name: vp.Name, Path: display(dir)})
				}
				fmt.Fprintf(c.out, "linked %s (%s) to %s\n", vp.Name, vp.ID, display(dir))
				fmt.Fprintf(c.out, "run `private-sync pull %s` to restore its files\n", vp.ID)
				return nil
			},
		},
		&cobra.Command{
			Use:   "unlink <id|name>",
			Short: "Remove the mapping on this machine (vault untouched, no key needed)",
			Args:  exactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				return c.unlinkProject(args[0])
			},
		},
	)
	return cmd
}

// linkProject maps the vault project vp to dir (projects link, restore
// --path): it warns when the project was already linked to another directory
// on this machine (the mapping is replaced) and when dir strongly matches a
// different vault project, then records the mapping.
func (c *cli) linkProject(ctx context.Context, s *app.Session, vp *vault.Project, dir string) error {
	if existing, ok := s.Config.Project(vp.ID); ok && !samePath(existing.Path, dir) {
		c.warn(fmt.Sprintf("%s was linked to %s; replacing the mapping", vp.ID, existing.Path))
	}
	fps, matches, err := s.Identify(ctx, dir)
	if err != nil {
		return err
	}
	for _, m := range matches {
		if m.Strength == identity.StrengthStrong && m.ProjectID != vp.ID {
			c.warn(fmt.Sprintf("%s also matches vault project %s (%s)", display(dir), m.ProjectID, fingerprintList(m.Shared)))
		}
	}
	return s.LinkProject(ctx, vp.ID, vp.Name, dir, fps)
}

// samePath reports whether a configured project path (possibly ~-contracted)
// names the absolute directory dir.
func samePath(configured, dir string) bool {
	exp, err := paths.ExpandHome(configured)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(exp)
	if err != nil {
		return false
	}
	return filepath.Clean(abs) == filepath.Clean(dir)
}

// fingerprintList joins fingerprints for display.
func fingerprintList(fps []identity.Fingerprint) string {
	parts := make([]string, 0, len(fps))
	for _, f := range fps {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, " ")
}

// linkedView / unlinkedView are the JSON shapes of projects list.
type linkedView struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Path  string      `json:"path"`
	Badge string      `json:"badge"`
	State summaryView `json:"state"`
}

type unlinkedView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Fingerprints []string `json:"fingerprints"`
	Machines     []string `json:"machines"`
}

// listProjects prints linked projects (with badges from a local plan) then
// the unlinked vault projects.
func (c *cli) listProjects(cmd *cobra.Command, s *app.Session) error {
	plan, err := s.Engine.Plan(cmd.Context(), c.syncOptions(sync.ModeSync, nil))
	if err != nil {
		return err
	}
	for _, w := range plan.Warnings {
		c.warn(w)
	}
	byID := make(map[string]*sync.ProjectPlan, len(plan.Projects))
	for i := range plan.Projects {
		byID[plan.Projects[i].ID] = &plan.Projects[i]
	}
	unlinked, err := s.UnlinkedProjects()
	if err != nil {
		return err
	}

	if c.g.json {
		out := struct {
			Linked   []linkedView   `json:"linked"`
			Unlinked []unlinkedView `json:"unlinked"`
		}{Linked: []linkedView{}, Unlinked: []unlinkedView{}}
		for _, p := range s.Config.Projects {
			lv := linkedView{ID: p.ID, Name: p.Name, Path: p.Path}
			if pp := byID[p.ID]; pp != nil {
				lv.Badge = badge(pp)
				lv.State = toSummaryView(summarize(pp))
				if lv.Name == "" {
					lv.Name = pp.Name
				}
			}
			out.Linked = append(out.Linked, lv)
		}
		for _, vp := range unlinked {
			fps := make([]string, 0, len(vp.Fingerprints))
			for _, f := range vp.Fingerprints {
				fps = append(fps, f.String())
			}
			out.Unlinked = append(out.Unlinked, unlinkedView{ID: vp.ID, Name: vp.Name, Fingerprints: fps, Machines: nonNil(vp.Machines)})
		}
		return c.printJSON(out)
	}

	if len(s.Config.Projects) == 0 {
		fmt.Fprintln(c.out, "no linked projects on this machine")
	} else {
		rows := [][]string{{"ID", "NAME", "PATH", "STATE"}}
		for _, p := range s.Config.Projects {
			name, b := p.Name, ""
			if pp := byID[p.ID]; pp != nil {
				b = badge(pp)
				if name == "" {
					name = pp.Name
				}
			}
			rows = append(rows, []string{p.ID, name, p.Path, b})
		}
		c.table(rows)
	}
	if len(unlinked) > 0 {
		fmt.Fprintln(c.out)
		fmt.Fprintln(c.out, "not linked on this machine (projects link <id> <path>, then pull):")
		rows := [][]string{{"ID", "NAME", "FINGERPRINTS"}}
		for _, vp := range unlinked {
			rows = append(rows, []string{vp.ID, vp.Name, fingerprintList(vp.Fingerprints)})
		}
		c.table(rows)
	}
	return nil
}

// unlinkProject removes a mapping using the config only (no key): the base
// entries are dropped too when the vault id is known without a passphrase.
func (c *cli) unlinkProject(ref string) error {
	a, err := c.loadApp()
	if err != nil {
		if errors.Is(err, app.ErrNoConfig) {
			return errNoConfig
		}
		return err
	}
	ref = strings.TrimSpace(ref)
	p, ok := a.Config.Project(ref)
	if !ok {
		return fmt.Errorf("%w: %q", app.ErrNotLinked, ref)
	}
	id, name, path := p.ID, p.Name, p.Path
	a.Config.RemoveProject(id)
	if err := a.SaveConfig(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if vaultID := stateDirVaultID(a.Dirs, a.Config); vaultID != "" {
		st, err := state.Open(a.Dirs.State, vaultID)
		if err == nil {
			st.DeleteProject(id)
			err = st.Save()
		}
		if err != nil {
			c.warn(fmt.Sprintf("could not drop the sync state of %s: %v", id, err))
		}
	}
	if c.g.json {
		return c.printJSON(struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Path string `json:"path"`
		}{id, name, path})
	}
	fmt.Fprintf(c.out, "unlinked %s (%s) from %s; the vault is untouched\n", name, id, path)
	return nil
}
