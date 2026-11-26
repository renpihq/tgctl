<p align="center"><img src="assets/icon-128.png" width="96" alt="tgctl"></p>

# tgctl

One command to run a Terragrunt or Terraform action on a unit, with a timestamped log file, a live browser view, and the AWS SSO check that `aws sts` gets wrong.

```
tgctl . plan
tgctl dev/waf apply --profile my-profile
tgctl prod "state show" --resource 'module.alb.aws_lb.this[0]'
```

Single static binary, Go standard library only.

## Why

Running infrastructure changes by hand leaves no trace and repeats the same chores: pick the right profile, make sure the SSO session is actually valid, remember the terragrunt `run --` syntax, keep the output somewhere reviewable. `tgctl` does those once, the same way, for every repo.

## Features

- **Any unit, anywhere** — a directory holding `terragrunt.hcl` (with a `source`) or plain `*.tf` files. Pass `.`/a path, or a substring of `repo/path` and pick from the units discovered under `$TGCTL_ROOT`.
- **Terragrunt or Terraform** — terragrunt units use `terragrunt run --non-interactive [--all] [--source-update] -- <cmd>`; plain directories use `terraform -chdir` (falls back to `tofu`).
- **Actions** — `init`, `validate`, `plan`, `apply`, `destroy`, `output`, `state list|pull|show|rm`, with the usual options (`-reconfigure`, `-upgrade`, `-migrate-state`, `-destroy`, `-refresh-only`, `-target=…`).
- **Guard rails** — `apply`, `destroy` and `state rm` ask for confirmation; a resource address is validated before `-target`/`state show`/`state rm`.
- **Real SSO check** — before running, the newest token in `~/.aws/sso/cache` is tested against IAM Identity Center (the same call Terraform makes). `aws sts get-caller-identity` is not used as a test: the CLI answers from cached role credentials even after the SSO token is revoked, which is why Terraform fails with `UnauthorizedException` while `aws sts` still works. On failure tgctl offers to log in (`awsp <profile>` if the fish function exists, else `aws sso login --profile`).
- **Profile memory** — the last profile used per unit is stored in `~/.config/tgctl/profiles.json` and proposed first.
- **Logs** — every run writes
  `<logs>/<repo>/<unit>/<yyyy>/<mm>/<dd>/<action>/<branch>/<timestamp>-<profile>-tf-<action>.log`
  with a header (time, profile, exact command, log path) and an `# OK in …` / `# FAILED after …` footer. Output is streamed to the terminal at the same time, colour codes stripped.
- **Live view** — each run serves the log on `http://127.0.0.1:<port>/` and opens it in the browser: Terraform diff colouring (`+` `-` `~`), error/warning highlighting, status badge, follow toggle, raw view, copy to clipboard, and a *Quit server* button. The page stays up after the run until you press Enter in the terminal or Quit on the page.
  A *Rerun* button runs the exact same command again (same unit, profile, options) into a new log file and switches the page to it; `apply`, `destroy` and `state rm` ask for confirmation in the browser first. A rerun never logs in: an expired SSO session is reported as `# FAILED` in the new log.
- **Log browser** — `tgctl logs` lists every log under `$TGCTL_LOGS` (newest first, OK/FAILED badge, substring filter) and opens any of them in the same viewer.

## Install

```sh
go install github.com/renpihq/tgctl@latest
```

That drops `tgctl` into `$(go env GOPATH)/bin` (usually `~/go/bin`); put it on your PATH. Or clone and `go build -o tgctl .`.

Requirements at runtime: `terragrunt` (≥ 0.73, "run --" syntax) and/or `terraform`/`tofu`, `aws` CLI v2, `git`. Optional: `fzf` for interactive picking (only needed when a choice is ambiguous), a browser opener (`wslview`, `xdg-open`, `open` or `cmd.exe` are tried in that order).

## Usage

```
tgctl [UNIT] [ACTION] [flags]

UNIT        directory (".", ./dev/waf, /abs/path) or a substring of repo/path under $TGCTL_ROOT
ACTION      init | validate | plan | apply | destroy | output | "state list" | "state pull" | "state show" | "state rm"

--profile P        AWS profile (default: fzf, last used for this unit first)
--all              terragrunt run --all (whole stack under the unit)
--source-update    re-download module sources
--opt X            repeatable terraform option:
                     init:    reconfigure upgrade migrate-state
                     plan:    destroy refresh-only target
                     apply:   destroy target
                     destroy: target
--resource R       resource address for state show/rm and --opt target
-h, --help
```

Anything missing is picked with fzf. Unambiguous substrings resolve without a prompt, so `tgctl` also works from scripts and CI.

### Examples

```sh
tgctl                                   # pick unit, action, profile interactively
tgctl . init --opt reconfigure --opt upgrade
tgctl infra/live/dev plan --profile my-profile
tgctl dev/waf apply --profile my-profile
tgctl prod "state list" --profile it-aws-admin
tgctl prod plan --opt target --resource 'module.alb.aws_lb.this[0]'
TGCTL_PORT=8921 tgctl . plan            # fixed port for the live page
tgctl logs                              # browse every past log in the browser
```

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `TGCTL_ROOT` | `~/sources` | where repos are discovered (first-level directories, walked for `terragrunt.hcl`) |
| `TGCTL_LOGS` | `~/logs` | log root |
| `TGCTL_PORT` | random | port of the live log page |
| `TGCTL_BROWSER` / `BROWSER` | auto | command used to open the live page (`true` disables) |
| `TGCTL_OPEN` | unset | command run with the log file when the run ends, e.g. `code` |
| `AWS_REGION` | `eu-west-3` | region for the SSO validity call when the token file has none |

State is a single file: `~/.config/tgctl/profiles.json` (unit → last profile).

## How a run works

1. Resolve the unit (directory or discovery), action, profile, resource.
2. Build the argv; refuse invalid combinations (`--all` on plain Terraform, unknown `--opt`, missing resource).
3. Confirm destructive actions.
4. Validate the SSO token against Identity Center; offer login if invalid. Then `aws sts get-caller-identity` with the profile.
5. Create the log file, start the live server, open the browser.
6. Run the tool with `AWS_PROFILE`, `TF_IN_AUTOMATION=1`, `TF_DATA_DIR` unset, colours disabled; tee stdout/stderr to the log and the terminal; stdin is passed through for interactive prompts.
7. Append the footer, optionally run `TGCTL_OPEN`, keep the page until Enter/Quit, exit with the tool's status.

## Development

```sh
go vet ./... && go test ./...     # argv builder tests
go build -o tgctl .
```

`main.go` is the whole program; `assets/icon.svg` is embedded and served as the page favicon.

## Non-goals

No daemon, no remote execution, no state inspection UI, no credential handling beyond selecting a profile. It drives the tools you already have and keeps the evidence.
