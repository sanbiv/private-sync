# private-sync

CLI + TUI in Go che trova i file di configurazione e i segreti dei tuoi progetti
(`.env`, `*.yaml`, `*.json`, chiavi, certificati…), li salva **cifrati** in un vault
locale e sincronizza quel vault fra più computer tramite **git**, **rclone** (Google
Drive) o una cartella già sincronizzata da un client desktop (Google Drive, Dropbox,
iCloud, Syncthing).

Ogni computer ha la propria configurazione YAML (percorsi diversi) e il proprio
identificativo. I progetti vengono riconosciuti sui vari computer tramite *fingerprint*
(URL del remote git, modulo `go.mod`, nome in `package.json`, …). La sincronizzazione è
a tre vie (locale ↔ ultima versione sincronizzata ↔ vault) con merge automatico e
risoluzione interattiva dei conflitti.

## Installazione

```bash
go install github.com/sanbiv/private-sync/cmd/private-sync@latest
```

oppure dal sorgente:

```bash
make build   # produce bin/private-sync
```

Requisiti opzionali: `git` (remote git e riconoscimento file ignorati), `rclone`
(Google Drive), `bw` (Bitwarden CLI).

## Primo avvio

```bash
private-sync
```

Al primo avvio parte il wizard: percorso del vault, tipo di remote, sorgente della
chiave (passphrase digitata, file di chiave, Bitwarden) e nome del computer. Alla fine il
vault viene creato (o aperto, se il remote ne contiene già uno) e si apre la dashboard.

Sul secondo computer basta ripetere `private-sync`, indicare lo stesso remote e la
stessa passphrase: il vault viene aperto e i progetti già presenti compaiono nella
dashboard come `not linked`. Con `enter` (o `projects link`) li colleghi alla cartella
locale e un `pull` ripristina tutti i file.

## Uso quotidiano

| comando | cosa fa |
|---|---|
| `private-sync` | dashboard interattiva |
| `private-sync add ~/Work/myapp` | scansiona la cartella, propone i file, associa/crea il progetto nel vault |
| `private-sync scan ~/Work/myapp` | anteprima dei candidati senza toccare nulla |
| `private-sync status` | stato per file (nessuna modifica) |
| `private-sync sync` | sincronizzazione bidirezionale con merge |
| `private-sync push` / `pull` | solo locale → vault / solo vault → locale |
| `private-sync restore myapp` | riporta in locale tutti i file del progetto dal vault |
| `private-sync projects list\|link\|unlink` | gestione dei collegamenti su questo computer |
| `private-sync files add\|rm\|delete` | aggiunge, smette di seguire, elimina ovunque |
| `private-sync trash list\|restore\|purge` | cestino cifrato locale |
| `private-sync passphrase change` | cambia la passphrase (nessun file viene ricifrato) |
| `private-sync config edit\|show\|path` | configurazione |

Flag globali: `--config`, `--yes`, `--strategy ask|local|remote|abort`, `--delete`
(propaga le cancellazioni locali, mai implicito), `--no-remote`, `--accept-rollback`,
`--json`.

Nella dashboard: `a` aggiungi progetto, `s` sync, `u` push, `d` pull, `r` fetch e
aggiorna, `enter` dettaglio, `c` impostazioni, `q` esci, `esc` indietro/annulla.

## Configurazione (`~/.config/private-sync/config.yaml`)

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
      remote: gdrive       # nome del remote rclone
      path: private-sync-vault
key:
  source: bitwarden        # prompt | file | bitwarden
  file:
    path: ~/.config/private-sync/key
  bitwarden:
    item: private-sync vault
    field: password        # password | notes | <campo personalizzato>
scan:
  max_file_size: 2MiB
projects:
  - id: 3f2a9c1e5b7d0a46
    name: myapp
    path: ~/Work/myapp
```

Variabili d'ambiente: `PRIVATE_SYNC_PASSPHRASE` (per script; viene rimossa dall'ambiente
dei processi figli), `PRIVATE_SYNC_CONFIG`, `BW_SESSION`.

### Google Drive

Due modi:

1. **Cartella sincronizzata** (`remote.type: none`): imposta `vault.path` dentro la
   cartella di Google Drive per desktop (consigliata la modalità "Mirror files").
2. **rclone** (`remote.type: rclone`): configura un remote `drive` con `rclone config`
   e indica `remote` e `path`.

Il vault è progettato perché ogni computer scriva solo i propri file (più i blob cifrati
indirizzati per contenuto): nessun conflitto a livello di git o di Drive.

## Come funziona la sicurezza

* Passphrase → Argon2id → chiave che avvolge una chiave di vault casuale
  (`vault.json`). Cambiare passphrase riavvolge solo quella chiave.
* Contenuti cifrati con XChaCha20-Poly1305. I blob sono deterministici (stesso contenuto
  → stesso file) così due computer non generano mai conflitti.
* Nel vault e nello stato locale non esiste mai testo in chiaro: anche il cestino è
  cifrato. Nomi, percorsi e fingerprint dei progetti stanno solo in documenti cifrati.
* I processi figli (`git`, `rclone`, `bw`, `$EDITOR`) non ereditano mai passphrase o
  sessioni Bitwarden; `git` non può mai chiedere input interattivo.

## Come funziona la sincronizzazione

Per ogni file il motore confronta: contenuto locale, ultima versione sincronizzata su
questo computer (*base*) e versione nel vault (*head*, calcolata con version vector per
macchina). Modifiche da un solo lato vengono applicate; modifiche da entrambi i lati
vengono unite (merge per chiave sui file `.env`, diff3 a righe sugli altri file di
testo); i conflitti residui si risolvono nella TUI (locale, remoto, per chiave, editor
esterno). Le cancellazioni locali non vengono mai propagate senza `--delete` o
`files delete`; i file sostituiti finiscono nel cestino cifrato.

La specifica completa è in `docs/superpowers/specs/2026-09-05-private-sync-design.md`.
