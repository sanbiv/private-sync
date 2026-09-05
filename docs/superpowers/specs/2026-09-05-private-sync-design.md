# private-sync — design spec

Date: 2026-09-05. Status: draft v1 (pre-critique).

## 1. Goal

A Go CLI + TUI (`private-sync`) that finds configuration/secret files inside configured
project directories (`.env`, `*.yaml`, `*.json`, keys, …), stores them **encrypted** in a
local *vault* directory, and synchronises that vault across several computers through
**git** or **Google Drive** (or any folder that is already synced by a desktop client).
Every machine has its own YAML config (different paths, its own machine id). Projects are
matched across machines by *fingerprints* (git remote URL, `go.mod` module, `package.json`
name, …). Sync is three‑way: local ↔ last‑synced base ↔ vault head, with automatic merge
and interactive conflict resolution.

Non‑goals (v1): Google Drive OAuth API client (we use rclone or a Drive‑synced folder),
structural YAML/JSON merge (line based diff3 is used), blob garbage collection, multiple
vaults per config, watching the filesystem.

## 2. User facing surface

Binary: `private-sync`. Module: `github.com/sanbiv/private-sync`. Go 1.27.

### 2.1 CLI (cobra)

| command | purpose |
|---|---|
| `private-sync` | launch the TUI (dashboard). First run → setup wizard. |
| `private-sync init` | interactive setup (config + vault init/open) without the full TUI |
| `private-sync add <path>` | scan a directory, select files (TUI list), associate/create vault project |
| `private-sync scan <path>` | dry‑run: print candidate files with scores |
| `private-sync status [project]` | show per file sync state (no changes) |
| `private-sync sync [project…]` | bidirectional sync with merge |
| `private-sync push [project…]` | local → vault only (remote‑only changes are reported, not applied) |
| `private-sync pull [project…]` | vault → local only (local‑only changes are reported, not applied) |
| `private-sync restore <project>` | force vault head → local for all files (after confirmation / `--yes`) |
| `private-sync projects list|remove <id>` | manage local project mappings |
| `private-sync files add|rm <project> <relpath>` | change tracked file set |
| `private-sync config edit|show|path` | open config in `$EDITOR` / print |
| `private-sync unlock` | test that the key can be obtained and opens the vault |

Global flags: `--config <file>`, `--yes` (non‑interactive, accept defaults), `--strategy
local|remote|abort` (conflict strategy when non‑interactive, default `abort`),
`--no-remote` (skip fetch/push), `--json` for `status`/`scan`.

### 2.2 TUI (Bubble Tea v1 + Bubbles + Lip Gloss + Huh)

Screens:

1. **Setup wizard** (first run): vault path, remote type (`none` / `git` / `rclone`) and its
   parameters, key source (`prompt` / `file` / `bitwarden`) and its parameters, machine name.
   Ends by creating or opening the vault (verifying the key).
2. **Dashboard**: vault + remote summary; list of projects with state badge
   (`synced`, `local changes`, `remote changes`, `conflicts`, `path missing`). Keys:
   `a` add project, `s` sync, `u` push (upload), `d` pull (download), `enter` project
   detail, `c` settings, `r` refresh, `q` quit.
3. **Add project wizard**: path input (with `~` expansion and completion) → scanning
   spinner → candidate list (multi‑select, columns: path, size, reason; high‑score items
   pre‑checked; `+` to type an extra relative path; `/` filter; `t` toggle show‑all) →
   identity: shows detected fingerprints and, if the vault has a matching project,
   proposes to **associate** it (default) or create a new one → name → confirm.
4. **Project detail**: file list with state; `a` add files (re‑scan), `x` untrack file,
   `D` delete file everywhere, `R` restore project, `-` remove project from this machine,
   `s` sync this project.
5. **Sync view**: progress log; when conflicts exist → **Conflict resolver**: for each
   conflicting file, unified diff view (local vs remote, plus the auto‑merge result if
   clean). Choices: `m` use merged (only if clean), `l` keep local, `r` keep remote,
   `e` edit merged text with conflict markers in `$EDITOR`, `k` per‑key resolution (dotenv
   files only), `s` skip file. Then a summary.
6. **Settings**: edit key source, remote, scan include/exclude, machine name.

The TUI never blocks on the key: the key is requested once per process (prompt screen, or
Bitwarden master password prompt when `bw` is locked) and kept in memory.

## 3. Configuration (per machine)

Location: `$XDG_CONFIG_HOME/private-sync/config.yaml`, default `~/.config/private-sync/config.yaml`;
override with `--config` or `PRIVATE_SYNC_CONFIG`. Written by the program (UI) and editable by hand.

```yaml
version: 1
machine:
  id: 7d5f0c8e-…            # uuid generated at first run, never changes
  name: macbook-pro         # human label
vault:
  path: ~/.private-sync/vault      # local vault directory (a git clone when remote is git)
  remote:
    type: git                      # none | git | rclone
    git:
      url: git@github.com:me/private-sync-vault.git
      branch: main
    rclone:
      remote: gdrive               # rclone remote name (e.g. Google Drive)
      path: private-sync-vault     # path inside the remote
key:
  source: bitwarden                # prompt | file | bitwarden
  file:
    path: ~/.private-sync/key      # passphrase file (whitespace trimmed)
  bitwarden:
    item: private-sync vault       # item name or id
    field: password                # password | notes | <custom field name>
scan:
  include: [...]                   # glob patterns on the file name (defaults in §6)
  exclude_dirs: [...]
  exclude_files: [...]
  max_file_size: 2MiB
projects:
  - id: myapp-3f2a               # vault project id
    path: ~/Work/myapp             # local path on THIS machine
```

Env overrides: `PRIVATE_SYNC_PASSPHRASE` (skips the key source), `BW_SESSION` (reused by
the Bitwarden source).

The **tracked file list lives in the vault**, not in the config: a project restored on a
new machine gets all its files without any local list.

## 4. Cryptography

* Passphrase → master key: **Argon2id** (time 3, memory 64 MiB, threads 4, 32 bytes) with a
  16 byte random salt stored in `vault.json`. All key sources produce a passphrase string
  (Bitwarden field, key file content, interactive prompt, env var).
* Master key → HKDF‑SHA256 subkeys: `enc` (32 B), `mac` (32 B), `nonce` (32 B).
* Cipher: **XChaCha20‑Poly1305** (`golang.org/x/crypto/chacha20poly1305`, 24 byte nonce).
* **Blobs are deterministic**: `blobID = hex(HMAC‑SHA256(mac, plaintext))`,
  `nonce = HMAC‑SHA256(nonce, plaintext)[:24]`, AAD = `"blob:" + blobID`. Same plaintext on
  two machines ⇒ byte‑identical `.enc` file ⇒ no git add/add conflicts, no re‑upload.
  Determinism only reveals plaintext equality, which the content‑addressed id reveals anyway.
* **Documents** (journals, metadata) use a random nonce, AAD = the vault‑relative file path
  (prevents swapping files around).
* File format: `"PSV1"` magic + 24 B nonce + ciphertext (with 16 B tag).
* Key check: `vault.json.verifier` = base64 of the *document* encryption of the string
  `private-sync-verifier` with AAD `verifier`. Opening a vault decrypts it to validate the
  passphrase.
* Key material is zeroed on process exit where practical; never written to disk.

## 5. Vault layout and data model

```
<vault>/
  vault.json                          # plaintext: {version, id, kdf{salt,time,memory,threads}, verifier, created_at}
  machines/<machineID>.json.enc       # {id, name, last_seen}
  projects/<projectID>/
      meta/<machineID>.json.enc       # {name, fingerprints[], created_at}  (union across machines)
      state/<machineID>.json.enc      # journal: map relpath → Entry (this machine's latest entry per path)
  blobs/<aa>/<blobID>.enc             # content addressed, deterministic
```

**Rule: a machine only ever writes its own `<machineID>` files plus blobs.** Two machines
therefore never modify the same file, so git merges are always clean and Drive/rclone/desktop
sync never has to resolve anything. All conflict handling happens in the application, with
full information.

```go
type Entry struct {
    Path      string   // slash separated, relative to project root
    Blob      string   // blobID, "" when Deleted or Untracked
    Version   uint64   // lamport clock per path
    Parents   []string // blobIDs this version was derived from (0, 1 or 2)
    Deleted   bool     // propagate deletion (other machines move their copy to trash)
    Untracked bool     // stop syncing, keep local copies
    Mode      uint32   // unix permission bits
    Size      int64
    ModTime   time.Time
    UpdatedAt time.Time
    Machine   string
}
```

**Head resolution** for a (project, path): take the latest entry of every machine; drop
entries whose `Blob` appears in another candidate's `Parents` (superseded); the remaining
set with the maximum `Version` is the head. One element ⇒ head. Several with different
blobs ⇒ *concurrent* (remote‑remote conflict): base = a blob shared by their `Parents`
(if any), else no base. Concurrency is resolved by whoever syncs next writing
`Version = max+1, Parents = [all candidate blobs]`.

A machine writes a new entry only after it has fetched and incorporated the current head
(`Version = head.Version + 1`, `Parents = [head.Blob]`; for a merge `Parents = [head, local base]`).

Project ids: `<slug(name)>-<4 hex random>`. Fingerprints (§7) are the cross‑machine key;
the id is only a stable directory name.

## 6. Scanning

Walk the project directory (no symlink following, skip `exclude_dirs`, size ≤
`max_file_size`). Candidate = file name matches an `include` glob (case insensitive on the
base name) or a *secret‑ish* name pattern.

Default include: `.env`, `.env.*`, `*.env`, `.envrc`, `*.yaml`, `*.yml`, `*.json`, `*.toml`,
`*.ini`, `*.cfg`, `*.conf`, `*.properties`, `.npmrc`, `.yarnrc`, `.yarnrc.yml`, `.pypirc`,
`.netrc`, `*.pem`, `*.key`, `*.crt`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `*.tfvars`,
`*secret*`, `*credential*`, `service-account*.json`, `appsettings.*.json`,
`application-*.yml`, `application-*.yaml`, `application-*.properties`, `*.local.*`,
`wp-config.php`, `.htpasswd`, `*.env.js`, `*.env.ts`.

Default exclude dirs: `.git`, `node_modules`, `vendor`, `dist`, `build`, `out`, `target`,
`.next`, `.nuxt`, `.svelte-kit`, `.venv`, `venv`, `env`, `__pycache__`, `.idea`, `.cache`,
`coverage`, `.terraform`, `bin`, `obj`, `Pods`, `DerivedData`, `.gradle`, `.dart_tool`,
`.turbo`, `.parcel-cache`, `.pytest_cache`, `.mypy_cache`, `tmp`, `logs`.

Default exclude files: `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `composer.lock`,
`Cargo.lock`, `go.sum`, `*.min.json`, `tsconfig*.json`, `*.schema.json`, `.eslintrc*`,
`.prettierrc*`, `renovate.json`, `lerna.json`.

Scoring (drives pre‑selection):

* `High`: secret‑ish name (`.env*`, `*secret*`, `*credential*`, keys/certs, `*.local.*`,
  `.netrc`, `.npmrc`, `.pypirc`, `*.tfvars`) — always pre‑selected.
* `High`: matches include **and** is ignored by git (`git check-ignore --stdin` batch, or a
  built‑in gitignore matcher when `git` is missing) — these are exactly the files git does
  not carry between machines.
* `Low`: matches include but is tracked by git (`git ls-files`) — listed, not pre‑selected.
* `Medium`: matches include, project is not a git repo — listed, not pre‑selected (except
  secret‑ish → High).

Files already tracked in the vault for that project are shown as `tracked` and checked.

## 7. Project identity

Fingerprints, ordered by strength, all stored:

1. `git:<host>/<path>` — normalised `origin` remote (scp/ssh/https → lowercase host, strip
   user, `.git` suffix and trailing slash). Read from `.git/config` (or `git remote get-url`).
2. `go:<module>` from `go.mod`; `npm:<name>` from `package.json`; `cargo:<name>` from
   `Cargo.toml`; `py:<name>` from `pyproject.toml`; `composer:<name>`; `maven:<groupId>:<artifactId>`;
   `gem:<name>` from `*.gemspec`; `swift:<name>` from `Package.swift`; `flutter:<name>` from `pubspec.yaml`.
3. `dir:<basename>` — always present, weakest.

Matching: a local directory matches a vault project if they share any fingerprint of
level 1–2; a `dir:` only match is proposed with a warning. Associating adds the new
fingerprints to this machine's `meta/<machine>.json.enc`.

## 8. Merge

`merge.ThreeWay(path string, base, local, remote []byte) Result` where
`Result{Clean bool, Merged []byte, Hunks []Hunk, Kind: text|dotenv|binary}`.

* **dotenv** (`.env*`, `*.env`, `.envrc`, `*.properties`): parse `KEY=VALUE` lines keeping
  comments/blank lines/order. Key‑level 3‑way: for each key, if `local==base` → take
  remote; if `remote==base` → take local; if `local==remote` → either; else conflict on
  that key. Output keeps local ordering and comments, appends new remote keys at the end.
  Conflicting keys are reported as hunks so the UI can resolve per key.
* **text** (valid UTF‑8, no NUL): line‑level **diff3**: LCS diff base→local and
  base→remote, walk the hunks; non‑overlapping changes are applied, overlapping ones become
  conflict hunks (`<<<<<<< local / ||||||| base / ======= / >>>>>>> remote` when rendered
  for `$EDITOR`). Identical changes on both sides are clean.
* **binary** (or no base for text): not mergeable → conflict, user picks a side.
* Missing base (both sides added the same path): if contents equal → clean; else conflict.

## 9. Sync engine

Inputs per (project, relpath): `local` (hash of the file on disk, or absent), `base`
(from the local state store, or absent), `head` (vault, may be `deleted`/`untracked`/absent).

| local vs base | head vs base | result |
|---|---|---|
| = | = | in sync |
| ≠ | = | **upload** (new blob, entry v+1) |
| = | ≠ | **download** (write file; head deleted → move local file to trash) |
| ≠ | ≠, local = head | **converge** (update base only) |
| ≠ | ≠, local ≠ head | **conflict** → `merge.ThreeWay(base, local, head)` |
| present, no base, no head | — | **upload** (newly tracked) |
| absent, no base, head present | — | **download** |
| present, no base, head present, different | — | conflict without base |
| absent, base present, head = base | — | local deletion → **delete** in vault (only if user removed the file; the engine asks in interactive mode, `--yes` deletes) |

Modes: `sync` applies everything; `push` applies uploads/deletes/conflict resolutions and
only reports downloads; `pull` applies downloads and conflict resolutions and only reports
uploads. Conflicts must be resolved (or skipped) in every mode — a merge result is uploaded
as a new version with two parents, so other machines see it as resolved.

Flow: `Plan` = fetch remote → open vault → compute head map → hash local files → build
items (+ pre‑computed merge results). `Apply(plan, resolutions)` = write local files
(atomic: temp file + rename, preserve mode), write blobs, write this machine's journal
and meta, update the local base store, then `remote.Push`. Trash dir for replaced/deleted
local files: `$XDG_STATE_HOME/private-sync/trash/<timestamp>/<project>/<relpath>`.

Local base store: `$XDG_STATE_HOME/private-sync/vaults/<vaultID>/base.json` —
`{projects: {projectID: {relpath: {blob, version}}}}` (blob ids are HMACs, so nothing
sensitive).

Remote push retry: git push rejected ⇒ `git pull --rebase` ⇒ push again (max 3). Because
every machine writes distinct files, the rebase never conflicts.

## 10. Remotes

```go
type Remote interface {
    Name() string
    Prepare(ctx) error            // clone / init / create folder on first use
    Fetch(ctx, log func(string)) error
    Push(ctx, message string, log func(string)) error
}
```

* `none`: no‑op. Use when `vault.path` is inside a folder synced by Google Drive desktop,
  Dropbox, iCloud, Syncthing… (documented as the simplest Google Drive setup).
* `git`: shells out to `git` (SSH keys / credential helpers just work). `Prepare` clones if
  the path does not exist, or `git init` + `remote add` + initial push if the path exists
  without `.git`. `Fetch` = `git pull --rebase --autostash` (or nothing if the remote is
  empty). `Push` = `git add -A && git commit -m … && git push` with the retry loop.
  Blobs and `.enc` files are marked binary through a generated `.gitattributes`.
* `rclone`: shells out to `rclone` — `Fetch` = `rclone copy <remote>:<path> <vault>`;
  `Push` = `rclone copy <vault> <remote>:<path>` (copy, never `sync --delete`). Works with
  the `drive` backend for Google Drive.

## 11. Key sources

```go
type Source interface { Passphrase(ctx, ui Prompter) ([]byte, error) }
```

* `env`: `PRIVATE_SYNC_PASSPHRASE` (always tried first).
* `prompt`: `ui.Password("Vault passphrase")` (TUI form or terminal prompt).
* `file`: read file, trim whitespace, error if world/group readable (warning only).
* `bitwarden`: `bw status` → if `unauthenticated` error with instructions; if `locked`,
  ask the master password through `ui.Password` and run `bw unlock --raw --passwordenv
  BW_PASSWORD` to get a session for this process; then `bw get item <item> --session …` and
  extract `field` (`password` → `login.password`, `notes` → `notes`, otherwise a custom
  field by name). `BW_SESSION` from the environment is reused when present.

## 12. Package layout

```
cmd/private-sync/main.go
internal/config      load/save/defaults/expand ~, validation
internal/crypto      kdf, subkeys, blob/document encrypt+decrypt, ids
internal/keysource   env, prompt, file, bitwarden + Prompter interface
internal/identity    fingerprints
internal/scan        walk + scoring + git ignore/tracked detection
internal/merge       dotenv + diff3 + dispatch
internal/vault       vault.json, open/create, journals, meta, blobs, head resolution
internal/state       local base store + trash
internal/remote      Remote interface, none, git, rclone
internal/sync        Plan/Apply engine
internal/tui         Bubble Tea app (screens above)
internal/cli         cobra commands wiring everything
```

Every package has unit tests; `internal/sync` has an integration test that simulates two
machines (two config/state dirs, two project dirs, one shared vault dir with remote `none`)
and covers: initial upload, restore on the second machine, one‑sided edits, converging
edits, dotenv key conflict (auto‑resolved), text conflict (strategy local/remote),
deletion propagation, untrack. A git test uses a local bare repository as remote.

## 13. Error handling and safety

* Never overwrite a local file that differs from `base` without resolution; replaced
  files always go to the trash dir first.
* Atomic writes for local files, journals and blobs (temp + rename).
* Wrong passphrase ⇒ clear error before anything is read or written.
* `vault.json` version check; refuse to open newer versions.
* Project path missing on this machine ⇒ project shown as `path missing`, skipped by sync.
* Untracked files are never touched; deletions only move to trash.
