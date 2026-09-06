# AGENTS.md

## Project overview

`private-sync` is a Go CLI + TUI (module `github.com/sanbiv/private-sync`) that keeps a project's config and secret files in an encrypted vault and synchronises that vault across machines over git or rclone. The data model is a per-machine, append-only one: every machine writes only its own journal, meta and machine documents, content is stored as deterministic content-addressed encrypted blobs, and per-path version vectors (`vault.Clock`, a `map[machineID]uint64`) decide which version is the head and when two machines have genuinely diverged. The stack is cobra for the CLI, the Charm stack (bubbletea, bubbles, huh, lipgloss) for the TUI, `golang.org/x/crypto` for Argon2id + XChaCha20-Poly1305, and `gopkg.in/yaml.v3` for the config. All logic lives under `internal/`; `cmd/private-sync/main.go` is 14 lines.

Design spec: `docs/superpowers/specs/2026-09-05-private-sync-design.md`. Package comments cite it as `spec §N` (134 references). README.md is the human-facing usage guide.

## Setup and build

Go 1.27.0 is required and matches `go.mod` exactly. There is no `toolchain` directive, no `vendor/`, no CI, and no linter config — do not add any.

```bash
go version                       # go1.27.0 darwin/arm64
make build                       # go build -o bin/private-sync ./cmd/private-sync  (~0.4s, 9.7MB)
make install                     # go install ./cmd/private-sync
make test                        # go test ./...   (no -count=1)
make vet                         # go vet ./...
make run                         # build, then ./bin/private-sync
make clean                       # rm -rf bin
```

`/bin/` is gitignored. `make lint` is a **silent no-op**: `lint` is listed in `.PHONY` but has no recipe, so it prints `make: Nothing to be done for 'lint'.` and exits 0 — its success means nothing, and there is no linter here. `build`, `install`, `test`, `vet`, `run` and `clean` are the only real targets; there are no `fmt`, `race` or `tidy` ones — run those as raw `go` commands.

## Development workflow

The whole quality gate is local. Nothing external will catch a regression, so run all four after every edit; the first three are essentially free and all are clean today.

```bash
gofmt -l .                       # 0.05s — must print nothing
go vet ./...                     # 0.2s  — must exit 0 with no output
go build ./...                   # 0.4s
go test ./... -count=1           # 32s   — 15 packages ok + 3 `[no test files]` (18 total)
```

`go list ./...` returns 18 packages: 17 under `internal/` plus `cmd/private-sync`. `cmd/private-sync`, `internal/execx` and `internal/paths` have no test files, so a clean run prints 15 `ok` lines and 3 `?  … [no test files]` lines.

Before handing work back, also run the race build (green today, ~1m10s):

```bash
go test ./... -race -count=1
```

`go mod tidy` currently produces one unrelated line of churn: `golang.org/x/sys v0.47.0` moves from the indirect block to the direct block. Verify with `go mod tidy -diff` — **it exits 1 whenever it prints a diff**, so a nonzero status here is the expected state, not a broken gate. Either expect that line in your diff or leave `go.mod` alone.

## Testing

616 `func Test…` functions, standard library only — no testify, no assertion helpers. Tests are table-driven (`for _, tc := range tests` + `t.Run(tc.name, …)`), build every fixture under `t.TempDir()`, and use `t.Setenv`/`t.Chdir` instead of touching the real HOME. Only 6 files call `t.Parallel()`.

```bash
go test ./internal/vault/ -count=1                                  # ~4s standalone (~19s inside ./...)
go test ./internal/vault/ -count=1 -run TestCreateOpenRoundTrip     # 0.2s
go test ./internal/sync/ -count=1 -run TestDecisionTable -v
```

**Silent-pass trap.** A `-run` pattern that matches nothing exits 0 and prints `PASS`. The only signal is the `[no tests to run]` marker on the `ok` line — always check for it before believing a targeted test went green.

```bash
go test ./internal/vault/ -count=1 -run TestNoSuchName
# ok  github.com/sanbiv/private-sync/internal/vault  0.1…s [no tests to run]   <- NOT a pass
```

Conventions to follow when adding tests:

- **Fake the runner.** Subprocesses are stubbed with `execx.Fake(func(c execx.Cmd) (execx.Result, error){…})`. Packages that must never spawn anything install a runner that errors on every call (`internal/tui/helpers_test.go` `failRunner`).
- **Skip, don't fail, when a binary is missing.** Tests in `internal/remote` and `internal/identity` shell out to real `git` and guard with `execx.LookPath`/`exec.LookPath` + `t.Skip("git not installed")`. A green run on a machine without git therefore proves nothing about the git remote.
- **Isolate the environment** with the existing helpers: `isolate` (`internal/cli/cli_test.go:120`) repoints `XDG_CONFIG_HOME`/`XDG_STATE_HOME`/`XDG_DATA_HOME` at a fresh `t.TempDir()` and unsets `PRIVATE_SYNC_CONFIG`, `PRIVATE_SYNC_PASSPHRASE`, `NO_COLOR`, `VISUAL`, `EDITOR`; `useMachine` (`internal/cli/e2e_test.go:482`) then repoints `XDG_CONFIG_HOME` and `XDG_STATE_HOME` only (not `XDG_DATA_HOME`) per simulated machine. Git-touching tests additionally pin `HOME`, `GIT_CONFIG_GLOBAL=os.DevNull`, `GIT_CONFIG_NOSYSTEM=1` (`remote/helpers_test.go:197-203`).
- **Lower the KDF cost.** Production defaults are Argon2id time=3, memory=64MiB, threads=4 (`internal/crypto/crypto.go:57-59`); `crypto.KDFMinMemory` is at `crypto.go:51`. Tests set `Memory: crypto.KDFMinMemory` (8MiB) — see `internal/vault/vault_test.go:46`, `internal/app/app_test.go:30`. A test that calls `crypto.DefaultKDFParams()` unmodified adds seconds per vault creation.
- **Drive the TUI through `Update`.** No test constructs a `tea.Program` (production has exactly three, at `dashboard.go:61`, `resolver.go:702`, `addproject.go:1163`). Tests build the model, call `m.Update(tea.KeyMsg{…})` / `m.View()`, and execute a returned `tea.Cmd` by calling it. Screen models return `(bool, tea.Cmd)` (dashboard returns a `dashAction`) precisely so they can be driven this way. New TUI logic must be reachable from a plain `Update` call.
- **Regression tests go in round files.** Bug fixes land in `*_fixes_test.go`, `*_review_test.go`, `*_regress_test.go`, `*_recovery_test.go`, `*round3_test.go`, `*round4_test.go` (14 files today). Most of them carry a `// Regression tests for the Nth review round…` banner placed *after* the import block, not at the top of the file — 8 do (`state/state_review_test.go:19`, `sync/{fixes_test.go:17, review_test.go:15, round3_test.go:14, round4_test.go:10}`, `vault/{vault_fixes_test.go:17, vault_regress_test.go:21, vault_round4_test.go:12}`); follow that convention in new ones. Every test carries a comment naming the exact bug it pins. Do not fold them into the package's main `*_test.go`, and do not "deduplicate" them.

## Architecture

The internal import graph is strictly layered and acyclic. A new dependency may only point **down** this table.

| Package | Imports (internal) | Role |
|---|---|---|
| `paths` | — | The **only** package allowed to read HOME / `XDG_*`. `Dirs`, `ExpandHome`, `ContractHome` |
| `execx` | — | Sanitised subprocess runner: `Cmd`, `Runner`, `Real()`, `Fake()`, `LookPath`, `Denylist()` |
| `fsutil` | — | `WriteFileAtomic`, temp-file cleanup |
| `ui` | — | Two-method `Prompter{Password, Confirm}` + `Silent`, so core packages can ask for secrets without a front end |
| `crypto` | — | Key hierarchy + the PSV1 on-disk format |
| `merge` | — | Three-way merge (spec §8): per-key for dotenv (`KindDotenv`, `dotenv.go`), diff3 over Myers for text |
| `config` | fsutil, paths | `config.yaml`; stores paths verbatim |
| `identity` | execx | Project fingerprints (`Detect`, `Union`) |
| `scan` | execx, fsutil | Candidate-file scoring |
| `remote` | config, execx | `Remote` interface, git and rclone transports, `OwnFiles` |
| `vault` | crypto, fsutil, **identity** | vault.json, blobs, journals, meta, machines, `ResolveHeads` |
| `state` | crypto, fsutil, vault | machine.json, vault pins, sync base store, encrypted trash |
| `keysource` | config, execx, paths, ui | Passphrase acquisition (prompt / env / file / Bitwarden) |
| `sync` | config, fsutil, identity, merge, remote, state, vault | The engine. **No UI, no flags, no env** |
| `app` | 12 packages | The facade both front ends share: `App` (`Load`) and `Session` (`Open`/`Setup`) |
| `cli` | 13 packages incl. app | cobra commands, exit-code classification |
| `tui` | 12 packages incl. app | Bubble Tea screens |
| `cmd/private-sync` | cli, tui | `os.Exit(cli.Main(os.Args[1:], tui.Frontend{}))` |

Rules that follow from the graph:

- **Leaves stay leaves.** Pulling `config`/`vault`/`state` into `paths`, `execx`, `fsutil`, `ui`, `crypto` or `merge` would force nearly the whole tree to depend on it and in most cases creates a cycle.
- **`state` depends on `vault`, never the reverse.** Local per-machine state may reference vault types; vault must stay ignorant of the state directory.
- **`vault` depends on `identity`** because `ProjectMeta.Fingerprints` / `Project.Fingerprints` are `[]identity.Fingerprint` (`vault.go:253,261`) and vault calls `identity.Union` when merging per-machine metadata (`vault.go:1458`). Changing `identity.Fingerprint` changes the on-disk encrypted metadata format.
- **`sync` is UI-free and env-free.** A feature that needs a user decision inside the engine must be a `sync.Options` field or a `sync.Resolutions` entry supplied by cli/tui — never a prompt or an env read inside `sync`. Everything the engine may touch is injected: `sync.New(v *vault.Vault, st *state.Store, cfg *config.Config, r remote.Remote, m state.Machine) *Engine` (`sync.go:382`). It runs no subprocesses; git/rclone happen behind the injected `remote.Remote`.
- **`cli` never imports `tui`.** The only channel is `cli.Frontend` (`cli.go:30-34`): `RunSetup`, `Run`, `RunAddProject`, `ResolveConflicts`. `tui.Frontend` is a zero-size struct implementing exactly those four. A new interactive screen the CLI must launch = a new method on that interface + an implementation in tui. `main.go` is the single composition root; no logic belongs there.
- **Shared wiring belongs in `app`.** Opening the vault, obtaining the key, linking projects — duplicating that in cli or tui is how the two front ends drift.
- The spec says `internal/tui` "uses only app/sync/merge/scan/identity types", but the real graph shows tui importing `crypto`, `execx`, `fsutil`, `keysource`, `vault`, `config` and `paths` directly. When adding a screen, go through `app.Session` as the spec intends; the existing direct imports are not the pattern to follow.

## Code style

**Errors.** Each package exports documented `errors.New` sentinels for failure modes callers branch on (~40 across vault, app, config, sync, state, crypto, keysource, remote, fsutil), grouped at the top of the file; unexported lowercase sentinels when only the package branches on them (`errUnresolved`, `errAborted`, `errNoConfig` in cli). Wrap with `fmt.Errorf` and `%w` (368 of 433 non-test `fmt.Errorf` calls) behind a short lowercase prefix — either `pkg.Func:` (`vault.Create:`, `sync.Apply:`, `crypto.SealDoc:`) or a plain phrase (`open vault:`, `load config %s:`). `%w: %w` carries both a sentinel and the cause. Match whatever form the file already uses.

**User-facing text.** There is no separate message layer: `internal/cli/cli.go:285` prints `error: <err.Error()>` on stderr, so the string you write in a deep package is what the user reads. Lowercase, no trailing period, and append a remedy after a semicolon or colon where one exists (`"…: unset PRIVATE_SYNC_PASSPHRASE, or write that passphrase to the file (chmod 600) and re-run init"`).

**Exit codes** are a fixed contract (`cli.go:37-42`): `0` ok, `1` any error, `2` bad flags/arguments, `3` unresolved conflicts or user abort. `exitCode` classifies with `errors.As`/`errors.Is`, never string matching. A new terminal condition must map onto an existing sentinel (or a new one) — a plain error silently means exit 1.

**Subprocesses** go through `execx.Runner`, taken from the caller (`Options.Runner` / `App.Runner`), never `os/exec`. `Cmd.Stdin` is nil ("children never see the terminal"). The only two direct `exec.Command` calls in non-test code are the `$EDITOR` launches at `internal/cli/cmd_root.go:355` and `internal/tui/resolver.go:514`, which need the real terminal and still set `cmd.Env = execx.SanitizedEnv(nil)`.

**Atomic writes.** Every persistent file goes through `fsutil.WriteFileAtomic(path, data, mode, suffix)` (12 non-test call sites), which writes `<target>.psv-tmp-<suffix>-<unique>`, fsyncs, chmods, renames, then fsyncs the directory. The suffix identifies the writer (first 8 chars of the machine id, or `cfg-<pid>-<seq>` / `state-<pid>`). Never `os.WriteFile` for anything that must survive a crash or a concurrent writer.

**Modes.** Files 0600, directories 0700, never relying on umask. The single exception is `os.MkdirAll(filepath.Dir(target), 0o755)` at `internal/cli/cmd_files.go:485`, the parent of a restored file inside the user's own project tree.

**Paths.** Vault-internal and journal paths are canonical relative **slash-separated** strings built with `path.Join`, converted with `filepath.FromSlash` only where they touch disk, and validated by `checkDocPath`/`checkEntryPath`/`checkRelPath` (`vault.go:1157-1183`), which reject empty, absolute, backslash, NUL and `.`/`..` elements. The document path is the AEAD AAD, so building one with `filepath.Join` produces documents a Windows machine seals and a macOS machine cannot open; skipping the validator lets `../x` escape the project root. Never call `os.UserHomeDir` or read `XDG_*` outside `internal/paths`. Config stores the contracted `~/...` form; read it back through `(*Config).VaultPath()`, `KeyFilePath()`, `MaxFileSize()`, `ProjectPath(id)` — the raw fields skip `paths.ExpandHome`.

**Doc comments.** 362 exported funcs/types, 48 undocumented — all of them trivial interface implementations (`Error`, `View`, `Init`, `Update`, `Name`, `SetSize`, `Passphrase`, `Unwrap`, `Run`, `Prepare`/`Push`/`Fetch`, `Password`/`Confirm`) plus three one-line accessors. Every new exported symbol needs a full-sentence comment starting with its name; cite `spec §N` when documenting a non-obvious rule.

## Invariants that must not break

| Invariant | Enforced by | First test to fail |
|---|---|---|
| A machine writes only its own machine/meta/journal files; blobs are the only shared files. This is what keeps git/Drive/rclone merges clean. | `vault.go:1498,1637,1725` reject foreign writes with `ErrForeignMachine` (`vault.go:62`) | `TestProjectMetaMerge` (vault_test.go:802), `TestJournals`, `TestMachines` |
| The transport mirrors that rule: rclone `Fetch` excludes `remote.OwnFiles(machineID)` so it can never overwrite this machine's own files; `Push` copies blobs + own files only, never `rclone sync`, never a delete. | `remote.go:108-114`, `rclone.go:273-277,306-314` | `TestRcloneFetchArgs` (rclone_test.go:94), `TestRclonePushBlobsFirstThenOwnFiles` |
| Heads are decided by per-path version vectors, not counters: `resolveHead` drops only candidates strictly `Before` another; survivors disagreeing on (Kind, Blob) are a concurrent head with `Entry == nil`. A scalar counter would make `{A:1,B:1}` vs `{A:3}` look ordered. | `vault.go:133-190`, `vault.go:350-404` | `TestClockCompare` (vault_test.go:1299), `TestResolveHeads` (vault_test.go:1421) |
| `KindUntracked` wins head resolution, and `ActionUntrack` only drops the local base — it never touches the file on disk. | `vault.go:390-391,398-399` (the `case untracked >= 0:` branch of `resolveHead`), `sync/apply.go:240-243` | `TestDecisionTable/row1…` (sync/plan_test.go:28) |
| Blobs are deterministic: `blobID = hex(HMAC-SHA256(mac, plaintext))`, nonce derived from the plaintext, AAD `"blob:"+id`. Identical plaintext on any machine yields a byte-identical `.enc`; `WriteBlob` dedupes and never rewrites. A random nonce would create duplicate blobs and add/add conflicts. | `crypto.go:259-290`, `vault.go:1020-1051` | `TestSealBlobDeterministic` (crypto_test.go:165), `TestBlobs` (vault_test.go:589) |
| The on-disk crypto format is frozen by known-answer vectors (subkeys, blob id, full ciphertext hex, KEK, wrapped key, project id). A failing KAT means the format drifted — **do not update the constants to make it pass**. | `internal/crypto/crypto_test.go:13-31` | `TestKnownAnswer` (crypto_test.go:84), `TestKnownAnswerKEK`, `TestSubkeysMatchHKDFDefinition` |
| `vault.json` is fully validated before the KDF runs, and `DeriveKEK` re-validates and returns `ErrKDFParams` without running Argon2id. A tampered `memory = 1<<31` KiB would otherwise OOM before any error surfaced. | `vault.go:667` precedes `vault.go:670`; `crypto.go:110-142` | `TestOpenTamperedVaultFile` (vault_test.go:292 — fails on wrong error *and* on elapsed > 1s), `TestDeriveKEKValidatesFirst` |
| Local deletions never propagate without `--delete`: row 13 yields `ActionMissingLocal` (report only); with `PropagateDeletes` it becomes `ActionDeleteRemote` and still carries `NeedsResolution = true`, so `--yes` alone cannot write a tombstone. | `sync/plan.go:541-551` | `TestDeletePropagationNonInteractive` (cli/regress_test.go:20) |
| The pre-image reaches the encrypted trash before any local file is overwritten or removed; a `TrashPut` failure aborts the item and keeps the local file. The project directory is often the only plaintext copy. | `sync/apply.go:441` `trashPreimage`, called at 509, 576, 717 before `writeLocal`/`os.Remove` | `TestConflictWritesBlobBeforeLocalFile` (sync/review_test.go:380), `TestFilesDeleteKeepsLocalWhenTrashFails` (cli/fixes_round4_test.go:44) |
| Plaintext exists only inside project directories. `state.TrashPut` seals the pre-image with the deterministic blob format before writing; the state dir is 0700/0600 and `base.json` holds only blob ids, kinds and clocks. | `state.go:73-74` and the `TrashPut` body | `TestTrashPutListRead` (state_test.go:577 — asserts no plaintext in the trash blob, and 0600/0700 on blob, shard and index.json) |
| An older head is never applied silently: `rollbackCheck` turns a download into `ActionRollback` when the head clock is `Before` or `Concurrent` with the recorded base clock (unless `--accept-rollback` or restore mode), and a journal whose `Seq` is below the highest seen raises a rollback warning. | `sync/plan.go` `rollbackCheck`, `plan.go:160-163` | `TestRollbackByClock` (plan_test.go:833), `TestRollbackByJournalSequence` (plan_test.go:774) |
| `Apply` re-reads the local file and refuses stale writes: `checkStale` compares existence and BlobID against what `Plan` saw and returns `ErrStale`. Plan and Apply are separated by a resolver the user can sit in for minutes. | `sync/apply.go:284` `checkStale`, called at 415, 484, 573, 617, 703 | `TestErrStale` (sync/plan_test.go:1156) |
| Children never inherit `PRIVATE_SYNC_PASSPHRASE` or `BW_*`. `execx.Denylist()` names those five, and every child gets `SanitizedEnv`. Git hooks, rclone backends, editor plugins and LSP servers all run as children. | `execx.go:64-86`, `execx.go:93` | `TestEditorCommandSanitisesEnvironment` (tui/resolver_test.go:668) |
| Git never prompts: `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=`, `SSH_ASKPASS=`, `GCM_INTERACTIVE=never`, `LC_ALL=C`, a `GIT_SSH_COMMAND` forced to `-o BatchMode=yes` with existing BatchMode options stripped first (ssh honours the *first* value), plus pinned `GIT_DIR`/`GIT_WORK_TREE`/`GIT_INDEX_FILE`/`GIT_COMMON_DIR`/`GIT_OBJECT_DIRECTORY` and an **empty** `GIT_ALTERNATE_OBJECT_DIRECTORIES` — the last two are the defence against receive-pack's quarantine (or an outer git) redirecting writes into another repo's object store — and nil stdin. | `gitEnv` + `(*gitRemote).env` (`remote/git.go:158-203`) | `TestGitCmdEnvAndArgs` (remote/git_fake_test.go:33) |
| The three default scan lists have pinned sizes: 37 include globs, 36 exclude dirs, 20 exclude files, matching the spec enumeration. | `scan.go:635-667` | `TestDefaults` (scan/scan_test.go:218) |
| A blob that cannot be read yet makes the item `ActionPending` — `Apply` counts it and returns, leaving the local file **and the recorded base** untouched, so the next run re-plans it. Turning a pending item into a skip that still advances the base would lose the update silently. | `sync/plan.go:678-685`, `sync/apply.go:234-236` (`sync.go:67`) | `TestDecisionTable` rows for `ActionPending` (sync/plan_test.go:28) |
| The original file mode round-trips: `TrashPut` records it (`state.TrashEntry.Mode`, `state.go:712`) and restore reapplies it, and `Apply` reconciles the head's `Mode` against the file on disk instead of recreating it with a default. | `sync/apply.go:381-384,447-463,503` `reconcileMode`; `cli/cmd_files.go:481-488` | `TestTrashPutListRead` (state_test.go:577) |
| `Apply` clears this machine's own stale temp files before it writes anything (`fsutil.CleanupTemp(vaultDir, suffix)` at the top of the run, plus `state.removeStaleTemp` when the store opens). A crashed earlier run must not leave `.psv-tmp-*` files that a later dedupe or listing mistakes for content. | `sync/apply.go:43`, `fsutil.go:106`, `state.go:110-117,252,461` | `TestCleanupTempMatchesUniqueNames` (fsutil_test.go:183), `TestTempSuffixIsWriterUnique` (state_fixes_test.go:122) |

## Pinned decisions (spec §14)

Spec §14 lists six decisions taken during implementation; it calls them "part of the contract; the tests pin them". Read it before touching the code below — these are exactly the rules that look like cleanup candidates.

- **Vault-tracked wins pre-selection** (§6, §14). `c.Preselected = c.Score == ScoreHigh || c.Tracked || (c.Score == ScoreMedium && c.SecretName)` (`scan.go:339`): a file already tracked in the vault is always pre-selected, beating "git-tracked ⇒ never pre-selected" (`ScoreLow`, `scan.go:41`). Vault-tracked files are also appended as candidates even when they match no include glob (`appendVaultTracked`, `scan.go:286,378`). Re-scanning must never silently drop a file already under sync.
- **`exclude_dirs` is case-insensitive and `.git` is unconditional** (§6, §14). The set is lowercased on build and lookup (`scan.go:154,194`) to match the default macOS filesystem, and `.git` is skipped by name (`gitDirName`, `scan.go:22`) even when a custom `scan.exclude_dirs` replaces the defaults — the comment at `scan.go:189` spells out why relying on `DefaultExcludeDirs()` to carry `.git` is not enough.
- **`rclone.path` keeps its leading `/`** (§10, §14). `strings.TrimRight(…, "/")` at `rclone.go:122` — `TrimRight`, not `Trim` — so an absolute remote path stays absolute; a relative one resolves against the backend's own root. This has its own commit (`b4d6e4a`). Note the field comment at `rclone.go:95` still says "no leading/trailing slash" and is stale; the code and `internal/remote/rclone_test.go` are the contract.

The other three §14 decisions — terminal background, deletion prompts, and the `passphrase change` warning — are covered under Gotchas and in the invariants table.

## Environment and manual runs

**A manual run touches the user's real vault.** The config resolves from `--config`, then `$PRIVATE_SYNC_CONFIG`, then `~/.config/private-sync/config.yaml`. Always redirect it first:

```bash
S=$(mktemp -d)
env HOME=$S XDG_CONFIG_HOME=$S/config XDG_STATE_HOME=$S/state \
    PRIVATE_SYNC_CONFIG=$S/config/private-sync/config.yaml \
    ./bin/private-sync status </dev/null
# error: no configuration found: run `private-sync init` first
```

`scan` and `status` are the safe read-only probes — `scan` needs neither a key nor a config, and neither contacts the remote **unless** you pass `status --fetch` (`cmd_sync.go:46`), which does fetch. `sync`, `push`, `pull`, `restore` and `init` run `git fetch` + `git push` unless `--no-remote` is passed.

```bash
./bin/private-sync scan . </dev/null          # exit 0, no config and no key needed
./bin/private-sync --help </dev/null          # exit 0
./bin/private-sync </dev/null                 # exit 1: …could not open a new TTY…
```

| Variable | Read by | Notes |
|---|---|---|
| `PRIVATE_SYNC_CONFIG` | `cli.go:47`, resolved at `cli.go:455` | `--config` wins over it |
| `PRIVATE_SYNC_PASSPHRASE` | `keysource.go:27` | Captured into memory and `os.Unsetenv`-ed at startup (`CaptureEnv`, keysource.go:63); never readable via `os.Getenv` afterwards, never inherited by children |
| `PRIVATE_SYNC_BACKGROUND` | `tui/background.go:13` | `dark`\|`light`; pins the palette instead of querying the terminal. Only effective inside the four `initBackground()` call sites (setup, dashboard, addproject, resolver) |
| `BW_SESSION` | `keysource.go:30` | On the execx denylist |
| `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_DATA_HOME` | `paths.go:28,32,51` | The only place `XDG_*`/HOME may be read |
| `EDITOR`, `VISUAL`, `TERM`, `NO_COLOR` | `cmd_root.go:351`, `cli.go:360`, `tui` | |
| `GIT_SSH_COMMAND` | `remote/git.go:165` | Reused, with BatchMode forced |

## Gotchas

- **No TTY, no TUI.** Bare `private-sync`, `init` without an existing config, and `add` all die instantly with exit 1 and `error: [setup: huh: ]could not open a new TTY: open /dev/tty: device not configured`. The `setup: huh: ` prefix is present for bare and `init` (both go through the setup wizard, which wraps with `setup: %w` at `tui/setup.go:160`) and absent for `add`, whose `RunAddProject` (`tui/addproject.go:1151`) returns the program error unwrapped. Grep for the fragment `could not open a new TTY`, not the whole line. It is not a hang — drive subcommands, `--yes` and `--json` instead.
- **`init --yes` is not a substitute for the wizard.** It goes straight to vault setup only if a complete config file is already on disk (`cmd_root.go:165`). On a first run with no config it still opens the wizard and fails without a terminal. Bootstrapping headlessly means writing `config.yaml` yourself first.
- **There is no CLI path to create a project.** for an unknown project `files add` fails with `app.ErrNotLinked` ("project is not linked on this machine", `app.go:38`) and `projects link` with `vault.ErrNoProject` ("project not found in vault", `vault.go:44`). The first project must come from the `add` wizard, which needs a terminal; after that another machine can `projects link`.
- **A silent terminal costs 5s at process start, on every command.** Bubble Tea's package `init()` calls `lipgloss.HasDarkBackground()` (`bubbletea@v1.3.10/tea_init.go`), so the OSC 11 background query is issued before `main` runs — `--help` included. A terminal that answers replies in milliseconds (0.01s measured); one that stays silent costs termenv's full `OSCTimeout` (5.04s measured under a non-answering pty). `PRIVATE_SYNC_BACKGROUND` does **not** shorten it (5.02s): it only overrides the cached value afterwards, and `internal/tui/background.go` says so. termenv skips the query when stdio is not a TTY, when the process is in the background, and when `TERM` is `dumb` or starts with `screen`/`tmux` (`termenv_unix.go:234-240`) — those are the only escapes. Budget 5s before concluding a pty harness is broken; `TERM=dumb` makes it instant.
- **`passphrase change` refuses while `PRIVATE_SYNC_PASSPHRASE` was captured** (`cmd_root.go:487` `errEnvPassphraseRekey`, `tui/settings.go:263`), because the captured value outranks the configured key source and a rekey would strand it. This is deliberate, not a bug.
- **A green `internal/remote` run without git installed proves nothing** — a large slice of it skips itself.
- Tests outnumber source, 29,773 lines to 21,350. The review-round files and the KAT constants are pinned coverage; treat a failure there as a real regression, not a stale expectation.

## Pull request / commit guidelines

Commit subjects follow `type(scope): summary` with a lowercase imperative summary and no trailing period. Types in use: `feat`, `fix`, `docs`, `test`, `chore`. Scopes are package names — `vault`, `cli`, `tui`, `scan`, `app`, `remote` — and are omitted for cross-cutting changes (`fix: wire the cross-package leftovers from the review`).

The body is optional and explains *why*, in prose paragraphs, not a bullet list of what changed. Example from `feat(cli): make init scriptable and quiet its transport chatter`: it explains that a complete config already answers everything the wizard asks, that the wizard would only fail without a terminal, and that this blocked provisioning a second machine from a script.

End every new commit with the trailer:

```
Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
```

This is a rule for new work, not a description of the history: of the 18 commits in the repo only the 7 most recent carry it, 5 carry `Claude Fable 5.1` and 6 carry no trailer at all. Do not rewrite them.

Before committing, `gofmt -l .`, `go vet ./...` and `go test ./... -count=1` must all be clean, and `go test ./... -race -count=1` for anything touching concurrency.
