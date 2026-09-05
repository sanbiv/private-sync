# private-sync — design spec

Date: 2026-09-05. Status: **v2** (after adversarial critique: 39 findings, all addressed below).

## 1. Goal

A Go CLI + TUI (`private-sync`) that finds configuration/secret files inside configured
project directories (`.env`, `*.yaml`, `*.json`, keys, …), stores them **encrypted** in a
local *vault* directory, and synchronises that vault across several computers through
**git**, **rclone** (Google Drive) or any folder that is already synced by a desktop client.
Every machine has its own YAML config (different paths) and its own machine id. Projects
are matched across machines by *fingerprints* (git remote URL, `go.mod` module,
`package.json` name, …). Sync is three-way: local ↔ last-synced base ↔ vault head, with
automatic merge and interactive conflict resolution.

Non-goals (v1): Google Drive OAuth API client (rclone or a Drive-synced folder instead),
structural YAML/JSON merge (line based diff3 with whitespace tolerance instead), blob
garbage collection, several vaults per config, filesystem watching, rename detection,
automatic merge of duplicate vault projects (they are detected and reported).

## 2. User facing surface

Binary: `private-sync`. Module: `github.com/sanbiv/private-sync`. Go 1.27.
Libraries: cobra v1.10, Bubble Tea **v1.3**, Bubbles **v1.0**, Lip Gloss **v1.1**, Huh
**v1.0**, `gopkg.in/yaml.v3`, `golang.org/x/crypto` (argon2, chacha20poly1305, hkdf),
`golang.org/x/term`, `github.com/google/uuid`. No other third party code.

### 2.1 CLI (cobra)

| command | key? | network? | purpose |
|---|---|---|---|
| `private-sync` | yes | no (fetch on `s`/`r`) | launch the TUI dashboard; first run → setup wizard |
| `init` | yes | yes | setup wizard (config + vault create/open) without the dashboard |
| `add <path>` | yes | yes (fetch first) | scan a directory, select files, associate/create vault project |
| `scan <path>` | **no** | no | dry run: print candidates with score and reason (`--json`) |
| `status [project…]` | yes | no (`--fetch`) | per file sync state, no changes (`--json`) |
| `sync [project…]` | yes | yes | bidirectional sync with merge |
| `push [project…]` | yes | yes | local → vault; remote-only changes are reported, not applied |
| `pull [project…]` | yes | yes | vault → local; local-only changes are reported, not applied |
| `restore <project> [--path <dir>]` | yes | yes | vault head → local for every tracked file (asks / `--yes`); `--path` links an unlinked vault project first |
| `projects list` | yes | no | linked projects (id, name, path, state) **and** unlinked vault projects |
| `projects link <id\|name> <path>` | yes | no | map a vault project to a local directory (then `pull` restores it) |
| `projects unlink <id\|name>` | no | no | remove the mapping from this machine (vault untouched) |
| `files add <project> <relpath…>` | yes | yes | track more files (upload) |
| `files rm <project> <relpath…>` | yes | yes | untrack everywhere, local copies kept |
| `files delete <project> <relpath…>` | yes | yes | delete everywhere (other machines move their copy to the trash) |
| `trash list\|restore <id>\|purge [--older-than 30d]` | yes | no | encrypted local trash |
| `passphrase change` | yes | yes | rewrap the vault key with a new passphrase |
| `config edit\|show\|path` | no | no | open config in `$EDITOR` / print |
| `unlock` | yes | no | test that the key can be obtained and the vault opens |
| `machine` | no | no | print machine id/name |

Global flags: `--config <file>`, `--yes` (non-interactive: accept confirmations),
`--strategy ask|local|remote|abort` (conflicts when non-interactive; default `abort` with
`--yes`, `ask` otherwise), `--delete` (propagate local deletions, rsync convention; never
implied by `--yes`), `--no-remote` (skip fetch/push), `--accept-rollback`, `--json`.

Projects are addressed by id or by name (name resolved from the vault after unlock).

### 2.2 TUI

Screens:

1. **Setup wizard** (first run, or `init`): one Huh multi-group form (shift-tab = back):
   vault path, remote type (`none`/`git`/`rclone`) + parameters, key source
   (`prompt`/`file`/`bitwarden`) + parameters, machine name. The form runs **standalone**
   (no dashboard program yet). Nothing is written until the final confirm; then: config
   saved (single atomic write), key obtained, vault creation protocol (§10.1) or open.
2. **Dashboard**: vault + remote summary; list of projects = linked projects (badges:
   `synced`, `local changes`, `remote changes`, `conflicts`, `pending`, `path missing`,
   `duplicate of <id>`) followed by **unlinked vault projects** (badge `not linked`, shows
   fingerprints). Keys: `a` add project, `s` sync, `u` push, `d` pull, `r` fetch+refresh,
   `enter` detail (or link, for an unlinked project), `c` settings, `q` quit.
   Badges are computed from a local `Plan` (no network) and refreshed after `r`/sync.
3. **Add project wizard**: path input (`~` expanded, must exist) → fetch (spinner, `esc`
   cancels) → identity: fingerprints computed; if the vault has a strong match the wizard
   proposes **associate** (default) and lists the vault-tracked files that are missing
   locally as `will be restored`; weak (`dir:` only) matches are shown with a warning;
   "create new project anyway" is an explicit choice → scanning (cancellable, live counter
   "walked N files, M candidates — esc to stop") → candidate multi-select (columns: path,
   size, score, reason; High pre-checked; `+` type an extra relative path; `/` filter;
   `t` show low-score too) → name → confirm → Apply (upload/restore) → Push.
4. **Project detail**: file list with state; `a` add files (re-scan), `x` untrack file,
   `D` delete everywhere (confirm), `R` restore project (confirm), `-` unlink (confirm),
   `s` sync this project.
5. **Sync view**: progress log fed by `sync.Event`s; when the plan has unresolved items →
   **Conflict resolver**: one file at a time, side-by-side/unified diff (local vs remote,
   plus the auto-merge if clean). Keys: `m` use merged (clean only), `l` keep local,
   `r` keep remote, `L`/`R` apply local/remote to all remaining, `k` per-key resolution
   (dotenv), `e` edit merged text with conflict markers in `$EDITOR` (via
   `tea.ExecProcess`; refused while markers remain), `s` skip file, `A` abort sync
   (nothing applied). Modify/delete conflicts show `keep` / `delete`. Then a summary.
6. **Settings**: key source, remote, scan lists, machine name, passphrase change.
   Key-source changes take effect at next launch.

Global keys: `esc` = back one step in wizards (state kept) / cancel the running operation
(scan, fetch, push, apply — all run as `tea.Cmd`s holding a cancellable `context`) /
close a detail; `ctrl+c` = quit (confirmation only while Apply is running).

**Key acquisition happens before any `tea.Program` starts**, in every entry point: the
root command obtains the passphrase through the terminal prompter (§11), opens the vault,
then launches the dashboard. The Bitwarden master-password prompt (§11) is part of that
synchronous step.

## 3. Configuration (per machine)

Location: `$XDG_CONFIG_HOME/private-sync/config.yaml`, default `~/.config/private-sync/config.yaml`
on every OS (plain XDG; no Apple/Windows native dirs). Override with `--config` or
`PRIVATE_SYNC_CONFIG`. `~` is expanded with `os.UserHomeDir()`; values are stored as typed
and expanded on load. Directory created 0700, file 0600.

```yaml
version: 1
machine:
  name: macbook-pro               # human label only (the id lives in the state dir, §9.4)
vault:
  path: ~/.local/share/private-sync/vault   # local vault directory (git clone when remote is git)
  remote:
    type: git                     # none | git | rclone
    git:
      url: git@github.com:me/private-sync-vault.git
      branch: main
    rclone:
      remote: gdrive              # rclone remote name (e.g. Google Drive)
      path: private-sync-vault    # path inside the remote
key:
  source: bitwarden               # prompt | file | bitwarden
  file:
    path: ~/.config/private-sync/key     # passphrase file, must be 0600 and owned by the user
  bitwarden:
    item: private-sync vault      # item name or id
    field: password               # password | notes | <custom field name>
scan:
  include: [...]                  # defaults in §6; a non-empty list replaces the defaults
  exclude_dirs: [...]
  exclude_files: [...]
  max_file_size: 2MiB
projects:
  - id: 3f2a9c1e5b7d0a46          # vault project id (opaque)
    name: myapp                   # informational, written by the program, never read for identity
    path: ~/Work/myapp            # local path on THIS machine
```

Env: `PRIVATE_SYNC_PASSPHRASE` (read once at startup into `[]byte`, then `os.Unsetenv`;
for cron use prefer `key.source: file`), `BW_SESSION` (reused by the Bitwarden source).

The **tracked file list lives in the vault** (journals), not in the config.

Validation (hard errors): `key.file.path` inside `vault.path`; unknown remote/key types;
`max_file_size` unparsable. Warning: key file inside a configured project path.

## 4. Cryptography

* **Vault key** `VK`: 32 random bytes generated at vault creation (`crypto/rand`).
* **KEK** = Argon2id(passphrase, salt 16 B, time 3, memory 64 MiB, threads 4, 32 B).
  Parameters are stored in `vault.json` and validated **before** running the KDF:
  `salt` exactly 16 bytes, `time ∈ [1,16]`, `memory ∈ [8 MiB, 1 GiB]` (stored in KiB, as
  `x/crypto/argon2` wants), `threads ∈ [1,16]`; anything else ⇒ "vault.json is invalid or
  tampered".
* `vault.json.wrapped_key` = XChaCha20-Poly1305(KEK, VK, random nonce,
  AAD = `"vault-key:" + vaultID`). A successful unwrap **is** the passphrase check (no
  separate verifier). `passphrase change` = derive new KEK (fresh salt), rewrap, rewrite
  `vault.json` only — no blob or id changes. Documented limit: rotation does not protect
  against an attacker who already holds an old copy of the vault (git history keeps the
  old `wrapped_key`); it protects against a weak/leaked passphrase going forward and
  allows KDF parameter upgrades.
* Subkeys: `sub(label) = HKDF-SHA256(ikm = VK, salt = nil, info = "private-sync/v1/" + label, 32 B)`
  for `enc`, `mac`, `nonce`. `internal/crypto` ships a known-answer test (fixed VK →
  expected subkeys, blob id and ciphertext) so the format cannot drift.
* Cipher: **XChaCha20-Poly1305** (24 B nonce). File format: `"PSV1"` (4 B) + nonce (24 B)
  + ciphertext (with 16 B tag).
* **Blobs are deterministic**: `blobID = hex(HMAC-SHA256(mac, plaintext))`,
  `nonce = HMAC-SHA256(nonce, plaintext)[:24]`, AAD = `"blob:" + blobID`. Same plaintext on
  two machines ⇒ byte-identical `.enc` ⇒ no git add/add conflicts, no re-upload.
  Determinism only reveals plaintext equality, which the content-addressed id reveals anyway.
* **Documents** (journals, meta, machines, trash blobs index) use a random nonce and
  AAD = the vault-relative path (or `"trash:" + id`), which prevents moving files around.
* Keyed ids: `project id = hex(HMAC-SHA256(mac, "project:" + strongestFingerprint))[:16]`
  (§7), so directory names reveal nothing to whoever hosts the vault.
* Passphrases travel as `[]byte`; the buffer is zeroed right after Argon2id; only the
  subkeys stay in memory for the life of the process and are zeroed on exit. Wiping is
  best effort (Go cannot guarantee no copies remain); key material is never written to
  disk. The Bitwarden master password bytes are dropped as soon as `bw unlock` returns.

Threat model note: the remote learns the number of machines, project count, blob count and
exact plaintext sizes (`len+16`), and the activity timeline. It never learns names, paths,
fingerprints or content. Anyone with write access to the remote can withhold or roll back
journals (detected, §9.3) but cannot forge them.

## 5. Vault layout and data model

```
<vault>/
  vault.json                          # plaintext: {version:1, id, kdf{algo:"argon2id", salt, time, memory, threads}, wrapped_key, created_at}
  .gitattributes / .gitignore         # written by Prepare (§10): *.enc binary; .psv-tmp-*, .DS_Store, …
  machines/<machineID>.json.enc       # MachineInfo {id, name, hostname, last_seen}
  projects/<projectID>/
      meta/<machineID>.json.enc       # ProjectMeta {name, fingerprints[], created_at}  (union across machines)
      state/<machineID>.json.enc      # Journal {machine, seq, updated_at, entries: map relpath → Entry}
  blobs/<aa>/<blobID>.enc             # content addressed, deterministic, immutable
```

**Rule: a machine only ever writes its own `<machineID>` files plus blobs.** Two machines
never modify the same file, so git merges are always clean and Drive/rclone/desktop sync
never has to resolve anything. Conflict handling happens in the application.
Listing `machines/`, `meta/`, `state/` accepts only names matching
`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json\.enc$`; anything
else (Drive `(1)`, Dropbox `conflicted copy`, `.DS_Store`) is skipped with a warning naming
the file. An undecryptable journal makes the whole project `unreadable` (plan aborted for
that project) rather than being silently ignored.

```go
package vault

type Kind int // KindFile, KindDeleted, KindUntracked
type Clock map[string]uint64 // machineID → counter (version vector)
type Ordering int            // Equal, Before, After, Concurrent
func (c Clock) Compare(o Clock) Ordering
func (c Clock) Merge(o Clock) Clock          // componentwise max
func (c Clock) Tick(machine string) Clock    // copy + increment own component

type Entry struct {
    Path      string    `json:"path"`      // slash separated, relative to project root
    Kind      Kind      `json:"kind"`
    Blob      string    `json:"blob,omitempty"`    // "" for tombstones
    Clock     Clock     `json:"clock"`
    Parents   []string  `json:"parents,omitempty"` // blob ids this version was derived from (never "")
    Mode      uint32    `json:"mode,omitempty"`
    Size      int64     `json:"size,omitempty"`
    ModTime   time.Time `json:"mtime,omitempty"`
    UpdatedAt time.Time `json:"updated_at"`
    Machine   string    `json:"machine"`
}
type Journal struct {
    Machine   string           `json:"machine"`
    Seq       uint64           `json:"seq"`        // +1 on every write by its owner
    UpdatedAt time.Time        `json:"updated_at"`
    Entries   map[string]Entry `json:"entries"`    // latest entry per path
}
type ProjectMeta struct { Name string; Fingerprints []identity.Fingerprint; CreatedAt time.Time }
type Project struct { ID, Name string; Fingerprints []identity.Fingerprint; Machines []string; CreatedAt time.Time } // merged view
type MachineInfo struct { ID, Name, Hostname string; LastSeen time.Time }

type Head struct {
    Path       string
    Candidates []Entry // one per machine after dominance filtering, ≥1
    Entry      *Entry  // non-nil when Candidates agree (single head)
    Base       string  // blob shared by all candidates' Parents, "" if none (concurrent only)
}
func ResolveHeads(journals map[string]*Journal) map[string]Head
```

**Head resolution** for a path: candidates = the latest entry of every machine; drop every
candidate whose Clock is dominated by (Before) another's; the survivors are compared by the
state tuple `(Kind, Blob)`:
* all equal ⇒ single head (`Entry` set, any survivor).
* several `KindUntracked` vs anything ⇒ `KindUntracked` wins (safety: never touch the file).
* `KindDeleted` vs `KindDeleted` ⇒ single head.
* otherwise ⇒ **concurrent** (`Entry == nil`, `Candidates` listed, `Base` = a blob common to
  every candidate's non-empty `Parents`, else "").

Parents are used **only** to find a merge base and blob equality only for "same content";
neither is used for ordering. A stale entry `{A:1}` is dominated by `{A:3}`; a concurrent
`{A:1,B:1}` vs `{A:3}` is incomparable and flagged — the scalar counter of v1 could not
distinguish those.

**Writing** a new entry for a path: `Clock = merge(all candidates' clocks).Tick(self)`,
`Parents = non-empty blobs of the candidates it incorporates` (one for a normal edit, two
or more for a merge). Writing never requires a preceding fetch (offline edits are fine);
divergence is simply detected at the next fetch as a concurrent head.

Project ids: opaque 16 hex; deterministic from the strongest level 1–2 fingerprint (§4)
so two machines that add the same repo before seeing each other land in the same directory
(each still writes only its own `meta/` and `state/` files); random 16 hex only for
`dir:`-only projects or when the user explicitly chooses "create new anyway". The project
name lives only in encrypted meta (first-created wins for display; the union of
fingerprints is the identity). Plan detects distinct project ids sharing a level 1–2
fingerprint and reports `duplicate of <id>` (manual fix: `projects link`).

Vault API (package `vault`):

```go
func Exists(dir string) bool
func Create(dir string, passphrase []byte, params crypto.KDFParams) (*Vault, error) // writes vault.json (0600) only
func Open(dir string, passphrase []byte) (*Vault, error)                            // validates, unwraps; ErrWrongPassphrase / ErrInvalidVault / ErrNewerVersion
func (v *Vault) ID() string
func (v *Vault) Dir() string
func (v *Vault) Keys() *crypto.Keys
func (v *Vault) Close()                                    // zero keys
func (v *Vault) Rekey(newPassphrase []byte, params crypto.KDFParams) error
func (v *Vault) BlobID(plaintext []byte) string
func (v *Vault) WriteBlob(plaintext []byte) (id string, created bool, err error)
func (v *Vault) ReadBlob(id string) ([]byte, error)        // ErrBlobMissing when absent/unreadable/undecryptable
func (v *Vault) HasBlob(id string) bool
func (v *Vault) ListProjects() ([]Project, []string /*warnings*/, error)
func (v *Vault) ReadProject(id string) (*Project, []string, error)  // ErrNoProject
func (v *Vault) WriteProjectMeta(projectID, machineID string, m ProjectMeta) error
func (v *Vault) ReadJournals(projectID string) (map[string]*Journal, []string /*warnings*/, error) // ErrUnreadableJournal aborts
func (v *Vault) ReadJournal(projectID, machineID string) (*Journal, error) // nil, nil when absent
func (v *Vault) WriteJournal(projectID string, j *Journal) error           // bumps Seq, atomic
func (v *Vault) ListMachines() ([]MachineInfo, []string, error)
func (v *Vault) WriteMachine(m MachineInfo) error
func (v *Vault) Written() []string   // vault-relative paths written since Open (for rclone Push)
```

## 6. Scanning

`scan.Scan(ctx, dir, opts, runner)` walks the project directory: no symlink following,
skip `exclude_dirs` by name, skip files above `max_file_size`, skip the configured key
file and anything under the vault path (hard excludes), stop descending into any
directory other than the root that contains a `.git` entry (dir **or** file) and list it
once in `NestedRepos` ("add it as its own project"). Walk cap: 200 000 files / 5 000
candidates ⇒ `Truncated = true` + warning. Progress callback every 500 files.

Candidate = base name matches an `include` glob (case-insensitive) or a secret-ish pattern.

Default include: `.env`, `.env.*`, `*.env`, `.envrc`, `*.yaml`, `*.yml`, `*.json`, `*.toml`,
`*.ini`, `*.cfg`, `*.conf`, `*.properties`, `.npmrc`, `.yarnrc`, `.yarnrc.yml`, `.pypirc`,
`.netrc`, `*.pem`, `*.key`, `*.crt`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `*.tfvars`,
`*secret*`, `*credential*`, `service-account*.json`, `appsettings.*.json`,
`application-*.yml`, `application-*.yaml`, `application-*.properties`, `*.local.*`,
`wp-config.php`, `.htpasswd`, `*.env.js`, `*.env.ts`.

Default exclude dirs: `.git`, `node_modules`, `vendor`, `dist`, `build`, `out`, `target`,
`.next`, `.nuxt`, `.svelte-kit`, `.venv`, `venv`, `env`, `__pycache__`, `.idea`, `.cache`,
`coverage`, `.terraform`, `bin`, `obj`, `Pods`, `DerivedData`, `.gradle`, `.dart_tool`,
`.turbo`, `.parcel-cache`, `.pytest_cache`, `.mypy_cache`, `tmp`, `logs`, `testdata`,
`fixtures`, `__fixtures__`, `locales`, `i18n`, `.github`.

Default exclude files: `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `composer.lock`,
`Cargo.lock`, `go.sum`, `*.min.json`, `tsconfig*.json`, `*.schema.json`, `.eslintrc*`,
`.prettierrc*`, `renovate.json`, `lerna.json`, `jsconfig.json`, `package.json`,
`composer.json`, `manifest.json`, `*.lock.json`.

**Secret-ish** names: `.env`, `.env.*`, `*.env`, `.netrc`, `.npmrc`, `.pypirc`, `.htpasswd`,
`*.tfvars`, `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, and `*secret*`,
`*credential*`, `*.local.*` **only when the extension is not a source-code extension**
(`.go .ts .tsx .js .jsx .mjs .cjs .java .kt .py .rs .rb .php .cs .swift .c .h .cpp .m .scala .vue .svelte`).

Git state is evaluated **first** with one batch per scan, run from the project path
(so sub-directory projects and worktrees work): `git -C <dir> ls-files -z` (tracked set)
and `git -C <dir> check-ignore -z --stdin --no-index` (exit 0 and 1 are success; 128 or no
git binary ⇒ "no git info", every match becomes Medium).

Scoring:
* git-tracked ⇒ **Low**, never pre-selected, badge `committed to git` (git already carries it).
* git-ignored and matches ⇒ **High** (exactly the files git does not carry).
* untracked, not ignored ⇒ **Medium**; pre-selected only if secret-ish.
* no git info ⇒ **Medium**; secret-ish ⇒ **High**.

Files already tracked in the vault for that project are shown as `tracked` and checked.

```go
package scan
type Score int // ScoreLow, ScoreMedium, ScoreHigh
type Candidate struct {
    Path string; Size int64; Mode uint32       // Path slash-separated relative to dir
    Score Score; Reasons []string
    GitTracked, GitIgnored, SecretName, Tracked, Preselected bool
}
type Options struct {
    Include, ExcludeDirs, ExcludeFiles []string
    MaxFileSize int64
    HardExclude []string            // absolute paths (key file, vault dir)
    Tracked map[string]bool         // relpaths already in the vault
    MaxFiles, MaxCandidates int     // 0 = defaults
    Progress func(walked, found int)
}
type Result struct { Candidates []Candidate; NestedRepos []string; GitInfo bool; Truncated bool; Warnings []string }
func Scan(ctx context.Context, dir string, opts Options, r execx.Runner) (*Result, error)
func DefaultInclude() []string; func DefaultExcludeDirs() []string; func DefaultExcludeFiles() []string
func IsSecretName(name string) bool
```

## 7. Project identity

```go
package identity
type Level int // LevelStrong (git), LevelPackage (manifests), LevelDir
type Fingerprint struct { Kind string `json:"kind"`; Value string `json:"value"`; Level Level `json:"level"` }
func (f Fingerprint) String() string   // kind + ":" + value
func Detect(ctx context.Context, dir string, r execx.Runner) ([]Fingerprint, error)
type Strength int // StrengthNone, StrengthWeak, StrengthStrong
type Match struct { ProjectID string; Strength Strength; Shared []Fingerprint }
func MatchProjects(local []Fingerprint, vaultProjects map[string][]Fingerprint) []Match // strong first
func Strongest(fps []Fingerprint) (Fingerprint, bool)   // first level 1, else first level 2
func NormalizeGitURL(raw string) (hostPath string, ok bool)
```

Fingerprints, all stored:

1. `git` (LevelStrong), one per remote: value `<host>/<path>` at the repo root, or
   `<host>/<path>#<prefix>` when the project directory is a sub-directory of the repo
   (`git -C <dir> rev-parse --show-toplevel --show-prefix`; prefix without trailing slash).
   URL from `git -C <dir> config --get-regexp '^remote\..*\.url$'`; fallback without git:
   walk up to the nearest `.git` dir or file (`gitdir:` pointer) and parse `config`.
   Normalisation: lowercase; strip scheme, `user@`, `:port`; scp form `host:path` →
   `host/path`; strip leading `/`, trailing `/` and `.git`; `/_git/` → `/`; `ssh.`/`www.`
   host prefixes removed. Monorepo sub-projects therefore differ by prefix; forks with
   different remotes match through any shared remote.
2. LevelPackage: `go:<module>` (`go.mod`), `npm:<name>` (`package.json`), `cargo:<name>`
   (`Cargo.toml` `[package]`), `py:<name>` (`pyproject.toml` `[project]`/`[tool.poetry]`),
   `composer:<name>`, `maven:<groupId>:<artifactId>` (`pom.xml`), `gem:<name>`
   (`*.gemspec`), `swift:<name>` (`Package.swift`), `dart:<name>` (`pubspec.yaml`).
3. `dir:<basename>` (LevelDir), always present.

Matching: **strong** = any shared level 1–2 fingerprint; **weak** = only `dir:` shared
(proposed with a warning, never default). The wizard defaults to associate on strong, lists
all matches when several, and requires an explicit choice to create a new project when a
match exists. Associating writes this machine's `meta/` with the union of fingerprints.

## 8. Merge

```go
package merge
type Kind int // KindText, KindDotenv, KindBinary
type LineRange struct{ Start, End int }          // 0-based, half-open, in the side's lines
type Hunk struct {
    Key                string                     // dotenv variable name; "" for text
    Base, Local, Remote []byte                    // nil = absent on that side
    BaseRange, LocalRange, RemoteRange LineRange  // text only
}
type Result struct {
    Kind   Kind
    Clean  bool
    Merged []byte   // valid only when Clean
    Hunks  []Hunk   // conflicts, in file order; empty when Clean
    Note   string   // e.g. "formatting-only change on remote dropped", "dotenv parse failed, used text merge"
}
type Side int // SideLocal, SideRemote, SideBase, SideCustom
type Choice struct { Side Side; Custom []byte }
func ThreeWay(path string, base, local, remote []byte) *Result   // base nil = no base; local/remote never nil
func Resolve(r *Result, choices []Choice) ([]byte, error)         // one choice per hunk, reassembles the file
func RenderMarkers(r *Result, localLabel, remoteLabel string) []byte // diff3-style markers for $EDITOR
func HasMarkers(b []byte) bool
type DiffOp struct { Kind byte /* ' ', '-', '+' */; Text string }
func LineDiff(a, b []byte) []DiffOp                              // for the TUI diff view
func KindFor(path string, content []byte) Kind
```

* **dotenv** applies only to `.env`, `.env.*`, `*.env`. Grammar per physical line: blank |
  `#` comment | entry `^\s*(export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=(.*)$`; a value that
  starts with `"` or `'` and is not closed on the same line consumes following lines up to
  the matching unescaped quote. `Raw` keeps `export `, quotes and every physical line.
  **Strict**: any line matching none of these, an unbalanced quote, or a duplicate key on
  any side ⇒ fall back to text diff3 (`Note` set); no partial dotenv output ever.
  Key-level 3-way over the union of keys with *absent* as a value; equality is byte
  equality of `Raw` (quoting changes are changes): `local==base` → remote value (incl.
  removal); `remote==base` → local; `local==remote` → either; else a conflict hunk
  (`Base/Local/Remote` = raw entry bytes, nil when absent). Output keeps local order and
  comments, appends keys new on remote (absent in base) at the end.
* **text** (valid UTF-8, no NUL): whole-file whitespace-only short-circuit first —
  `normalise(side) == normalise(base)` (strip `\r`, trailing whitespace, collapse leading
  whitespace runs, ignore final newline) for exactly one side ⇒ result = the other side,
  Clean, `Note` says the formatting-only change was dropped. Then line-level diff3: LCS on
  lines compared with a trailing `\r` stripped, emitting the original bytes of the side
  that contributed each line; non-overlapping changes apply, identical changes on both
  sides are clean, overlapping ones become conflict hunks. Rendered markers:
  `<<<<<<< local / ||||||| base / ======= / >>>>>>> remote`.
* **binary** (NUL byte or invalid UTF-8): one hunk with whole contents; never mergeable.
* **no base** (`base == nil`): equal contents ⇒ Clean; otherwise one whole-file hunk
  (KindText still, so the diff view works).
* Tombstones never reach `ThreeWay`; modify/delete conflicts are engine-level (§9).

## 9. Sync engine

### 9.1 Types

```go
package sync
type Mode int      // ModeSync, ModePush, ModePull, ModeRestore
type Strategy int  // StrategyAsk, StrategyLocal, StrategyRemote, StrategyAbort
type Action int
const (
    ActionInSync Action = iota
    ActionUpload        // local → vault (new blob + entry)
    ActionDownload      // vault → local (previous local content, if it differs from base, goes to the trash)
    ActionConverge      // same content on both sides: update base only
    ActionTrashLocal    // head deleted, local unchanged: local copy → trash, base := tombstone
    ActionUntrack       // head untracked: drop base, never touch the file
    ActionMissingLocal  // tracked file absent locally, vault unchanged: report only (ActionDeleteRemote with --delete / files delete)
    ActionDeleteRemote  // write a KindDeleted entry
    ActionConflict      // needs a Resolution
    ActionPending       // a needed blob is not readable yet (remote in flight): skip, keep base
    ActionRollback      // head older than base: report only (restore/--accept-rollback to apply)
    ActionReportOnly    // would be applied in another mode (push/pull): report only
)
type ConflictKind int // ConflictContent, ConflictNoBase, ConflictModifyDelete, ConflictDeleteModify, ConflictConcurrent
type ItemKey struct{ Project, Path string }
type FileRef struct { Blob string; Kind vault.Kind; Size int64; Mode uint32; ModTime time.Time; Clock vault.Clock; Machine string }
type Item struct {
    Key          ItemKey
    Action       Action
    Conflict     ConflictKind
    Local, Base, Head *FileRef   // nil = absent; Head is the resolved (or synthetic merged) head
    Candidates   []vault.Entry   // >1 when the vault head is concurrent
    Merge        *merge.Result   // ConflictContent/NoBase/Concurrent: precomputed
    LocalText, BaseText, HeadText []byte // decrypted contents for the resolver (bounded by max_file_size)
    Reason       string          // one human line
    NeedsResolution bool
}
type ProjectPlan struct { ID, Name, Path string; Missing, Unreadable bool; DuplicateOf string; Items []Item; Warnings []string }
type Plan struct { Mode Mode; Projects []ProjectPlan; Warnings []string }
type ChoiceKind int // ChooseMerged, ChooseLocal, ChooseRemote, ChooseCustom, ChooseKeep (modify/delete: keep file), ChooseDelete, ChooseConfirm, ChooseSkip
type Resolution struct { Kind ChoiceKind; Content []byte /* ChooseCustom */ }
type Resolutions map[ItemKey]Resolution
type Event struct { Stage string /* fetch|plan|apply|push */; Project, Path, Message string; Done, Total int; Err error }
type Options struct {
    Mode Mode; Projects []string /* ids; empty = all */
    Strategy Strategy; PropagateDeletes, AcceptRollback bool
    Track map[ItemKey]bool     // paths explicitly (re)added this run
    Progress func(Event)       // nil = silent
}
type Report struct { Uploaded, Downloaded, Converged, Trashed, Untracked, Deleted, Skipped, Pending int; Unresolved []ItemKey; Errors []ItemError }
type ItemError struct { Key ItemKey; Err error }

type Engine struct { /* vault, state store, config, remote, machine id, hard limits */ }
func New(v *vault.Vault, st *state.Store, cfg *config.Config, r remote.Remote, machine state.Machine) *Engine
func (e *Engine) Fetch(ctx context.Context, progress func(Event)) error   // remote.Fetch only
func (e *Engine) Plan(ctx context.Context, opts Options) (*Plan, error)     // pure: local vault copy + base store + project dirs
func (e *Engine) Apply(ctx context.Context, p *Plan, res Resolutions, opts Options) (*Report, error)
func (e *Engine) Push(ctx context.Context, progress func(Event)) error    // remote.Push(vault.Written())
func ApplyStrategy(p *Plan, s Strategy, res Resolutions)                  // fills missing resolutions per strategy
```

The engine has **no UI, reads no flags and no env**; `Options` and `Resolutions` are its
only inputs. `local` hashes are `vault.BlobID(plaintext)` — the same HMAC as blob ids —
so every comparison is string equality (and every command that reads sync state needs
the key). `Plan` reads and decrypts every blob an item needs (head for download/merge,
base for merge) up to `max_file_size`; any read failure ⇒ `ActionPending`. Head resolution
depends on journals only.

### 9.2 Decision table

Planner path set per project = keys(head map) ∪ keys(base store) ∪ `opts.Track`.
`base` is the entry this machine last converged to (blob or tombstone, with its Clock).
Rows are checked top to bottom; `L` = local file, `B` = base, `H` = head.

| # | H | L | B | result |
|---|---|---|---|---|
| 1 | untracked | any | any | **untrack** (drop base, leave file); unless `Track[path]` ⇒ upload with `Clock = merge(candidates).Tick(self)` |
| 2 | concurrent set | — | — | pre-merge candidates pairwise with `ThreeWay(Head.Base, c1, c2)` (tombstone candidate or no base ⇒ not clean). Clean ⇒ synthetic H (merged bytes, blob written on apply, Parents = all candidate blobs) and continue with rows 5–12. Not clean ⇒ **conflict** `ConflictConcurrent` (never auto-download) |
| 3 | absent | present | any | **upload** if `Track[path]` or B present (re-publish); else nothing (not tracked) |
| 4 | absent | absent | any | drop base |
| 5 | deleted | absent | any | converge base := tombstone (no item) |
| 6 | deleted | present | file, L = B | **trash local** |
| 7 | deleted | present | file, L ≠ B | **conflict** `ConflictModifyDelete`: keep (upload, Clock from tombstone) / delete (trash) |
| 8 | deleted | present | absent | **conflict** `ConflictModifyDelete` without base (same choices) |
| 9 | deleted | present | tombstone | report `deleted in vault, present locally` (re-track with `files add`) |
| 10 | file H | present | absent or tombstone | L = H ⇒ **converge**; else **conflict** `ConflictNoBase` (2-way) |
| 11 | file H | present | file B | L = B ∧ H = B ⇒ in sync · L ≠ B ∧ H = B ⇒ **upload** · L = B ∧ H ≠ B ⇒ **download** (rollback check first) · L ≠ B ∧ H ≠ B ∧ L = H ⇒ **converge** · else **conflict** `ConflictContent` = `ThreeWay(B, L, H)` |
| 12 | file H | absent | absent or tombstone | **download** |
| 13 | file H | absent | file B, H = B | **missing locally** (report only; `--delete`/`files delete` ⇒ delete remote) |
| 14 | file H | absent | file B, H ≠ B | **download** (restore the newer remote version) |

Rollback check (row 11 download, row 12/14): if `H.Clock` is Before or Concurrent with
`B.Clock` the single head is older than what this machine already converged to — a journal
was replaced by an older copy. ⇒ `ActionRollback`, report only, applied only with
`AcceptRollback` (or `restore`). Also: a journal whose `Seq` is lower than the highest seen
(base store keeps `journals: {machine: maxSeq}`) marks every item derived from it with
`Reason = "journal of <machine> rolled back"` and the same treatment.

Modes: `sync` applies everything. `push` turns download/trash-local into `ActionReportOnly`
and applies uploads, delete-remote and resolved conflicts. `pull` turns upload/delete-remote
into `ActionReportOnly` and applies downloads, trash-local and resolved conflicts.
`restore` = every tracked head becomes a download (local copies to trash), for `restore`.
Conflicts must be resolved (or skipped) in every mode; a merge result is uploaded as a new
entry with all parents, so other machines see it resolved.

`ApplyStrategy`: `local` ⇒ `ChooseLocal` (modify/delete: keep), `remote` ⇒ `ChooseRemote`
(modify/delete: delete), `abort` ⇒ skip all, `ask` ⇒ leave unresolved (the front end asks).
Items with `NeedsResolution` and no resolution are skipped and listed in `Report.Unresolved`.

### 9.3 Apply

Per item, in order: write the blob (if any), write the local file (atomic, mode preserved;
if the current local content differs from what Plan saw ⇒ `ErrStale`, item skipped), record
the pre-image in the trash when a local file is replaced or removed and its content is not
the base, update the in-memory journal and base store. After all items of a project:
write this machine's journal (Seq+1), meta (if changed) and machine info, then save the
base store. Ctx cancellation between items is safe: the next Plan recomputes from disk
(deterministic blobs, journal only written after the project's items).

### 9.4 Local state (package `state`)

`$XDG_STATE_HOME/private-sync/` (default `~/.local/state/private-sync/`), dir 0700, files 0600:

* `machine.json` — `{id (uuid), created_at, vaults: {<abs vault path>: <vaultID>}}`. The
  machine id is generated here on first use and **never lives in config.yaml** (a copied
  config must not clone the id). `vaults` pins the vault id per path: opening a vault whose
  id differs from the pin ⇒ hard error ("remote already contains a different vault; run
  init and choose open, or delete the local copy").
* `vaults/<vaultID>/base.json` — `{projects: {projectID: {relpath: {blob, kind, clock}}}, journals: {machineID: maxSeq}}`.
* `vaults/<vaultID>/trash/index.json` + `trash/<aa>/<blobID>.enc` — pre-images encrypted
  with the vault's deterministic blob format (§4). Index rows `{id, ts, project, path, blob, mode, size}`.
  `trash list|restore|purge`; default purge of rows older than 30 days on every Apply.
  **Invariant: plaintext secret content exists only inside project directories.**

```go
package state
type Machine struct { ID string; CreatedAt time.Time }
func LoadMachine(stateDir string) (Machine, error)       // creates on first use
func PinnedVault(stateDir, vaultPath string) (string, bool, error)
func PinVault(stateDir, vaultPath, vaultID string) error
type BaseEntry struct { Blob string; Kind vault.Kind; Clock vault.Clock }
type Store struct{ /* … */ }
func Open(stateDir, vaultID string) (*Store, error)
func (s *Store) Base(project, path string) (BaseEntry, bool)
func (s *Store) SetBase(project, path string, b BaseEntry); func (s *Store) DeleteBase(project, path string)
func (s *Store) Bases(project string) map[string]BaseEntry
func (s *Store) JournalSeq(machine string) uint64; func (s *Store) SetJournalSeq(machine string, seq uint64)
func (s *Store) Save() error
type TrashEntry struct { ID string; Time time.Time; Project, Path, Blob string; Mode uint32; Size int64 }
func (s *Store) TrashPut(k *crypto.Keys, project, path string, content []byte, mode uint32) (TrashEntry, error)
func (s *Store) TrashList() ([]TrashEntry, error)
func (s *Store) TrashRead(k *crypto.Keys, id string) ([]byte, TrashEntry, error)
func (s *Store) TrashPurge(olderThan time.Duration) (int, error)
```

## 10. Remotes

```go
package remote
type Remote interface {
    Name() string
    Prepare(ctx context.Context, log func(string)) error                    // idempotent bootstrap
    Fetch(ctx context.Context, log func(string)) error
    Push(ctx context.Context, written []string, log func(string)) error     // vault-relative paths written since open
}
type Options struct { MachineID, MachineName string; Runner execx.Runner }
func New(cfg config.RemoteConfig, vaultDir string, o Options) (Remote, error)
```

**Transport invariant** (every backend): Push transfers only this machine's own three files
(`machines/<id>`, `projects/*/meta/<id>`, `projects/*/state/<id>`) plus blobs and
`vault.json`/`.gitattributes`/`.gitignore`; Fetch never overwrites this machine's own files.

* `none`: no-op. For a vault path inside a folder synced by Google Drive desktop, Dropbox,
  iCloud, Syncthing… (documented as the simplest Google Drive setup; recommend "Mirror
  files" / disable "Optimize storage" so blobs are never dehydrated).
* `git`: every command runs through `execx` with stdin nil, stdout parsed, stderr streamed
  to `log`, env `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`, `GIT_SSH_COMMAND` = the user's value (or
  `ssh`) + ` -o BatchMode=yes`; args prefixed with `-c user.name=private-sync
  -c user.email=private-sync@localhost -c commit.gpgsign=false -c core.autocrlf=false
  -c core.hooksPath=/dev/null`. Never prompts; failures are classified from stderr
  (auth / unknown host key / network / non-fast-forward) with a one-line hint such as
  "authenticate once in a terminal: git -C <vault> fetch".
  - `Prepare`: if `<vault>/.git` is missing: `git init -b <branch>`, `git remote add origin <url>`
    (or `set-url`); write `.gitattributes` (`*.enc binary`, `vault.json text`) and
    `.gitignore` (`.psv-tmp-*`, `.DS_Store`, `desktop.ini`, `Thumbs.db`, `.tmp.driveupload/`,
    `.tmp.drivedownload/`) if absent. Then `git fetch origin`; `have := git rev-parse
    --verify -q origin/<branch>` is the "remote empty" predicate. If `have` and the local
    branch has no commits: `git reset --hard origin/<branch>` (adopt remote history) —
    but if a local `vault.json` already exists with a different id ⇒ error (§10.1). If
    `have` and local commits exist: `git rebase origin/<branch>`. Set `branch.<b>.remote/merge`.
  - `Fetch`: `git fetch origin`; if `origin/<branch>` exists: `git rebase origin/<branch>`
    (working tree must be clean; if a previous Push left uncommitted files, commit them
    first). On rebase failure: `git rebase --abort` and return an error naming the file.
  - `Push`: `git add -A`; `git diff --cached --quiet || git commit --no-verify -m sync`;
    `git push -u origin <branch>`; on rejection: Fetch (rebase) and retry, max 3.
* `rclone`: `Fetch` = two invocations, blobs first: `rclone copy <remote>:<path>/blobs
  <vault>/blobs --ignore-existing`, then `rclone copy <remote>:<path> <vault> --exclude
  /blobs/** --exclude 'machines/<id>.json.enc' --exclude 'projects/*/meta/<id>.json.enc'
  --exclude 'projects/*/state/<id>.json.enc' --exclude '.psv-tmp-*'`. `Push` = blobs first
  with `--files-from <list of written blobs> --no-traverse --ignore-existing`, then the own
  files with `--files-from`. Never `sync`, never deletes. Works with the `drive` backend.

### 10.1 Vault creation protocol (setup wizard / `init`)

`remote.Prepare` → `remote.Fetch` → if `vault.json` exists ⇒ **open** is the only option
(passphrase verified by unwrap) ⇒ pin the id. Else ⇒ **create**: write `vault.json`
(+ machine info) ⇒ `Push` immediately ⇒ `Fetch` again ⇒ read back `vault.json` and verify
the id equals the one written (another machine won the race ⇒ error telling the user to
re-run and open). Only then pin the id. `vault.json` is never rewritten except by
`passphrase change`.

## 11. Key sources

```go
package ui
type Prompter interface {
    Password(ctx context.Context, title string) ([]byte, error)
    Confirm(ctx context.Context, title string, def bool) (bool, error)
}
package keysource
type Source interface { Name() string; Passphrase(ctx context.Context, p ui.Prompter) ([]byte, error) }
func FromConfig(cfg config.KeyConfig, r execx.Runner) (Source, error)
func Obtain(ctx context.Context, cfg config.KeyConfig, p ui.Prompter, r execx.Runner) ([]byte, error) // env first
```

* `env`: `PRIVATE_SYNC_PASSPHRASE`, captured once by `cli` at startup and unset.
* `prompt`: `p.Password("Vault passphrase")`.
* `file`: resolve symlinks; hard error (OpenSSH style, with the exact `chmod 600 <path>`
  remedy) if `mode & 0o077 != 0` or the owner is not the current uid (checks skipped on
  Windows); content trimmed of trailing whitespace. The wizard creates the file itself
  (`O_CREAT|O_EXCL`, 0600) with a generated 32-byte base64 passphrase when the user picks
  `file` and the file does not exist.
* `bitwarden`: `bw status` (JSON) ⇒ `unauthenticated` ⇒ error "run `bw login` first";
  `locked` ⇒ `p.Password("Bitwarden master password")` ⇒ `bw unlock --raw --passwordenv
  BW_PASSWORD` with `BW_PASSWORD` set **only** in that command's env ⇒ session kept in
  memory for this process; `unlocked` with `BW_SESSION` in the environment ⇒ reuse it.
  Then `bw get item <item>` with `BW_SESSION` only in that command's env; stdout parsed as
  JSON, stderr ignored (update nags); `field`: `password` → `login.password`, `notes` →
  `notes`, otherwise the custom field with that name. Master password bytes wiped
  immediately after unlock.

Calling rule: `Source.Passphrase` blocks and is only invoked before a `tea.Program`
exists. The one prompter implementation is `cli.TerminalPrompter` (`x/term.ReadPassword`
on the controlling terminal; huh standalone form when stdin is not a terminal but
`/dev/tty` is; error when neither).

## 12. Package layout

```
cmd/private-sync/main.go
internal/paths       Dirs{Config, State}, XDG defaults, ExpandHome/ContractHome (only package reading XDG_*/HOME)
internal/execx       subprocess runner: Cmd/Result/Runner, Real() with env denylist, Fake() for tests, LookPath
internal/fsutil      WriteFileAtomic (same-dir temp `.psv-tmp-<machineID8>`, fsync, rename, Windows retry), CleanupTemp, EnsureDir
internal/ui          Prompter interface (leaf)
internal/config      Config types, Default, Load, Save (atomic 0600), Validate, size parsing, project helpers
internal/crypto      KDFParams, DeriveKEK, NewVaultKey, Wrap/UnwrapKey, Keys (HKDF), Seal/OpenBlob (deterministic), Seal/OpenDoc, BlobID, KeyedID; KAT tests
internal/identity    fingerprints, git URL normalisation, matching
internal/scan        walker + scoring + git batch
internal/merge       dotenv + diff3 + LineDiff + Resolve/RenderMarkers/HasMarkers
internal/vault       vault.json, Create/Open/Rekey, blobs, journals, meta, machines, ResolveHeads, Written()
internal/state       machine.json, vault pin, base store, encrypted trash
internal/remote      Remote interface, none, git, rclone (+ Prepare protocol helpers)
internal/sync        Plan/Apply engine, ApplyStrategy, Events
internal/keysource   env/prompt/file/bitwarden
internal/app         wiring shared by cli and tui: App (config + dirs + machine, no key), Session (vault + state + remote + engine), Setup (§10.1)
internal/tui         Bubble Tea app (screens §2.2) — uses only app/sync/merge/scan/identity types
internal/cli         cobra commands, TerminalPrompter, env capture, exit codes
```

```go
package execx
type Cmd struct { Name string; Args []string; Dir string; Env []string /* extra KEY=VALUE */; Stdin []byte; OnStderr func(line string) }
type Result struct { Stdout, Stderr []byte; ExitCode int }
type ExitError struct { Cmd Cmd; Result Result } // returned (wrapped) on non-zero exit
type Runner interface { Run(ctx context.Context, c Cmd) (Result, error) }
func Real() Runner                                   // exec.CommandContext, stdin nil, env = os.Environ() minus Denylist() plus c.Env
func Denylist() []string                             // PRIVATE_SYNC_PASSPHRASE, BW_PASSWORD, BW_SESSION, BW_CLIENTSECRET
func Fake(handler func(c Cmd) (Result, error)) Runner
func LookPath(name string) (string, bool)

package app
type App struct { Dirs paths.Dirs; ConfigPath string; Config *config.Config; Machine state.Machine; Runner execx.Runner }
func Load(configPath string, dirs paths.Dirs, r execx.Runner) (*App, error)   // ErrNoConfig on first run; no key
type Session struct { *App; Vault *vault.Vault; State *state.Store; Remote remote.Remote; Engine *sync.Engine }
func (a *App) Open(ctx context.Context, p ui.Prompter) (*Session, error)          // key + Prepare + vault.Open + pin check; NO fetch
func (a *App) Setup(ctx context.Context, p ui.Prompter, log func(string)) (*Session, error) // §10.1 create-or-open
func (s *Session) Close()
```

Every package has unit tests. `internal/sync` has an integration test simulating two
machines (two state dirs, two project dirs, one shared vault dir, remote `none`, two
`Engine`s with different machine ids) covering: initial upload, restore on the second
machine, one-sided edits, converging edits, dotenv key conflict auto-resolved, text
conflict with strategy local/remote, concurrent-head detection after an offline edit on
both sides, deletion propagation via `files delete`, untrack stickiness, rollback
detection, pending blob. `internal/remote` tests git against a local bare repository
(skipped if `git` is absent) and rclone/bitwarden with `execx.Fake`.

## 13. Error handling and safety

* Plaintext secret content exists only inside project directories; state dir and vault
  hold ciphertext only. Config/state/vault dirs 0700, files 0600 regardless of source mode
  (original mode recorded in journal/trash and reapplied on restore).
* Never overwrite a local file that differs from its base without a resolution; pre-images
  go to the encrypted trash first. Local deletions are propagated only on explicit request.
* Atomic writes everywhere (`fsutil`); stale own temp files removed at Apply start.
* Wrong passphrase ⇒ `ErrWrongPassphrase` before anything is read or written. `vault.json`
  validated before the KDF runs; newer `version` refused.
* Project path missing ⇒ `path missing`, skipped by sync. Unreadable journal ⇒ project
  skipped with an error. Pending blobs ⇒ skipped, base untouched.
* Children never inherit `PRIVATE_SYNC_PASSPHRASE`/`BW_*`; git never prompts.
* `--yes` never deletes anything on its own; `--delete` is the only way to propagate a
  local deletion non-interactively.
