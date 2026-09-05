package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/sync"
)

// settingsStep is where the settings screen is (spec §2.2 item 6).
type settingsStep int

const (
	settingsStepForm settingsStep = iota
	settingsStepRekeyForm
	settingsStepRekeying
	settingsStepDone
)

// settingsModel is the settings screen: a Huh form for the editable config
// fields, plus an optional "change passphrase" sub-flow.
type settingsModel struct {
	ctx context.Context
	s   *app.Session

	step settingsStep

	form *huh.Form

	machineName              string
	remoteType               string
	gitURL, gitBranch        string
	rcloneRemote, rclonePath string
	keySource                string
	keyFilePath              string
	bwItem, bwField          string
	includeCSV               string
	excludeDirsCSV           string
	excludeFilesCSV          string
	wantRekey                bool

	rekeyForm    *huh.Form
	pass1, pass2 string
	rekeyErr     string
	// rekeyWarning carries a non-fatal note to show once rekeying settles
	// (e.g. "update the Bitwarden item by hand"), set by startRekey.
	rekeyWarning string

	sv  *syncView
	err string

	width, height int
}

func newSettingsModel(ctx context.Context, s *app.Session) *settingsModel {
	cfg := s.Config
	m := &settingsModel{
		ctx:             ctx,
		s:               s,
		machineName:     cfg.Machine.Name,
		remoteType:      string(cfg.Vault.Remote.Type),
		gitURL:          cfg.Vault.Remote.Git.URL,
		gitBranch:       cfg.Vault.Remote.Git.Branch,
		rcloneRemote:    cfg.Vault.Remote.Rclone.Remote,
		rclonePath:      cfg.Vault.Remote.Rclone.Path,
		keySource:       string(cfg.Key.Source),
		keyFilePath:     cfg.Key.File.Path,
		bwItem:          cfg.Key.Bitwarden.Item,
		bwField:         cfg.Key.Bitwarden.Field,
		includeCSV:      joinCSV(cfg.Scan.Include),
		excludeDirsCSV:  joinCSV(cfg.Scan.ExcludeDirs),
		excludeFilesCSV: joinCSV(cfg.Scan.ExcludeFiles),
	}
	m.form = m.buildForm()
	return m
}

func joinCSV(list []string) string { return strings.Join(list, ", ") }

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (m *settingsModel) buildForm() *huh.Form {
	groupGeneral := huh.NewGroup(
		huh.NewInput().Title("Machine name").Value(&m.machineName),
		huh.NewSelect[string]().Title("Remote").Value(&m.remoteType).Options(
			huh.NewOption("None", string(config.RemoteNone)),
			huh.NewOption("Git", string(config.RemoteGit)),
			huh.NewOption("Rclone", string(config.RemoteRclone)),
		),
	).Title("General")

	groupGit := huh.NewGroup(
		huh.NewInput().Title("Git remote URL").Value(&m.gitURL),
		huh.NewInput().Title("Branch").Value(&m.gitBranch),
	).Title("Git remote").WithHideFunc(func() bool { return m.remoteType != string(config.RemoteGit) })

	groupRclone := huh.NewGroup(
		huh.NewInput().Title("Rclone remote name").Value(&m.rcloneRemote),
		huh.NewInput().Title("Path on the remote").Value(&m.rclonePath),
	).Title("Rclone remote").WithHideFunc(func() bool { return m.remoteType != string(config.RemoteRclone) })

	groupKey := huh.NewGroup(
		huh.NewSelect[string]().Title("Passphrase source").Value(&m.keySource).Options(
			huh.NewOption("Prompt at every launch", string(config.KeyPrompt)),
			huh.NewOption("File on disk", string(config.KeyFile)),
			huh.NewOption("Bitwarden", string(config.KeyBitwarden)),
		),
	).Title("Encryption key").Description("Takes effect at next launch")

	groupKeyFile := huh.NewGroup(
		huh.NewInput().Title("Key file path").Value(&m.keyFilePath),
	).Title("Key file").WithHideFunc(func() bool { return m.keySource != string(config.KeyFile) })

	groupBitwarden := huh.NewGroup(
		huh.NewInput().Title("Bitwarden item").Value(&m.bwItem),
		huh.NewInput().Title("Bitwarden field").Value(&m.bwField),
	).Title("Bitwarden").WithHideFunc(func() bool { return m.keySource != string(config.KeyBitwarden) })

	groupScan := huh.NewGroup(
		huh.NewInput().Title("Include globs").Description("comma-separated; blank = defaults").Value(&m.includeCSV),
		huh.NewInput().Title("Exclude directories").Description("comma-separated; blank = defaults").Value(&m.excludeDirsCSV),
		huh.NewInput().Title("Exclude files").Description("comma-separated; blank = defaults").Value(&m.excludeFilesCSV),
	).Title("Scanning")

	groupFinish := huh.NewGroup(
		huh.NewConfirm().Title("Change passphrase now?").Value(&m.wantRekey).Affirmative("Yes").Negative("No"),
	).Title("Save")

	return huh.NewForm(groupGeneral, groupGit, groupRclone, groupKey, groupKeyFile, groupBitwarden, groupScan, groupFinish).
		WithTheme(huh.ThemeCharm())
}

func (m *settingsModel) buildRekeyForm() *huh.Form {
	m.pass1, m.pass2 = "", ""
	group := huh.NewGroup(
		huh.NewInput().Title("New passphrase").EchoMode(huh.EchoModePassword).Value(&m.pass1).
			Validate(requiredField("passphrase")),
		huh.NewInput().Title("Confirm passphrase").EchoMode(huh.EchoModePassword).Value(&m.pass2).
			Validate(requiredField("passphrase")),
	).Title("Change passphrase")
	return huh.NewForm(group).WithTheme(huh.ThemeCharm())
}

func (m *settingsModel) SetSize(w, h int) {
	m.width, m.height = w, h
	if m.sv != nil {
		m.sv.SetSize(w, h)
	}
}

func (m *settingsModel) Init() tea.Cmd { return m.form.Init() }

// Update handles one message; the returned bool is true once the screen
// should be dismissed back to the dashboard.
func (m *settingsModel) Update(msg tea.Msg) (bool, tea.Cmd) {
	switch m.step {
	case settingsStepForm:
		return m.updateForm(msg)
	case settingsStepRekeyForm:
		return m.updateRekeyForm(msg)
	case settingsStepRekeying:
		return m.updateRekeying(msg)
	case settingsStepDone:
		if key, ok := msg.(tea.KeyMsg); ok && (key.String() == "enter" || key.String() == "esc") {
			return true, nil
		}
	}
	return false, nil
}

func (m *settingsModel) updateForm(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "esc" {
		return true, nil
	}
	next, cmd := m.form.Update(msg)
	if f, ok := next.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateAborted:
		return true, cmd
	case huh.StateCompleted:
		if err := m.save(); err != nil {
			m.err = err.Error()
			return false, cmd
		}
		if m.wantRekey {
			m.rekeyForm = m.buildRekeyForm()
			m.step = settingsStepRekeyForm
			return false, tea.Batch(cmd, m.rekeyForm.Init())
		}
		m.step = settingsStepDone
		return false, cmd
	}
	return false, cmd
}

// save applies the form's fields to the config and persists it. It builds
// the new values on a copy and validates that copy before installing it: the
// naive field-by-field mutation of the live *config.Config left the
// in-memory session config half-updated and invalid whenever SaveConfig's
// own Validate call refused to write (e.g. remote=git with an empty URL),
// so every later Plan/ScanOptions/LinkProject in the same dashboard session
// would keep using the rejected values while the file on disk still had the
// old, valid ones.
func (m *settingsModel) save() error {
	cfg := *m.s.Config
	cfg.Machine.Name = strings.TrimSpace(m.machineName)
	cfg.Vault.Remote = config.RemoteConfig{
		Type: config.RemoteType(m.remoteType),
		Git: config.GitRemote{
			URL:    strings.TrimSpace(m.gitURL),
			Branch: strings.TrimSpace(m.gitBranch),
		},
		Rclone: config.RcloneRemote{
			Remote: strings.TrimSpace(m.rcloneRemote),
			Path:   strings.TrimSpace(m.rclonePath),
		},
	}
	cfg.Key = config.KeyConfig{
		Source:    config.KeySource(m.keySource),
		File:      config.FileKey{Path: strings.TrimSpace(m.keyFilePath)},
		Bitwarden: config.BitwardenKey{Item: strings.TrimSpace(m.bwItem), Field: strings.TrimSpace(m.bwField)},
	}
	cfg.Scan.Include = splitCSV(m.includeCSV)
	cfg.Scan.ExcludeDirs = splitCSV(m.excludeDirsCSV)
	cfg.Scan.ExcludeFiles = splitCSV(m.excludeFilesCSV)
	if err := cfg.Validate(); err != nil {
		return err
	}
	*m.s.Config = cfg
	return m.s.SaveConfig()
}

func (m *settingsModel) updateRekeyForm(msg tea.Msg) (bool, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "esc" {
		m.step = settingsStepDone
		return false, nil
	}
	next, cmd := m.rekeyForm.Update(msg)
	if f, ok := next.(*huh.Form); ok {
		m.rekeyForm = f
	}
	switch m.rekeyForm.State {
	case huh.StateAborted:
		m.step = settingsStepDone
		return false, cmd
	case huh.StateCompleted:
		if m.pass1 != m.pass2 {
			m.rekeyErr = "passphrases do not match"
			m.rekeyForm = m.buildRekeyForm()
			m.step = settingsStepRekeyForm
			return false, tea.Batch(cmd, m.rekeyForm.Init())
		}
		return false, tea.Batch(cmd, m.startRekey())
	}
	return false, cmd
}

// rekeyDoneMsg reports that Vault.Rekey (and, for key.source=file, the key
// file rewrite) finished, before the push stage.
type rekeyDoneMsg struct {
	err error
	// warning is a non-fatal note to show once settled (key.source=bitwarden
	// cannot be rewritten for the user; they must update it by hand).
	warning string
}

// startRekey rewraps the vault key with the new passphrase and, for
// key.source=file, rewrites the key file with it too — otherwise the next
// launch reads the old passphrase from that file, fails to derive the
// (now-rewrapped) vault key, and the user is locked out until they fix the
// key file by hand. This mirrors the CLI's `passphrase change`
// (internal/cli/cmd_root.go's updateKeyFile): resolve symlinks, then an
// atomic 0600 write.
func (m *settingsModel) startRekey() tea.Cmd {
	m.step = settingsStepRekeying
	pass := []byte(m.pass1)
	m.pass1, m.pass2 = "", ""
	v := m.s.Vault
	cfg := m.s.Config
	return func() tea.Msg {
		defer crypto.Zero(pass)
		params, err := crypto.DefaultKDFParams()
		if err != nil {
			return rekeyDoneMsg{err: err}
		}
		// Rekey zeroes the buffer it receives; keep pass itself intact for
		// the key file update below.
		if err := v.Rekey(append([]byte(nil), pass...), params); err != nil {
			return rekeyDoneMsg{err: err}
		}
		switch cfg.Key.Source {
		case config.KeyFile:
			keyPath, err := cfg.KeyFilePath()
			if err == nil {
				err = writeKeyFilePassphrase(keyPath, pass)
			}
			if err != nil {
				return rekeyDoneMsg{err: fmt.Errorf("passphrase changed but the key file could not be updated (%w): update %s by hand before the next launch", err, keyPath)}
			}
		case config.KeyBitwarden:
			return rekeyDoneMsg{warning: fmt.Sprintf("update the Bitwarden item %q with the new passphrase before the next launch", cfg.Key.Bitwarden.Item)}
		}
		return rekeyDoneMsg{}
	}
}

// writeKeyFilePassphrase rewrites the key file (symlinks resolved) with
// passphrase: an atomic rename from a 0600 temp file in the same directory,
// so the file keysource reads is always complete.
func writeKeyFilePassphrase(path string, passphrase []byte) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("key.file.path is empty")
	}
	resolved, err := keysource.ResolveKeyFile(path)
	if err != nil {
		return err
	}
	data := make([]byte, 0, len(passphrase)+1)
	data = append(data, passphrase...)
	data = append(data, '\n')
	defer keysource.Zero(data)
	return fsutil.WriteFileAtomic(resolved, data, 0o600, "key")
}

func (m *settingsModel) updateRekeying(msg tea.Msg) (bool, tea.Cmd) {
	if msg, ok := msg.(rekeyDoneMsg); ok {
		if msg.err != nil {
			m.err = msg.err.Error()
			m.step = settingsStepDone
			return false, nil
		}
		m.rekeyWarning = msg.warning
		m.sv = newSyncView(m.ctx, m.s.Engine, sync.Options{}, stagePush, false, true)
		m.sv.SetSize(m.width, m.height)
		return false, m.sv.Init()
	}
	if m.sv == nil {
		return false, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok && m.sv.stage.terminal() {
		if key.String() == "enter" || key.String() == "esc" {
			m.step = settingsStepDone
			return false, nil
		}
	}
	sv, cmd := m.sv.Update(msg)
	m.sv = sv
	return false, cmd
}

func (m *settingsModel) View() string {
	var b strings.Builder
	b.WriteString(styles.Title.Render("Settings") + "\n\n")
	if m.err != "" {
		b.WriteString(styles.Error.Render(m.err) + "\n")
	}
	switch m.step {
	case settingsStepForm:
		b.WriteString(m.form.View())
	case settingsStepRekeyForm:
		if m.rekeyErr != "" {
			b.WriteString(styles.Error.Render(m.rekeyErr) + "\n")
		}
		b.WriteString(m.rekeyForm.View())
	case settingsStepRekeying:
		if m.sv != nil {
			b.WriteString(m.sv.View())
		} else {
			b.WriteString("changing passphrase...\n")
		}
	case settingsStepDone:
		b.WriteString(styles.Success.Render("Settings saved.") + "\n\n")
		if m.rekeyWarning != "" {
			b.WriteString(styles.Warning.Render(m.rekeyWarning) + "\n\n")
		}
		b.WriteString(styles.Help.Render("enter: back to dashboard"))
	}
	return b.String()
}
