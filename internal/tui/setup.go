package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/paths"
)

// RunSetup runs the standalone setup wizard (spec §2.2 item 1): one Huh
// multi-group form (vault path + machine name; remote type and its
// parameters; key source and its parameters; a final confirm). Nothing is
// written to disk here — the caller (cli/app.Setup) saves the returned
// config and runs the vault creation protocol.
func RunSetup(ctx context.Context, dirs paths.Dirs, existing *config.Config) (*config.Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	base := setupDefaults(dirs, existing)

	vaultPath := base.Vault.Path
	machineName := base.Machine.Name
	remoteType := string(base.Vault.Remote.Type)
	gitURL := base.Vault.Remote.Git.URL
	gitBranch := base.Vault.Remote.Git.Branch
	rcloneRemote := base.Vault.Remote.Rclone.Remote
	rclonePath := base.Vault.Remote.Rclone.Path
	keySource := string(base.Key.Source)
	keyFilePath := base.Key.File.Path
	bwItem := base.Key.Bitwarden.Item
	bwField := base.Key.Bitwarden.Field
	confirmed := false

	if remoteType == "" {
		remoteType = string(config.RemoteNone)
	}
	if keySource == "" {
		keySource = string(config.KeyPrompt)
	}

	groupVault := huh.NewGroup(
		huh.NewInput().
			Title("Vault path").
			Description("Where the encrypted vault is kept on this machine (~ is expanded)").
			Value(&vaultPath).
			Validate(requiredField("vault path")),
		huh.NewInput().
			Title("Machine name").
			Description("Shown to other machines that share this vault").
			Value(&machineName).
			Validate(requiredField("machine name")),
	).Title("Vault")

	groupRemote := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Remote").
			Description("How this vault reaches other machines").
			Value(&remoteType).
			Options(
				huh.NewOption("None — a folder synced by Drive/Dropbox/iCloud/Syncthing", string(config.RemoteNone)),
				huh.NewOption("Git repository", string(config.RemoteGit)),
				huh.NewOption("Rclone remote (e.g. Google Drive)", string(config.RemoteRclone)),
			),
	).Title("Remote")

	groupGit := huh.NewGroup(
		huh.NewInput().Title("Git remote URL").Value(&gitURL).Validate(requiredField("git remote URL")),
		huh.NewInput().Title("Branch").Placeholder(config.DefaultBranch).Value(&gitBranch),
	).Title("Git remote").WithHideFunc(func() bool { return remoteType != string(config.RemoteGit) })

	groupRclone := huh.NewGroup(
		huh.NewInput().
			Title("Rclone remote name").
			Description("As configured with `rclone config`").
			Value(&rcloneRemote).
			Validate(requiredField("rclone remote name")),
		huh.NewInput().Title("Path on the remote").Value(&rclonePath),
	).Title("Rclone remote").WithHideFunc(func() bool { return remoteType != string(config.RemoteRclone) })

	groupKey := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Passphrase source").
			Value(&keySource).
			Options(
				huh.NewOption("Prompt at every launch", string(config.KeyPrompt)),
				huh.NewOption("File on disk", string(config.KeyFile)),
				huh.NewOption("Bitwarden", string(config.KeyBitwarden)),
			),
	).Title("Encryption key")

	groupKeyFile := huh.NewGroup(
		huh.NewInput().
			Title("Key file path").
			Description("Created for you with a random passphrase if it does not exist yet").
			Value(&keyFilePath).
			Validate(requiredField("key file path")),
	).Title("Key file").WithHideFunc(func() bool { return keySource != string(config.KeyFile) })

	groupBitwarden := huh.NewGroup(
		huh.NewInput().Title("Bitwarden item").Value(&bwItem).Validate(requiredField("Bitwarden item")),
		huh.NewInput().
			Title("Bitwarden field").
			Description("password, notes, or a custom field name").
			Placeholder(config.DefaultBitwardenField).
			Value(&bwField),
	).Title("Bitwarden").WithHideFunc(func() bool { return keySource != string(config.KeyBitwarden) })

	// bindings ties the review note's DescriptionFunc to every field it
	// reads. huh's Eval.shouldUpdate only recomputes a *Func value when
	// hashstructure.Hash(bindings) changes between renders; a constant
	// string here would hash the same forever; hashstructure dereferences
	// pointers (as in huh's own "&md" example), so a struct of pointers to
	// the live locals makes the hash track every keystroke and the note
	// re-render on every field the confirm screen actually summarises.
	bindings := &struct {
		VaultPath, MachineName, RemoteType, GitURL, GitBranch, RcloneRemote, RclonePath, KeySource, KeyFilePath, BwItem, BwField *string
	}{&vaultPath, &machineName, &remoteType, &gitURL, &gitBranch, &rcloneRemote, &rclonePath, &keySource, &keyFilePath, &bwItem, &bwField}

	groupConfirm := huh.NewGroup(
		huh.NewNote().
			Title("Review").
			DescriptionFunc(func() string {
				return setupSummary(vaultPath, machineName, remoteType, gitURL, gitBranch, rcloneRemote, rclonePath, keySource, keyFilePath, bwItem, bwField)
			}, bindings),
		huh.NewConfirm().
			Title("Save this configuration?").
			Affirmative("Save").
			Negative("Cancel").
			Value(&confirmed),
	).Title("Confirm")

	form := huh.NewForm(
		groupVault,
		groupRemote,
		groupGit,
		groupRclone,
		groupKey,
		groupKeyFile,
		groupBitwarden,
		groupConfirm,
	).WithTheme(huh.ThemeCharm())

	runErr := form.RunWithContext(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if runErr != nil {
		if errors.Is(runErr, huh.ErrUserAborted) {
			return nil, fmt.Errorf("setup cancelled: %w", runErr)
		}
		return nil, fmt.Errorf("setup: %w", runErr)
	}
	if !confirmed {
		return nil, errors.New("setup cancelled: not confirmed")
	}

	out := &config.Config{
		Version: config.CurrentVersion,
		Machine: config.MachineConfig{Name: strings.TrimSpace(machineName)},
		Vault: config.VaultConfig{
			Path: strings.TrimSpace(vaultPath),
			Remote: config.RemoteConfig{
				Type: config.RemoteType(remoteType),
				Git: config.GitRemote{
					URL:    strings.TrimSpace(gitURL),
					Branch: strings.TrimSpace(gitBranch),
				},
				Rclone: config.RcloneRemote{
					Remote: strings.TrimSpace(rcloneRemote),
					Path:   strings.TrimSpace(rclonePath),
				},
			},
		},
		Key: config.KeyConfig{
			Source:    config.KeySource(keySource),
			File:      config.FileKey{Path: strings.TrimSpace(keyFilePath)},
			Bitwarden: config.BitwardenKey{Item: strings.TrimSpace(bwItem), Field: strings.TrimSpace(bwField)},
		},
		// Scan defaults and existing project mappings are untouched by setup.
		Scan:     base.Scan,
		Projects: base.Projects,
	}
	if out.Vault.Remote.Git.Branch == "" {
		out.Vault.Remote.Git.Branch = config.DefaultBranch
	}
	if out.Key.Bitwarden.Field == "" {
		out.Key.Bitwarden.Field = config.DefaultBitwardenField
	}
	return out, nil
}

// setupDefaults seeds the wizard's starting values: the existing config when
// there is one, otherwise config.Default(dirs).
func setupDefaults(dirs paths.Dirs, existing *config.Config) *config.Config {
	if existing != nil {
		cp := *existing
		return &cp
	}
	return config.Default(dirs)
}

// requiredField rejects a blank value with a message naming the field.
func requiredField(label string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}
}

// setupSummary renders the final review note before the confirm prompt.
func setupSummary(vaultPath, machineName, remoteType, gitURL, gitBranch, rcloneRemote, rclonePath, keySource, keyFilePath, bwItem, bwField string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Vault:   %s\n", orPlaceholder(vaultPath, "(not set)"))
	fmt.Fprintf(&b, "Machine: %s\n", orPlaceholder(machineName, "(not set)"))
	switch config.RemoteType(remoteType) {
	case config.RemoteGit:
		fmt.Fprintf(&b, "Remote:  git %s (branch %s)\n", orPlaceholder(gitURL, "(not set)"), orPlaceholder(gitBranch, config.DefaultBranch))
	case config.RemoteRclone:
		fmt.Fprintf(&b, "Remote:  rclone %s:%s\n", orPlaceholder(rcloneRemote, "(not set)"), rclonePath)
	default:
		b.WriteString("Remote:  none (folder sync)\n")
	}
	switch config.KeySource(keySource) {
	case config.KeyFile:
		fmt.Fprintf(&b, "Key:     file %s\n", orPlaceholder(keyFilePath, "(not set)"))
	case config.KeyBitwarden:
		fmt.Fprintf(&b, "Key:     Bitwarden item %q, field %s\n", bwItem, orPlaceholder(bwField, config.DefaultBitwardenField))
	default:
		b.WriteString("Key:     prompt at launch\n")
	}
	return b.String()
}

// orPlaceholder returns def when s is blank.
func orPlaceholder(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
