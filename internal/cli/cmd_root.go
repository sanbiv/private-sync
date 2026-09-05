package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/execx"
	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// runRoot launches the dashboard; on the first run the setup wizard comes first.
func (c *cli) runRoot(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if c.fe == nil {
		return errors.New("no interactive front end available; use the subcommands (see --help)")
	}
	a, err := c.loadApp()
	switch {
	case err == nil:
	case errors.Is(err, app.ErrNoConfig):
		s, err := c.runSetup(ctx, nil)
		if err != nil {
			return err
		}
		defer s.Close()
		return c.fe.Run(ctx, s)
	default:
		return err
	}
	s, err := a.Open(ctx, c.prompter)
	if err != nil {
		return err
	}
	defer s.Close()
	s.Warn = c.warn
	return c.fe.Run(ctx, s)
}

// runSetup runs the wizard, saves the config and runs the vault creation
// protocol; the returned session is open.
func (c *cli) runSetup(ctx context.Context, existing *config.Config) (*app.Session, error) {
	if c.fe == nil {
		return nil, errors.New("the setup wizard needs an interactive front end")
	}
	dirs, err := c.dirs()
	if err != nil {
		return nil, err
	}
	path, err := c.configPath(dirs)
	if err != nil {
		return nil, err
	}
	cfg, err := c.fe.RunSetup(ctx, dirs, existing)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("%w: setup cancelled", errAborted)
	}
	if err := cfg.Save(path); err != nil {
		return nil, fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(c.out, "config written to %s\n", display(path))
	a, err := app.Load(path, dirs, c.runner)
	if err != nil {
		return nil, err
	}
	a.NewRemote = c.remoteFactory
	for _, w := range a.Config.Warnings() {
		c.warn(w)
	}
	log := func(line string) { fmt.Fprintln(c.out, line) }
	s, err := a.Setup(ctx, c.prompter, log)
	if err != nil {
		return nil, err
	}
	s.Warn = c.warn
	fmt.Fprintf(c.out, "vault %s ready at %s\n", s.Vault.ID(), display(s.Vault.Dir()))
	return s, nil
}

// initCommand: the setup wizard without the dashboard.
func (c *cli) initCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Run the setup wizard (config + vault create/open) without the dashboard",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var existing *config.Config
			a, err := c.loadApp()
			switch {
			case err == nil:
				existing = a.Config
			case errors.Is(err, app.ErrNoConfig):
			default:
				return err
			}
			s, err := c.runSetup(cmd.Context(), existing)
			if err != nil {
				return err
			}
			s.Close()
			return nil
		},
	}
}

// addCommand: the add-project wizard.
func (c *cli) addCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add <path>",
		Short: "Scan a directory, select files and associate/create the vault project",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if c.fe == nil {
				return errors.New("add needs an interactive front end")
			}
			dir, err := absDir(args[0])
			if err != nil {
				return err
			}
			s, err := c.openSession(ctx)
			if err != nil {
				return err
			}
			defer s.Close()
			if err := c.fetch(ctx, s); err != nil {
				return err
			}
			return c.fe.RunAddProject(ctx, s, dir)
		},
	}
}

// unlockCommand: test that the key can be obtained and the vault opens.
func (c *cli) unlockCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Check that the key can be obtained and the vault opens",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := c.openSession(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			if c.g.json {
				return c.printJSON(struct {
					Vault string `json:"vault"`
					Path  string `json:"path"`
				}{s.Vault.ID(), display(s.Vault.Dir())})
			}
			fmt.Fprintf(c.out, "vault %s opened\n", s.Vault.ID())
			return nil
		},
	}
}

// machineCommand prints this machine's id and name.
func (c *cli) machineCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "machine",
		Short: "Print this machine's id and name",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			dirs, err := c.dirs()
			if err != nil {
				return err
			}
			m, err := state.LoadMachine(dirs.State)
			if err != nil {
				return fmt.Errorf("machine identity: %w", err)
			}
			name := ""
			if cfg, _, err := c.loadConfig(); err == nil && cfg != nil {
				name = cfg.Machine.Name
			}
			if c.g.json {
				return c.printJSON(struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					CreatedAt string `json:"created_at"`
				}{m.ID, name, m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")})
			}
			c.table([][]string{{"id", m.ID}, {"name", name}})
			return nil
		},
	}
}

// configCommand groups edit | show | path.
func (c *cli) configCommand() *cobra.Command {
	cmd := groupCommand("config", "Edit or print the configuration")
	cmd.AddCommand(
		&cobra.Command{
			Use:   "path",
			Short: "Print the config file path",
			Args:  noArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				dirs, err := c.dirs()
				if err != nil {
					return err
				}
				path, err := c.configPath(dirs)
				if err != nil {
					return err
				}
				if c.g.json {
					return c.printJSON(struct {
						Path   string `json:"path"`
						Exists bool   `json:"exists"`
					}{path, fileExists(path)})
				}
				fmt.Fprintln(c.out, path)
				return nil
			},
		},
		&cobra.Command{
			Use:   "show",
			Short: "Print the config file (an annotated example when it does not exist)",
			Args:  noArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				dirs, err := c.dirs()
				if err != nil {
					return err
				}
				path, err := c.configPath(dirs)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						return err
					}
					fmt.Fprintf(c.errOut, "# %s does not exist; example configuration:\n", display(path))
					data = []byte(config.Example())
				}
				_, err = c.out.Write(data)
				if err == nil && !bytes.HasSuffix(data, []byte("\n")) {
					fmt.Fprintln(c.out)
				}
				return err
			},
		},
		&cobra.Command{
			Use:   "edit",
			Short: "Open the config in $VISUAL / $EDITOR (vi)",
			Args:  noArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				return c.editConfig()
			},
		},
	)
	return cmd
}

// editConfig opens the config in the user's editor and validates the result.
func (c *cli) editConfig() error {
	dirs, err := c.dirs()
	if err != nil {
		return err
	}
	path, err := c.configPath(dirs)
	if err != nil {
		return err
	}
	if !fileExists(path) {
		// Start from a valid default rather than an empty buffer.
		if err := config.Default(dirs).Save(path); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		fmt.Fprintf(c.errOut, "created %s with defaults\n", display(path))
	}
	argv := editorCommand(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	if _, ok := execx.LookPath(argv[0]); !ok {
		return fmt.Errorf("editor %q not found; set $EDITOR", argv[0])
	}
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = execx.SanitizedEnv(nil)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %s: %w", argv[0], err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("config is invalid after editing (fix it with `config edit`): %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config is invalid after editing (fix it with `config edit`): %w", err)
	}
	for _, w := range cfg.Warnings() {
		c.warn(w)
	}
	fmt.Fprintf(c.out, "config %s is valid\n", display(path))
	return nil
}

// editorCommand picks $VISUAL, then $EDITOR, then vi (notepad on Windows),
// splitting a value such as "code --wait" into argv.
func editorCommand(visual, editor string) []string {
	for _, v := range []string{visual, editor} {
		if argv := strings.Fields(v); len(argv) > 0 {
			return argv
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

// fileExists reports whether path exists (any type).
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// passphraseCommand groups passphrase change.
func (c *cli) passphraseCommand() *cobra.Command {
	cmd := groupCommand("passphrase", "Manage the vault passphrase")
	cmd.AddCommand(&cobra.Command{
		Use:   "change",
		Short: "Rewrap the vault key with a new passphrase (nothing is re-encrypted)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.changePassphrase(cmd.Context())
		},
	})
	return cmd
}

// changePassphrase asks for the new passphrase twice, rekeys and pushes vault.json.
func (c *cli) changePassphrase(ctx context.Context) error {
	s, err := c.openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := c.fetch(ctx, s); err != nil {
		return err
	}
	if c.prompter == nil {
		return errors.New("passphrase change needs a terminal")
	}
	first, err := c.prompter.Password(ctx, "New passphrase")
	if err != nil {
		return err
	}
	defer keysource.Zero(first)
	if len(bytes.TrimSpace(first)) == 0 {
		return errors.New("the new passphrase must not be empty")
	}
	second, err := c.prompter.Password(ctx, "Repeat new passphrase")
	if err != nil {
		return err
	}
	defer keysource.Zero(second)
	if !bytes.Equal(first, second) {
		return errors.New("passphrases do not match")
	}
	params, err := crypto.DefaultKDFParams()
	if err != nil {
		return fmt.Errorf("kdf parameters: %w", err)
	}
	// Rekey zeroes the buffer it receives; keep our own copy for the key file.
	if err := s.Vault.Rekey(append([]byte(nil), first...), params); err != nil {
		return err
	}
	// From here on the local vault.json only opens with the new passphrase:
	// the key source is updated first so that the next command works even
	// when the push below fails.
	keyFile := ""
	switch s.Config.Key.Source {
	case config.KeyFile:
		keyPath, _ := s.Config.KeyFilePath()
		written, err := updateKeyFile(keyPath, first)
		if err != nil {
			c.warn(fmt.Sprintf("key.source is file but the key file could not be updated (%v): write the new passphrase to %s (mode 0600) before the next run", err, display(keyPath)))
		} else {
			keyFile = written
			if !c.g.json {
				fmt.Fprintf(c.out, "key file %s updated\n", display(written))
			}
		}
	case config.KeyBitwarden:
		c.warn(fmt.Sprintf("key.source is bitwarden: update the item %q with the new passphrase", s.Config.Key.Bitwarden.Item))
	}
	pushed := false
	if !c.g.noRemote {
		log := func(line string) { fmt.Fprintf(c.errOut, "push: %s\n", line) }
		if err := s.Remote.Push(ctx, []string{vault.VaultFileName}, log); err != nil {
			return pushAfterRekeyError(err)
		}
		pushed = true
	}
	if c.g.json {
		return c.printJSON(struct {
			Vault   string `json:"vault"`
			KeyFile string `json:"key_file,omitempty"`
			Pushed  bool   `json:"pushed"`
		}{s.Vault.ID(), display(keyFile), pushed})
	}
	fmt.Fprintf(c.out, "passphrase changed for vault %s\n", s.Vault.ID())
	return nil
}

// pushAfterRekeyError explains a failed push of vault.json after a successful
// Rekey: the local copy already carries the new passphrase, so the other
// machines cannot open the vault until it reaches the remote.
func pushAfterRekeyError(err error) error {
	return fmt.Errorf("passphrase changed locally but the push of %s failed (%w); other machines cannot open the vault until it is pushed: run `private-sync passphrase change` again once the remote is reachable", vault.VaultFileName, err)
}

// updateKeyFile rewrites the key file (symlinks resolved) with passphrase:
// an atomic rename from a 0600 temp file in the same directory, so the file
// keysource reads is always complete. It returns the path written.
func updateKeyFile(path string, passphrase []byte) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("key.file.path is empty")
	}
	resolved, err := keysource.ResolveKeyFile(path)
	if err != nil {
		return "", err
	}
	data := make([]byte, 0, len(passphrase)+1)
	data = append(data, passphrase...)
	data = append(data, '\n')
	defer keysource.Zero(data)
	if err := fsutil.WriteFileAtomic(resolved, data, 0o600, "key"); err != nil {
		return "", err
	}
	return resolved, nil
}

// stateDirVaultID returns the vault id for the configured vault path without
// a passphrase (vault.json, else the pin), or "" when unknown.
func stateDirVaultID(dirs paths.Dirs, cfg *config.Config) string {
	vaultDir, err := cfg.VaultPath()
	if err != nil || vaultDir == "" {
		return ""
	}
	if id, err := vault.ReadID(vaultDir); err == nil && id != "" {
		return id
	}
	if id, ok, err := state.PinnedVault(dirs.State, filepath.Clean(vaultDir)); err == nil && ok {
		return id
	}
	return ""
}
