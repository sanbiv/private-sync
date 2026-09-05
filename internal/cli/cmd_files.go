package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/sync"
)

// filesCommand groups add | rm | delete.
func (c *cli) filesCommand() *cobra.Command {
	cmd := groupCommand("files", "Track, untrack or delete files of a project")
	var removeLocal bool
	del := &cobra.Command{
		Use:   "delete <project> <relpath...>",
		Short: "Delete files everywhere (other machines move their copy to the trash)",
		Args:  minArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.filesDelete(cmd, args[0], args[1:], removeLocal)
		},
	}
	del.Flags().BoolVar(&removeLocal, "local", false, "also remove the local copies (asked interactively when omitted; never implied by --yes)")
	cmd.AddCommand(
		&cobra.Command{
			Use:   "add <project> <relpath...>",
			Short: "Track more files of a project (upload)",
			Args:  minArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return c.filesAdd(cmd, args[0], args[1:])
			},
		},
		&cobra.Command{
			Use:   "rm <project> <relpath...>",
			Short: "Untrack files everywhere; local copies are kept",
			Args:  minArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return c.filesRm(cmd, args[0], args[1:])
			},
		},
		del,
	)
	return cmd
}

// projectFiles opens the session, resolves the project and normalises the
// paths (relative to the project directory, slash separated).
func (c *cli) projectFiles(cmd *cobra.Command, ref string, paths []string, mustExist bool) (*app.Session, *config.ProjectConfig, string, []string, error) {
	s, err := c.openSession(cmd.Context())
	if err != nil {
		return nil, nil, "", nil, err
	}
	p, err := s.ResolveProject(ref)
	if err != nil {
		s.Close()
		return nil, nil, "", nil, err
	}
	dir, err := s.Config.ProjectPath(p.ID)
	if err != nil {
		s.Close()
		return nil, nil, "", nil, err
	}
	rels, err := relPaths(dir, paths, mustExist)
	if err != nil {
		s.Close()
		return nil, nil, "", nil, err
	}
	pc := *p
	return s, &pc, dir, rels, nil
}

// relPaths turns user paths (relative to the project, or absolute inside it)
// into clean slash-separated relative paths.
func relPaths(dir string, in []string, mustExist bool) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			return nil, usagef("empty path")
		}
		rel := p
		if filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
			abs, err := absFile(p)
			if err != nil {
				return nil, err
			}
			r, err := filepath.Rel(dir, abs)
			if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s is outside the project directory %s", abs, display(dir))
			}
			rel = r
		}
		rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if !sync.ValidPath(rel) {
			return nil, fmt.Errorf("%w: %q", sync.ErrBadPath, raw)
		}
		if mustExist {
			st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", raw, err)
			}
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not a regular file", raw)
			}
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	return out, nil
}

// absFile expands ~ and makes p absolute (no existence check).
func absFile(p string) (string, error) {
	exp, err := paths.ExpandHome(p)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(exp)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// filesAdd tracks paths: a push plan with Track set, applied and pushed.
func (c *cli) filesAdd(cmd *cobra.Command, ref string, paths []string) error {
	ctx := cmd.Context()
	s, p, _, rels, err := c.projectFiles(cmd, ref, paths, true)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := c.fetch(ctx, s); err != nil {
		return err
	}
	opts := c.syncOptions(sync.ModePush, []string{p.ID})
	opts.Track = make(map[sync.ItemKey]bool, len(rels))
	for _, r := range rels {
		opts.Track[sync.ItemKey{Project: p.ID, Path: r}] = true
	}
	return c.syncWith(ctx, s, opts, nil)
}

// filesRm untracks paths everywhere; local copies are never touched.
func (c *cli) filesRm(cmd *cobra.Command, ref string, paths []string) error {
	ctx := cmd.Context()
	s, p, _, rels, err := c.projectFiles(cmd, ref, paths, false)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := c.fetch(ctx, s); err != nil {
		return err
	}
	if err := s.Engine.Untrack(ctx, p.ID, rels); err != nil {
		return err
	}
	if err := c.push(ctx, s); err != nil {
		return err
	}
	if c.g.json {
		return c.printJSON(struct {
			Project   string   `json:"project"`
			Untracked []string `json:"untracked"`
		}{p.ID, rels})
	}
	for _, r := range rels {
		fmt.Fprintf(c.out, "%s %s  %s\n", c.paint(colorDim, glyphUntrack), r, c.paint(colorDim, "untracked, local copy kept"))
	}
	fmt.Fprintf(c.out, "untracked %d file(s) of %s\n", len(rels), projectLabel(p))
	return nil
}

// filesDelete writes tombstones (delete everywhere) and optionally removes
// the local copies (pre-images go to the trash first).
func (c *cli) filesDelete(cmd *cobra.Command, ref string, paths []string, removeLocal bool) error {
	ctx := cmd.Context()
	s, p, dir, rels, err := c.projectFiles(cmd, ref, paths, false)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := c.fetch(ctx, s); err != nil {
		return err
	}
	ok, err := c.confirm(ctx, fmt.Sprintf("delete %d file(s) of %s from the vault on every machine (other machines move their copy to the trash)?", len(rels), projectLabel(p)), false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: nothing deleted", errAborted)
	}
	if err := s.Engine.DeleteEverywhere(ctx, p.ID, rels); err != nil {
		return err
	}
	if !removeLocal && c.isInteractive() {
		removeLocal, err = c.confirm(ctx, "also remove the local copies (they go to the encrypted trash first)?", false)
		if err != nil {
			return err
		}
	}
	var removed []string
	if removeLocal {
		for _, r := range rels {
			full := filepath.Join(dir, filepath.FromSlash(r))
			st, err := os.Lstat(full)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					c.warn(fmt.Sprintf("%s: %v", r, err))
				}
				continue
			}
			if st.Mode().IsRegular() {
				if content, err := os.ReadFile(full); err == nil {
					if _, err := s.State.TrashPut(s.Vault.Keys(), p.ID, r, content, uint32(st.Mode().Perm())); err != nil {
						c.warn(fmt.Sprintf("%s: could not save a copy in the trash: %v", r, err))
					}
				}
			}
			if err := os.Remove(full); err != nil {
				c.warn(fmt.Sprintf("%s: %v", r, err))
				continue
			}
			removed = append(removed, r)
		}
	}
	pushErr := c.push(ctx, s)
	if c.g.json {
		if err := c.printJSON(struct {
			Project      string   `json:"project"`
			Deleted      []string `json:"deleted"`
			RemovedLocal []string `json:"removed_local"`
		}{p.ID, rels, nonNil(removed)}); err != nil {
			return err
		}
		return pushErr
	}
	removedSet := map[string]bool{}
	for _, r := range removed {
		removedSet[r] = true
	}
	for _, r := range rels {
		note := "deleted in the vault, local copy kept"
		if removedSet[r] {
			note = "deleted in the vault and locally (copy in the trash)"
		}
		fmt.Fprintf(c.out, "%s %s  %s\n", c.paint(colorRed, glyphUp), r, c.paint(colorDim, note))
	}
	fmt.Fprintf(c.out, "deleted %d file(s) of %s everywhere\n", len(rels), projectLabel(p))
	return pushErr
}

// projectLabel renders "name (id)" for messages.
func projectLabel(p *config.ProjectConfig) string {
	if p.Name == "" {
		return p.ID
	}
	return fmt.Sprintf("%s (%s)", p.Name, p.ID)
}

// trashCommand groups list | restore | purge.
func (c *cli) trashCommand() *cobra.Command {
	cmd := groupCommand("trash", "Manage the encrypted local trash")
	var olderThan string
	purge := &cobra.Command{
		Use:   "purge [--older-than 30d]",
		Short: "Remove trash entries older than a duration",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			age, err := parseAge(olderThan)
			if err != nil {
				return usagef("--older-than %q: %v", olderThan, err)
			}
			s, err := c.openSession(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			n, err := s.State.TrashPurge(age)
			if err != nil {
				return err
			}
			if c.g.json {
				return c.printJSON(struct {
					Purged int `json:"purged"`
				}{n})
			}
			fmt.Fprintf(c.out, "purged %d trash entr%s older than %s\n", n, plural(n, "y", "ies"), olderThan)
			return nil
		},
	}
	purge.Flags().StringVar(&olderThan, "older-than", "30d", "age threshold (e.g. 30d, 12h, 1w)")
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the trash entries (newest first)",
			Args:  noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				s, err := c.openSession(cmd.Context())
				if err != nil {
					return err
				}
				defer s.Close()
				entries, err := s.State.TrashList()
				if err != nil {
					return err
				}
				return c.printTrash(s, entries)
			},
		},
		&cobra.Command{
			Use:   "restore <id>",
			Short: "Write a trash entry back into its project directory",
			Args:  exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return c.trashRestore(cmd, args[0])
			},
		},
		purge,
	)
	return cmd
}

// trashView is the JSON shape of a trash entry.
type trashView struct {
	ID      string `json:"id"`
	Time    string `json:"time"`
	Project string `json:"project"`
	Name    string `json:"project_name,omitempty"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
}

// printTrash renders the trash index.
func (c *cli) printTrash(s *app.Session, entries []state.TrashEntry) error {
	name := func(id string) string {
		if p, ok := s.Config.Project(id); ok && p.Name != "" {
			return p.Name
		}
		return ""
	}
	if c.g.json {
		views := make([]trashView, 0, len(entries))
		for _, e := range entries {
			views = append(views, trashView{
				ID: e.ID, Time: e.Time.UTC().Format(time.RFC3339), Project: e.Project, Name: name(e.Project),
				Path: e.Path, Size: e.Size, Mode: fmt.Sprintf("%04o", e.Mode&0o7777),
			})
		}
		return c.printJSON(struct {
			Entries []trashView `json:"entries"`
		}{views})
	}
	if len(entries) == 0 {
		fmt.Fprintln(c.out, "the trash is empty")
		return nil
	}
	rows := [][]string{{"ID", "DATE", "PROJECT", "PATH", "SIZE"}}
	for _, e := range entries {
		proj := e.Project
		if n := name(e.Project); n != "" {
			proj = n
		}
		rows = append(rows, []string{e.ID, e.Time.Local().Format("2006-01-02 15:04"), proj, e.Path, formatSize(e.Size)})
	}
	c.table(rows)
	return nil
}

// trashRestore writes an entry back to its project path after confirmation.
// An existing file with different content is refused without --yes (the
// current content is saved in the trash before being overwritten).
func (c *cli) trashRestore(cmd *cobra.Command, id string) error {
	ctx := cmd.Context()
	s, err := c.openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	content, entry, err := s.State.TrashRead(s.Vault.Keys(), strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if !sync.ValidPath(entry.Path) {
		return fmt.Errorf("trash entry %s has an invalid path %q", entry.ID, entry.Path)
	}
	p, ok := s.Config.Project(entry.Project)
	if !ok {
		return fmt.Errorf("%w: project %s of trash entry %s (link it first with projects link)", app.ErrNotLinked, entry.Project, entry.ID)
	}
	dir, err := s.Config.ProjectPath(p.ID)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.FromSlash(entry.Path))
	mode := fs.FileMode(entry.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	if existing, err := os.ReadFile(target); err == nil {
		if bytes.Equal(existing, content) {
			fmt.Fprintf(c.out, "%s already has the content of trash entry %s\n", display(target), entry.ID)
			return nil
		}
		if !c.g.yes {
			return fmt.Errorf("%s exists with different content; pass --yes to overwrite it (the current content is kept in the trash)", display(target))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	ok, err = c.confirm(ctx, fmt.Sprintf("restore %s (%s, %s) to %s?", entry.Path, formatSize(entry.Size), entry.Time.Local().Format("2006-01-02 15:04"), display(target)), true)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: nothing restored", errAborted)
	}
	if existing, err := os.ReadFile(target); err == nil && !bytes.Equal(existing, content) {
		st, statErr := os.Lstat(target)
		var curMode uint32 = 0o600
		if statErr == nil {
			curMode = uint32(st.Mode().Perm())
		}
		if _, err := s.State.TrashPut(s.Vault.Keys(), p.ID, entry.Path, existing, curMode); err != nil {
			return fmt.Errorf("save the current content in the trash: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(target, content, mode, tempSuffix(s.Machine.ID)); err != nil {
		return err
	}
	if c.g.json {
		return c.printJSON(struct {
			ID     string `json:"id"`
			Path   string `json:"path"`
			Target string `json:"target"`
			Size   int    `json:"size"`
		}{entry.ID, entry.Path, display(target), len(content)})
	}
	fmt.Fprintf(c.out, "restored %s (%s) to %s\n", entry.Path, formatSize(int64(len(content))), display(target))
	return nil
}

// tempSuffix identifies this machine's temp files (first 8 chars of the id).
func tempSuffix(machineID string) string {
	if len(machineID) > 8 {
		return machineID[:8]
	}
	if machineID == "" {
		return "cli"
	}
	return machineID
}

// ageUnitRE matches one leading day or week component of an age ("30d", "1w").
var ageUnitRE = regexp.MustCompile(`^(\d+)([dw])`)

// parseAge parses "30d", "1w", "12h", "1d12h", "1w2d", "2d1w6h": any number
// of day/week components in any order, followed by an optional
// time.ParseDuration tail. Negative and overflowing values are errors.
func parseAge(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, errors.New("empty duration")
	}
	var d time.Duration
	for {
		m := ageUnitRE.FindStringSubmatch(s)
		if m == nil {
			break
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, err
		}
		unit := 24 * time.Hour
		if m[2] == "w" {
			unit *= 7
		}
		if n > int64(math.MaxInt64/unit) || d > math.MaxInt64-time.Duration(n)*unit {
			return 0, errors.New("duration too large")
		}
		d += time.Duration(n) * unit
		s = s[len(m[0]):]
	}
	if s == "" {
		return d, nil
	}
	rest, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if rest < 0 {
		return 0, errors.New("negative duration")
	}
	if d > math.MaxInt64-rest {
		return 0, errors.New("duration too large")
	}
	return d + rest, nil
}

// plural picks the singular or plural suffix.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
