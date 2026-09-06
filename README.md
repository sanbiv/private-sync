# private-sync

A Go CLI + TUI that finds your projects' configuration files and secrets
(`.env`, `*.yaml`, `*.json`, keys, certificates…), stores them **encrypted** in a local
vault, and syncs that vault across computers using **git**, **rclone** (Google
Drive), or a folder already synced by a desktop client (Google Drive, Dropbox,
iCloud, Syncthing).

Each computer has its own YAML configuration (with different paths) and its own
identifier. Projects are recognized across computers using *fingerprints*
(git remote URL, `go.mod` module, name in `package.json`, …). Sync uses a
three-way comparison (local ↔ last synced version ↔ vault), with automatic merging
and interactive conflict resolution.

## Installation

```bash
go install github.com/sanbiv/private-sync/cmd/private-sync@latest
```

Or build from source:

```bash
make build   # produces bin/private-sync
```

Optional requirements: `git` (git remotes and ignored file detection), `rclone`
(Google Drive), `bw` (Bitwarden CLI).

## First run

```bash
private-sync
```

On first run, a wizard guides you through choosing the vault path, remote type,
key source (entered passphrase, key file, Bitwarden), and computer name. Once complete,
the vault is created (or opened if the remote already contains one), and the dashboard opens.

On a second computer, simply run `private-sync` again and provide the same remote and
passphrase: the vault opens, and existing projects appear in the dashboard as
`not linked`. Use `enter` (or `projects link`) to link them to their local folders,
then run `pull` to restore all files.

## Everyday use

| Command | What it does |
|---|---|
| `private-sync` | Interactive dashboard |
| `private-sync add ~/Work/myapp` | Scans the folder, suggests files, and links or creates the project in the vault |
| `private-sync scan ~/Work/myapp` | Previews candidate files without changing anything |
| `private-sync status` | Shows per-file status (no changes made) |
| `private-sync sync` | Bidirectional sync with merging |
| `private-sync push` / `pull` | Local → vault only / vault → local only |
| `private-sync restore myapp` | Restores all project files locally from the vault |
| `private-sync projects list\|link\|unlink` | Manages project links on this computer |
| `private-sync files add\|rm\|delete` | Adds files, stops tracking them, or deletes them everywhere |
| `private-sync trash list\|restore\|purge` | Manages the local encrypted trash |
| `private-sync passphrase change` | Changes the passphrase (no files are re-encrypted) |
| `private-sync config edit\|show\|path` | Configuration |

Global flags: `--config`, `--yes`, `--strategy ask|local|remote|abort`, `--delete`
(propagates local deletions; never implicit), `--no-remote`, `--accept-rollback`,
`--json`.

In the dashboard: `a` add project, `s` sync, `u` push, `d` pull, `r` fetch and
refresh, `enter` details, `c` settings, `q` quit, `esc` back/cancel.

## Configuration (`~/.config/private-sync/config.yaml`)

```yaml
version: 1
machine:
  name: macbook-pro
vault:
  path: ~/.local/share/private-sync/vault
  remote:
    type: git              # none | git | rclone
    git:
      url: git@github.com:me/private-sync-vault.git
      branch: main
    rclone:
      remote: gdrive       # rclone remote name
      path: private-sync-vault
key:
  source: bitwarden        # prompt | file | bitwarden
  file:
    path: ~/.config/private-sync/key
  bitwarden:
    item: private-sync vault
    field: password        # password | notes | <custom field>
scan:
  max_file_size: 2MiB
projects:
  - id: 3f2a9c1e5b7d0a46
    name: myapp
    path: ~/Work/myapp
```

Environment variables: `PRIVATE_SYNC_PASSPHRASE` (for scripts; removed from child
process environments), `PRIVATE_SYNC_CONFIG`, `BW_SESSION`.

### Google Drive

Two options:

1. **Synced folder** (`remote.type: none`): set `vault.path` to a location inside the
   Google Drive for desktop folder ("Mirror files" mode is recommended).
2. **rclone** (`remote.type: rclone`): configure a `drive` remote with `rclone config`
   and specify `remote` and `path`.

The vault is designed so that each computer writes only its own files (plus encrypted,
content-addressed blobs): no conflicts at the git or Drive level.

## How security works

* Passphrase → Argon2id → key that wraps a random vault key
  (`vault.json`). Changing the passphrase only rewraps that key.
* Contents are encrypted with XChaCha20-Poly1305. Blobs are deterministic (same content
  → same file), so two computers never generate conflicts.
* The vault and local state never contain plaintext: even the trash is
  encrypted. Project names, paths, and fingerprints are stored only in encrypted documents.
* Child processes (`git`, `rclone`, `bw`, `$EDITOR`) never inherit passphrases or
  Bitwarden sessions; `git` can never prompt for interactive input.

## How sync works

For each file, the engine compares the local content, the last version synced on
this computer (*base*), and the version in the vault (*head*, calculated using
per-machine version vectors). Changes on only one side are applied; changes on both
sides are merged (per-key merging for `.env` files, line-based diff3 for other text
files). Remaining conflicts are resolved in the TUI (local, remote, per key, or an
external editor). Local deletions are never propagated without `--delete` or
`files delete`; replaced files go into the encrypted trash.

The full specification is in `docs/superpowers/specs/2026-09-05-private-sync-design.md`.

## Useful environment variables

| Variable | Effect |
|---|---|
| `PRIVATE_SYNC_PASSPHRASE` | Passphrase for non-interactive runs (removed from child process environments) |
| `PRIVATE_SYNC_CONFIG` | Alternative configuration file path |
| `PRIVATE_SYNC_BACKGROUND` | `dark` or `light`: sets the TUI palette on terminals that do not respond to background color queries |
| `BW_SESSION` | An already unlocked Bitwarden session, reused by the `bitwarden` source |
