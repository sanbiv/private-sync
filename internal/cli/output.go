package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/sanbiv/private-sync/internal/sync"
)

// Report glyphs (spec: "↑ path", "↓ path", "= path", "! conflict",
// "… pending", "⚠ rollback").
const (
	glyphUp       = "↑"
	glyphDown     = "↓"
	glyphSame     = "="
	glyphConflict = "!"
	glyphPending  = "…"
	glyphRollback = "⚠"
	glyphInfo     = "·"
	glyphMissing  = "?"
	glyphUntrack  = "-"
	glyphError    = "✗"
)

// ANSI colours (only used when stdout is a terminal).
const (
	colorNone   = ""
	colorRed    = "31"
	colorGreen  = "32"
	colorYellow = "33"
	colorCyan   = "36"
	colorDim    = "2"
)

// paint wraps s in an ANSI colour when colours are enabled.
func (c *cli) paint(color, s string) string {
	if !c.color || color == colorNone || s == "" {
		return s
	}
	return "\x1b[" + color + "m" + s + "\x1b[0m"
}

// table prints aligned columns.
func (c *cli) table(rows [][]string) {
	tw := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

// printJSON writes v indented to stdout.
func (c *cli) printJSON(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// formatSize renders a byte count compactly.
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGT"[exp])
}

// itemView is the JSON shape of a planned item.
type itemView struct {
	Path            string `json:"path"`
	Action          string `json:"action"`
	Original        string `json:"original_action,omitempty"`
	Conflict        string `json:"conflict,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
	NeedsResolution bool   `json:"needs_resolution,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Error           string `json:"error,omitempty"`
}

// projectView is the JSON shape of a project's plan.
type projectView struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	Badge       string      `json:"badge"`
	Missing     bool        `json:"missing,omitempty"`
	Unreadable  bool        `json:"unreadable,omitempty"`
	DuplicateOf string      `json:"duplicate_of,omitempty"`
	Summary     summaryView `json:"summary"`
	Items       []itemView  `json:"items"`
	Warnings    []string    `json:"warnings,omitempty"`
}

// summaryView is summary with JSON tags.
type summaryView struct {
	InSync         int `json:"in_sync"`
	LocalChanges   int `json:"local_changes"`
	RemoteChanges  int `json:"remote_changes"`
	Conflicts      int `json:"conflicts"`
	Pending        int `json:"pending"`
	Rollback       int `json:"rollback"`
	Missing        int `json:"missing"`
	DeletedInVault int `json:"deleted_in_vault,omitempty"`
}

func toSummaryView(s summary) summaryView {
	return summaryView{
		InSync:         s.InSync,
		LocalChanges:   s.LocalChanges,
		RemoteChanges:  s.RemoteChanges,
		Conflicts:      s.Conflicts,
		Pending:        s.Pending,
		Rollback:       s.Rollback,
		Missing:        s.Missing,
		DeletedInVault: s.DeletedInVault,
	}
}

// summary is sync.Summary with the decision table row 9 items split out of
// Missing: they are deleted in the vault but still present on disk, so
// counting them as "missing locally" contradicts what the user sees.
type summary struct {
	sync.Summary
	DeletedInVault int
}

// deletedInVault reports whether it is row 9 (or its too-large variant): the
// vault head is a tombstone the base already records, and the local copy is
// deliberately left in place. Plan reports these with Original
// ActionMissingLocal, which sync.Summarize counts as Missing; a genuine
// ActionMissingLocal has no local file at all (Local == nil).
func deletedInVault(it *sync.Item) bool {
	if it == nil || it.Local == nil {
		return false
	}
	a := it.Action
	if a == sync.ActionReportOnly {
		a = it.Original
	}
	return a == sync.ActionMissingLocal
}

// summarize is sync.Summarize with the row 9 items moved to DeletedInVault.
func summarize(pp *sync.ProjectPlan) summary {
	s := summary{Summary: sync.Summarize(pp)}
	if pp == nil {
		return s
	}
	for i := range pp.Items {
		if deletedInVault(&pp.Items[i]) {
			s.Missing--
			s.DeletedInVault++
		}
	}
	if s.Missing < 0 {
		s.Missing = 0
	}
	return s
}

// badge summarises a project's plan in one word for lists and status.
func badge(pp *sync.ProjectPlan) string {
	if pp == nil {
		return ""
	}
	switch {
	case pp.Missing:
		return "path missing"
	case pp.DuplicateOf != "":
		return "duplicate of " + pp.DuplicateOf
	case pp.Unreadable:
		return "unreadable"
	}
	s := summarize(pp)
	switch {
	case s.Conflicts > 0:
		return "conflicts"
	case s.Pending > 0:
		return "pending"
	case s.Rollback > 0:
		return "rollback"
	case s.LocalChanges > 0 && s.RemoteChanges > 0:
		return "local and remote changes"
	case s.LocalChanges > 0:
		return "local changes"
	case s.RemoteChanges > 0:
		return "remote changes"
	case s.Missing > 0:
		return "missing locally"
	case s.DeletedInVault > 0:
		return "deleted in the vault, local copy kept"
	}
	return "synced"
}

// summaryText renders a summary for humans ("2 in sync, 1 local change").
func summaryText(s summary) string {
	var parts []string
	add := func(n int, one, many string) {
		if n == 0 {
			return
		}
		if n == 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, one))
			return
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, many))
	}
	add(s.InSync, "in sync", "in sync")
	add(s.LocalChanges, "local change", "local changes")
	add(s.RemoteChanges, "remote change", "remote changes")
	add(s.Conflicts, "conflict", "conflicts")
	add(s.Pending, "pending", "pending")
	add(s.Rollback, "rollback", "rollbacks")
	add(s.Missing, "missing locally", "missing locally")
	add(s.DeletedInVault, "deleted in the vault (local copy kept)", "deleted in the vault (local copies kept)")
	if len(parts) == 0 {
		return "no tracked files"
	}
	return strings.Join(parts, ", ")
}

// describe renders an item as glyph, colour and note. res is nil when
// nothing was applied (status); applied reports whether Apply ran, so a
// conflict with a clean automatic merge is shown as done rather than pending.
func describe(it *sync.Item, mode sync.Mode, res sync.Resolutions, applied bool) (glyph, color, note string) {
	r, resolved := res[it.Key]
	switch it.Action {
	case sync.ActionUpload:
		if it.Synthetic {
			return glyphUp, colorGreen, "merged concurrent versions"
		}
		return glyphUp, colorGreen, ""
	case sync.ActionDeleteRemote:
		switch {
		case !applied:
			return glyphUp, colorGreen, "delete in the vault"
		case resolved && r.Kind == sync.ChooseConfirm:
			return glyphUp, colorGreen, "deleted in the vault"
		}
		return glyphConflict, colorYellow, "deletion not confirmed: " + it.Reason
	case sync.ActionDownload:
		if it.Synthetic {
			return glyphDown, colorGreen, "merged concurrent versions"
		}
		return glyphDown, colorGreen, ""
	case sync.ActionTrashLocal:
		return glyphDown, colorGreen, "deleted in the vault: local copy moved to the trash"
	case sync.ActionConverge, sync.ActionInSync:
		return glyphSame, colorDim, ""
	case sync.ActionUntrack:
		return glyphUntrack, colorDim, "untracked in the vault, local file left in place"
	case sync.ActionMissingLocal:
		return glyphMissing, colorYellow, "missing locally"
	case sync.ActionConflict:
		if !it.NeedsResolution && it.Merge != nil && it.Merge.Clean {
			if applied {
				return glyphUp, colorGreen, "merged automatically"
			}
			return glyphConflict, colorCyan, it.Reason
		}
		if !resolved || r.Kind == sync.ChooseSkip || r.Kind == sync.ChooseNone {
			return glyphConflict, colorYellow, "conflict: " + it.Reason
		}
		switch r.Kind {
		case sync.ChooseLocal, sync.ChooseKeep:
			return glyphUp, colorGreen, "resolved: kept the local version"
		case sync.ChooseRemote, sync.ChooseDelete:
			return glyphDown, colorGreen, "resolved: took the vault version"
		case sync.ChooseMerged:
			return glyphUp, colorGreen, "resolved: merged"
		case sync.ChooseCustom:
			return glyphUp, colorGreen, "resolved: edited"
		}
		return glyphUp, colorGreen, "resolved"
	case sync.ActionPending:
		return glyphPending, colorYellow, "pending: " + it.Reason
	case sync.ActionRollback:
		return glyphRollback, colorRed, "rollback: " + it.Reason
	case sync.ActionReportOnly:
		if it.Original == sync.ActionMissingLocal {
			return glyphMissing, colorYellow, it.Reason
		}
		if mode == sync.ModeSync || mode == sync.ModeRestore {
			return glyphInfo, colorDim, it.Reason
		}
		return glyphInfo, colorDim, fmt.Sprintf("not applied by %s (would %s): %s", mode, it.Original, it.Reason)
	}
	return glyphInfo, colorNone, it.Reason
}

// itemLine renders one item for the terminal.
func (c *cli) itemLine(it *sync.Item, mode sync.Mode, res sync.Resolutions, applied bool, err error) string {
	glyph, color, note := describe(it, mode, res, applied)
	if err != nil {
		glyph, color, note = glyphError, colorRed, err.Error()
	}
	line := c.paint(color, glyph) + " " + it.Key.Path
	if note != "" {
		line += "  " + c.paint(colorDim, note)
	}
	return line
}

// projectHeader renders the "name (id) path — badge" line.
func (c *cli) projectHeader(pp *sync.ProjectPlan) string {
	name := pp.Name
	if name == "" {
		name = pp.ID
	}
	h := fmt.Sprintf("%s (%s) %s", c.paint(colorCyan, name), pp.ID, display(pp.Path))
	b := badge(pp)
	if b != "" {
		h += "  " + c.paint(colorDim, b)
	}
	return h
}

// projectViews converts a plan for JSON output.
func projectViews(p *sync.Plan, res sync.Resolutions, errs map[sync.ItemKey]error) []projectView {
	out := make([]projectView, 0, len(p.Projects))
	for pi := range p.Projects {
		pp := &p.Projects[pi]
		pv := projectView{
			ID:          pp.ID,
			Name:        pp.Name,
			Path:        display(pp.Path),
			Badge:       badge(pp),
			Missing:     pp.Missing,
			Unreadable:  pp.Unreadable,
			DuplicateOf: pp.DuplicateOf,
			Summary:     toSummaryView(summarize(pp)),
			Items:       make([]itemView, 0, len(pp.Items)),
			Warnings:    pp.Warnings,
		}
		for i := range pp.Items {
			it := &pp.Items[i]
			iv := itemView{
				Path:            it.Key.Path,
				Action:          it.Action.String(),
				NeedsResolution: it.NeedsResolution,
				Reason:          it.Reason,
			}
			if it.Action == sync.ActionReportOnly || it.Action == sync.ActionRollback {
				iv.Original = it.Original.String()
			}
			if it.Conflict != sync.ConflictNone {
				iv.Conflict = it.Conflict.String()
			}
			if r, ok := res[it.Key]; ok {
				iv.Resolution = r.Kind.String()
			}
			if err := errs[it.Key]; err != nil {
				iv.Error = err.Error()
			}
			pv.Items = append(pv.Items, iv)
		}
		out = append(out, pv)
	}
	return out
}

// reportCounts is sync.Report's counters with JSON tags.
type reportCounts struct {
	Uploaded   int `json:"uploaded"`
	Downloaded int `json:"downloaded"`
	Converged  int `json:"converged"`
	Trashed    int `json:"trashed"`
	Untracked  int `json:"untracked"`
	Deleted    int `json:"deleted"`
	Resolved   int `json:"resolved"`
	Skipped    int `json:"skipped"`
	Pending    int `json:"pending"`
}

type keyView struct {
	Project string `json:"project"`
	Path    string `json:"path"`
}

// linkView is the JSON shape of a project mapping (projects link, restore --path).
type linkView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type reportView struct {
	Mode       string        `json:"mode"`
	Linked     *linkView     `json:"linked,omitempty"` // restore --path
	Projects   []projectView `json:"projects"`
	Report     reportCounts  `json:"report"`
	Unresolved []keyView     `json:"unresolved"`
	Errors     []string      `json:"errors,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
}

// unresolvedKeys lists the items that still need attention after Apply:
// the ones Apply reported plus conflicts skipped by the strategy.
func unresolvedKeys(p *sync.Plan, res sync.Resolutions, rep *sync.Report) []sync.ItemKey {
	seen := map[sync.ItemKey]bool{}
	var out []sync.ItemKey
	if rep != nil {
		for _, k := range rep.Unresolved {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	for _, it := range p.Items() {
		if !it.NeedsResolution || seen[it.Key] {
			continue
		}
		r, ok := res[it.Key]
		if !ok || r.Kind == sync.ChooseSkip || r.Kind == sync.ChooseNone {
			seen[it.Key] = true
			out = append(out, it.Key)
		}
	}
	return out
}

// printReport renders the outcome of Apply (text or JSON).
func (c *cli) printReport(p *sync.Plan, res sync.Resolutions, rep *sync.Report) error {
	if rep == nil {
		rep = &sync.Report{}
	}
	errs := make(map[sync.ItemKey]error, len(rep.Errors))
	for _, e := range rep.Errors {
		errs[e.Key] = e.Err
	}
	unresolved := unresolvedKeys(p, res, rep)
	if c.g.json {
		rv := reportView{
			Mode:     p.Mode.String(),
			Linked:   c.linked,
			Projects: projectViews(p, res, errs),
			Report: reportCounts{
				Uploaded: rep.Uploaded, Downloaded: rep.Downloaded, Converged: rep.Converged,
				Trashed: rep.Trashed, Untracked: rep.Untracked, Deleted: rep.Deleted,
				Resolved: rep.Resolved, Skipped: rep.Skipped, Pending: rep.Pending,
			},
			Unresolved: make([]keyView, 0, len(unresolved)),
			Warnings:   p.Warnings,
		}
		for _, k := range unresolved {
			rv.Unresolved = append(rv.Unresolved, keyView{Project: k.Project, Path: k.Path})
		}
		for _, e := range rep.Errors {
			rv.Errors = append(rv.Errors, fmt.Sprintf("%s/%s: %v", e.Key.Project, e.Key.Path, e.Err))
		}
		return c.printJSON(rv)
	}
	for pi := range p.Projects {
		pp := &p.Projects[pi]
		fmt.Fprintln(c.out, c.projectHeader(pp))
		for i := range pp.Items {
			it := &pp.Items[i]
			fmt.Fprintln(c.out, "  "+c.itemLine(it, p.Mode, res, true, errs[it.Key]))
		}
		for _, w := range pp.Warnings {
			fmt.Fprintln(c.out, "  "+c.paint(colorYellow, "warning: "+w))
		}
	}
	if len(p.Projects) == 0 {
		fmt.Fprintln(c.out, "no linked projects (use `private-sync add <path>` or `projects link`)")
	}
	fmt.Fprintln(c.out, reportSummary(rep, len(unresolved), skippedProjects(p)))
	return nil
}

// skippedProjects counts the projects Plan could not plan at all (unreadable
// journal, missing directory): nothing of theirs was applied, so the summary
// must not claim there was nothing to do.
func skippedProjects(p *sync.Plan) int {
	if p == nil {
		return 0
	}
	n := 0
	for i := range p.Projects {
		if p.Projects[i].Unreadable || p.Projects[i].Missing {
			n++
		}
	}
	return n
}

// reportSummary renders the counters of a report in one line.
func reportSummary(rep *sync.Report, unresolved, skipped int) string {
	var parts []string
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", label, n))
		}
	}
	add(rep.Uploaded, "uploaded")
	add(rep.Downloaded, "downloaded")
	add(rep.Converged, "converged")
	add(rep.Trashed, "trashed")
	add(rep.Untracked, "untracked")
	add(rep.Deleted, "deleted")
	add(rep.Resolved, "resolved")
	add(rep.Skipped, "skipped")
	add(rep.Pending, "pending")
	add(len(rep.Errors), "errors")
	add(unresolved, "unresolved")
	if len(parts) == 0 {
		if skipped > 0 {
			return fmt.Sprintf("nothing applied (%d project(s) skipped)", skipped)
		}
		return "nothing to do"
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d project(s) skipped", skipped))
	}
	return strings.Join(parts, ", ")
}
