# tgctl — agent context

Single-file Go CLI (`main.go`, stdlib only) that runs one terragrunt/terraform action on one unit with a log file and a live browser view. README.md is the user-facing doc; this file is the "why".

## Decisions already made (do not re-litigate)
- CLI only. A daemon + web UI + Caddy + systemd unit were built and then removed on request: "minimal af, just a CLI". The live page per run is the only HTTP left; it dies with the process.
- No hard-coded paths: `TGCTL_ROOT` (default `~/sources`), `TGCTL_LOGS` (`~/logs`), `TGCTL_PORT`, `TGCTL_BROWSER`/`BROWSER`, `TGCTL_OPEN`. Any directory with `terragrunt.hcl` (with `source =`) or `*.tf` works, discovery is only for the fzf picker.
- Terragrunt ≥ 0.73 syntax: `terragrunt --working-dir X run --non-interactive [--all] [--source-update] -- <cmd> [flags]`. Plain dirs: `terraform -chdir=X` (tofu fallback). `--all`/`--source-update` refused on plain terraform.
- SSO check: `aws sts get-caller-identity` is NOT a valid login test — the CLI answers from `~/.aws/cli/cache` while terraform calls SSO `GetRoleCredentials` with the token in `~/.aws/sso/cache` and gets 401 after revocation. `ssoValid()` calls `aws sso list-accounts` with the newest token file. On failure it offers login via `fish -c "awsp <profile>"` (user's function, which now does the same check) else `aws sso login --profile`.
- Log path: `<logs>/<repo>/<unit>/<yyyy/mm/dd>/<action>/<branch>/<ts>-<profile>-tf-<action>.log`, header with exact command, footer `# OK in …` / `# FAILED after …`. Env for the child: `TF_IN_AUTOMATION=1 TF_DATA_DIR= TG_NO_COLOR=1 TG_LOG_FORMAT=bare NO_COLOR=1 TERM=dumb` (terragrunt ignores `-no-color` for its own prefix).
- apply/destroy/state rm: y/N prompt. `apply` runs with `-auto-approve`; the prompt is the gate.
- Live page: `/` (coloured, polls `/raw` every 1.5 s), `/raw` plain text for fetch, `/raw?view=1` live uncoloured view, `/favicon.svg` (embedded `assets/icon.svg`), `/quit`. Server stays up after the run until Enter in the terminal or the Quit button — the user complained twice that the link was dead for fast runs. Quit can't close the tab (browser rule); it replaces the page with a "stopped" notice.
- Profile memory: `~/.config/tgctl/profiles.json`, unit → last profile, proposed first in fzf.
- fzf is optional: exact match or single substring hit resolves without a TTY, so scripts work.

## User (ehab) preferences
- Terse answers, no narration. Does the apply himself; the tool must never apply without the prompt.
- fish shell on WSL; `awsp <profile>` / `awslogin` are his functions in `~/.config/fish/functions/`.
- Wants copy-paste-ready commands, grouped per item, no env-var indirection in instructions.
- Binary symlinked at `~/.local/bin/tgctl`. Rebuild: `go build -o tgctl .` (binary is in-tree, keep it out of any future git repo).

## Verify
`go vet ./... && go test ./...` (argv builder cases) then a real run: `cd <unit> && tgctl . validate --profile <p>`; for the page, drive it under a pty (`python3 -c 'import pty…'`) because Enter is read from stdin.

## Not done / ideas parked
fish completions; history view of past logs; `tgctl . plan` picking profile from the unit's account.yaml; packaging (`go install`, release binary).
