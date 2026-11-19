// tgctl runs one terragrunt/terraform action on one unit, with a log file per run.
//
//	tgctl [UNIT] [ACTION] [--profile P] [--all] [--source-update] [--opt X]... [--resource R]
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var actions = map[string]string{ // action -> terraform flags
	"init": "-no-color", "validate": "-no-color", "plan": "-no-color", "apply": "-no-color -auto-approve",
	"destroy": "-no-color", "output": "-no-color", "state list": "", "state pull": "", "state show": "", "state rm": "",
}
var dangerous = map[string]bool{"apply": true, "destroy": true, "state rm": true}
var actionOpts = map[string][]string{ // --opt values allowed per action
	"init": {"reconfigure", "upgrade", "migrate-state"}, "plan": {"destroy", "refresh-only", "target"},
	"apply": {"destroy", "target"}, "destroy": {"target"},
}
var (
	sourceRe   = regexp.MustCompile(`(?m)^\s*source\s*=`)
	resourceRe = regexp.MustCompile(`^[A-Za-z0-9_.\[\]"\-/]+$`)
	skipDirs   = map[string]bool{".git": true, ".terragrunt-cache": true, ".terraform": true, "node_modules": true, "_template": true}
)

//go:embed assets/icon.svg
var iconSVG []byte

type Unit struct{ Repo, Rel, Abs, Tool string }

type Req struct {
	Action, Profile, Resource string
	Opts                      []string
	All, SourceUpdate         bool
}

const usage = `tgctl [UNIT] [ACTION] [flags]   run one terragrunt/terraform action, log it, stream it

  UNIT      a directory ("." , ./dev/waf, /path) holding terragrunt.hcl, child units (--all) or *.tf,
            or a substring of repo/path discovered under $TGCTL_ROOT (default ~/sources)
  ACTION    init | validate | plan | apply | destroy | output |
            "state list" | "state pull" | "state show" | "state rm"
  --profile P       AWS profile (fzf otherwise, last used for this unit first)
  --all             terragrunt run --all        --source-update   re-download sources
  --opt X           init: reconfigure upgrade migrate-state   plan: destroy refresh-only target
                    apply: destroy target   destroy: target   (repeatable)
  --resource R      address for state show/rm and --opt target
  --version         print version

Missing UNIT/ACTION/profile are picked with fzf. apply, destroy, state rm ask y/N.
Log: $TGCTL_LOGS (default ~/logs)/<repo>/<unit>/<date>/<action>/<branch>/<ts>-<profile>-tf-<action>.log
TGCTL_OPEN=code opens the log when done. While running, the log is also served live on
http://127.0.0.1:<port>/ (printed at start; TGCTL_PORT pins the port).

  tgctl . plan
  tgctl dev/waf apply --profile my-profile
  tgctl prod "state show" --resource 'module.alb.aws_lb.this[0]'
`

var version = "dev" // set by goreleaser -X main.version

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	home, _ := os.UserHomeDir()
	root := env("TGCTL_ROOT", filepath.Join(home, "sources"))
	logRoot := env("TGCTL_LOGS", filepath.Join(home, "logs"))
	var unitQ string
	var r Req
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-h" || a == "--help" || a == "help":
			fmt.Print(usage)
			return nil
		case a == "--version":
			fmt.Println("tgctl", version)
			return nil
		case a == "--all":
			r.All = true
		case a == "--source-update":
			r.SourceUpdate = true
		case a == "--profile":
			r.Profile = next()
		case a == "--resource":
			r.Resource = next()
		case a == "--opt":
			r.Opts = append(r.Opts, next())
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %s\n%s", a, usage)
		case unitQ == "":
			unitQ = a
		case r.Action == "":
			r.Action = a
		}
	}

	// unit: a directory, or an fzf pick among discovered units
	var u Unit
	var err error
	if st, e := os.Stat(unitQ); unitQ != "" && e == nil && st.IsDir() {
		u, err = unitFromDir(unitQ)
	} else {
		units := discover(root)
		lines := make([]string, len(units))
		for i, x := range units {
			lines[i] = x.Repo + "/" + x.Rel
		}
		var pick string
		if pick, err = fzf(lines, "unit> ", unitQ); err == nil {
			u = units[indexOf(lines, pick)]
		}
	}
	if err != nil {
		return err
	}
	if r.Action == "" {
		if r.Action, err = fzf(keys(actions), "action> ", ""); err != nil {
			return err
		}
	}
	memo := filepath.Join(home, ".config", "tgctl", "profiles.json")
	last := map[string]string{}
	if b, e := os.ReadFile(memo); e == nil {
		json.Unmarshal(b, &last)
	}
	if r.Profile == "" {
		out, _ := exec.Command("aws", "configure", "list-profiles").Output()
		ps := strings.Fields(string(out))
		if l := last[u.Repo+"/"+u.Rel]; l != "" {
			ps = append([]string{l}, remove(ps, l)...)
		}
		if r.Profile, err = fzf(ps, "profile> ", ""); err != nil {
			return err
		}
	}
	needRes := r.Action == "state show" || r.Action == "state rm" || indexOf(r.Opts, "target") >= 0
	if needRes && r.Resource == "" {
		fmt.Print("resource address: ")
		fmt.Scanln(&r.Resource)
	}
	argv, err := buildArgs(r, u)
	if err != nil {
		return err
	}
	if dangerous[r.Action] {
		fmt.Printf("Really run %q on %s/%s with %s? [y/N] ", r.Action, u.Repo, u.Rel, r.Profile)
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "yes" {
			return errors.New("aborted")
		}
	}
	if e := ssoValid(); e != nil {
		// awsp (fish) logs in via the shared sso-session; from a subprocess only the login part matters.
		login := exec.Command("aws", "sso", "login", "--profile", r.Profile)
		if exec.Command("fish", "-c", "functions -q awsp").Run() == nil {
			login = exec.Command("fish", "-c", "awsp "+r.Profile)
		}
		fmt.Fprintf(os.Stderr, "profile %s: %v\nLog in now with `%s`? [Y/n] ", r.Profile, e, strings.Join(login.Args, " "))
		var ans string
		fmt.Scanln(&ans)
		if ans == "n" || ans == "no" {
			return errors.New("not logged in")
		}
		login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stderr, os.Stderr
		if err := login.Run(); err != nil {
			return fmt.Errorf("login failed: %v", err)
		}
	}
	sts := exec.Command("aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
	sts.Env = append(os.Environ(), "AWS_PROFILE="+r.Profile)
	if out, e := sts.CombinedOutput(); e != nil {
		return fmt.Errorf("profile %s: not logged in: %s", r.Profile, strings.TrimSpace(string(out)))
	}

	// remember the profile, open the log, run
	last[u.Repo+"/"+u.Rel] = r.Profile
	os.MkdirAll(filepath.Dir(memo), 0o755)
	if b, e := json.MarshalIndent(last, "", "  "); e == nil {
		os.WriteFile(memo, b, 0o644)
	}
	now := time.Now()
	act := strings.ReplaceAll(r.Action, " ", "_")
	dir := filepath.Join(logRoot, u.Repo, u.Rel, now.Format("2006/01/02"), act, gitBranch(u.Abs))
	os.MkdirAll(dir, 0o755)
	logFile := filepath.Join(dir, fmt.Sprintf("%s-%s-tf-%s.log", now.Format("2006-01-02T15-04-05"), r.Profile, act))
	f, err := os.Create(logFile)
	if err != nil {
		return err
	}
	defer f.Close()
	w := io.MultiWriter(f, os.Stdout)
	cmdline := u.Tool + " " + strings.Join(argv, " ")
	fmt.Fprintf(w, "# %s\n# AWS_PROFILE=%s\n# %s\n# log: %s\n\n", now.Format(time.RFC3339), r.Profile, cmdline, logFile)
	c := exec.Command(u.Tool, argv...)
	c.Dir = u.Abs
	c.Env = append(os.Environ(), "AWS_PROFILE="+r.Profile, "TF_IN_AUTOMATION=1", "TF_DATA_DIR=", "TG_NO_COLOR=1", "TG_LOG_FORMAT=bare", "NO_COLOR=1", "TERM=dumb")
	c.Stdout, c.Stderr, c.Stdin = w, w, os.Stdin
	stop := serveLog(logFile)
	runErr := c.Run()
	el := time.Since(now).Round(time.Second)
	if runErr != nil {
		fmt.Fprintf(w, "\n# FAILED after %s: %v\n", el, runErr)
		if b, _ := os.ReadFile(logFile); strings.Contains(string(b), "UnauthorizedException") || strings.Contains(string(b), "No valid credential sources") {
			fmt.Fprintf(os.Stderr, "\n>> AWS SSO session for %s is invalid (the CLI cache hid it): aws sso login --profile %s, then retry\n", r.Profile, r.Profile)
		}
	} else {
		fmt.Fprintf(w, "\n# OK in %s\n", el)
	}
	if op := os.Getenv("TGCTL_OPEN"); op != "" {
		exec.Command(op, logFile).Start()
	}
	stop()
	return runErr
}

// serveLog serves the log on a loopback port with a live-updating page, opens the browser,
// and returns a stop func that waits for Enter in the terminal or the Quit button on the page.
// TGCTL_PORT pins the port (default: random); TGCTL_BROWSER overrides the opener.
func serveLog(logFile string) func() {
	ln, err := net.Listen("tcp", "127.0.0.1:"+env("TGCTL_PORT", "0"))
	if err != nil {
		return func() {}
	}
	quit := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, logPage, filepath.Base(logFile), logFile)
	})
	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(iconSVG)
	})
	mux.HandleFunc("/raw", func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(logFile)
		if r.URL.Query().Get("view") == "" { // fetch() from the page: plain text
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(b)
			return
		}
		// human "raw" view: uncoloured but still live
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><body style="margin:0;background:#1b1d23;color:#d6d8de"><pre id="l" style="padding:12px;font:14px/1.5 ui-monospace,monospace;white-space:pre-wrap"></pre>
<script>let n=0;async function f(){const s=await (await fetch('/raw')).text();if(s.length===n)return;n=s.length;document.getElementById('l').textContent=s;window.scrollTo(0,document.body.scrollHeight);if(/\n# (OK in|FAILED)/.test(s))clearInterval(t)}const t=setInterval(f,1500);f()</script>`)
	})
	mux.HandleFunc("/quit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "bye")
		select {
		case quit <- struct{}{}:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	url := fmt.Sprintf("http://%s/", ln.Addr())
	fmt.Fprintf(os.Stderr, "> live: %s\n", url)
	openBrowser(url)
	return func() {
		fmt.Fprintf(os.Stderr, "> log at %s — press Enter here or Quit on the page\n", url)
		go func() { fmt.Scanln(); quit <- struct{}{} }()
		<-quit
		srv.Close()
	}
}

func openBrowser(url string) {
	cands := [][]string{{env("TGCTL_BROWSER", env("BROWSER", "")), url}, {"wslview", url}, {"xdg-open", url}, {"open", url}, {"cmd.exe", "/c", "start", "", url}}
	for _, c := range cands {
		if c[0] == "" {
			continue
		}
		if _, err := exec.LookPath(c[0]); err == nil {
			exec.Command(c[0], c[1:]...).Start()
			return
		}
	}
}

const logPage = `<!doctype html><html><head><meta charset="utf-8"><title>%s</title><link rel="icon" href="/favicon.svg">
<style>
  body{margin:0;background:#1b1d23;color:#d6d8de;font:14px/1.5 ui-monospace,Menlo,Consolas,monospace}
  header{position:sticky;top:0;background:#24262e;border-bottom:1px solid #3a3d47;padding:8px 14px;display:flex;gap:14px;align-items:center}
  header b{color:#fff} header small{color:#8b8f9c;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  button{background:#3a3d47;color:#fff;border:0;border-radius:4px;padding:5px 10px;cursor:pointer} button.q{background:#a33}
  #st.run{color:#f5c542} #st.ok{color:#5fd36b} #st.fail{color:#ff6b6b}
  pre{margin:0;padding:12px 14px 60px;white-space:pre-wrap;word-break:break-all;tab-size:4}
  .add{color:#5fd36b}.del{color:#ff6b6b}.chg{color:#f5c542}.cmt{color:#8b8f9c}.err{color:#ff6b6b;font-weight:bold}.warn{color:#f5c542}.hd{color:#7fb8ff;font-weight:bold}.plan{color:#fff;font-weight:bold;background:#2e3140;display:inline-block;padding:0 6px}
  .cp{float:right;background:0;padding:0 4px;font-size:12px;line-height:1.2;opacity:.5}.cp:hover{opacity:1}
</style></head><body>
<header><img src="/favicon.svg" width="22" height="22" alt=""><b>%s</b><span id="st" class="run">running</span><small id="path"></small>
<label><input type="checkbox" id="follow" checked> follow</label>
<button title="copy log to clipboard" onclick="fetch('/raw').then(r=>r.text()).then(t=>navigator.clipboard.writeText(t)).then(()=>{this.textContent='copied';setTimeout(()=>this.textContent='copy',1200)})">copy</button>
<button onclick="location.href='/raw?view=1'">raw</button>
<button class="q" onclick="fetch('/quit').then(()=>{clearInterval(t);window.close();document.body.innerHTML='<p style=padding:20px>tgctl server stopped — you can close this tab.</p>'})">Quit server</button></header>
<pre id="log"></pre>
<script>
const esc=s=>s.replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]));
function cls(l){const t=l.trimStart();
  if(/^(#|\s*#) /.test(l)&&/^# /.test(l)) return 'cmt';
  if(/^(Error|│ Error|ERRO|\* Failed)/.test(t)||/# FAILED/.test(l)) return 'err';
  if(/^(Warning|│ Warning|WARN)/.test(t)) return 'warn';
  if(/^Plan:|^Apply complete|^No changes|^# OK in/.test(t)) return 'plan';
  if(/^(\+|-\/\+)/.test(t)) return 'add'; if(/^-/.test(t)&&!/^-{3,}/.test(t)) return 'del'; if(/^~/.test(t)) return 'chg';
  if(/^# .* (will|must) be /.test(t)) return 'hd'; if(/^#/.test(t)) return 'cmt';
  return '';}
let n=0,t=setInterval(load,1500);
const blk=[];function cp(e,i){navigator.clipboard.writeText(blk[i]).then(()=>{e.textContent='✓';setTimeout(()=>e.textContent='📋',1000)})}
async function load(){const r=await fetch('/raw');const s=await r.text();if(s.length===n)return;n=s.length;
  const lines=s.split('\n');document.getElementById('path').textContent=(lines.find(l=>l.startsWith('# log: '))||'').slice(7);
  blk.length=0;let b=-1;
  document.getElementById('log').innerHTML=lines.map(l=>{const c=cls(l);let h=c?'<span class="'+c+'">'+esc(l)+'</span>':esc(l);
    if(c==='hd'){b=blk.push(l)-1;h='<button class="cp" title="copy block" onclick="cp(this,'+b+')">📋</button>'+h}
    else if(b>=0){if(/^\w/.test(l))b=-1;else{blk[b]+='\n'+l;if(/^\s{4}\}\s*$/.test(l))b=-1}}
    return h}).join('\n');
  const st=document.getElementById('st');
  if(/\n# OK in /.test(s)){st.textContent='OK';st.className='ok';clearInterval(t)}else if(/\n# FAILED/.test(s)){st.textContent='FAILED';st.className='fail';clearInterval(t)}
  if(document.getElementById('follow').checked)window.scrollTo(0,document.body.scrollHeight)}
load();
</script></body></html>`

// buildArgs: terragrunt (v0.73+/v1 "run --" syntax) or plain terraform/tofu argv.
func buildArgs(r Req, u Unit) ([]string, error) {
	base, ok := actions[r.Action]
	if !ok {
		return nil, fmt.Errorf("unknown action %q", r.Action)
	}
	needRes := r.Action == "state show" || r.Action == "state rm" || indexOf(r.Opts, "target") >= 0
	if needRes && !resourceRe.MatchString(r.Resource) {
		return nil, errors.New("a valid resource address is required")
	}
	var args []string
	if u.Tool == "terragrunt" {
		args = []string{"--working-dir", u.Abs, "run", "--non-interactive"}
		if r.All {
			args = append(args, "--all")
		}
		if r.SourceUpdate {
			args = append(args, "--source-update")
		}
		args = append(args, "--")
	} else if r.All || r.SourceUpdate {
		return nil, errors.New("--all/--source-update need terragrunt")
	} else {
		args = []string{"-chdir=" + u.Abs}
	}
	args = append(args, strings.Fields(r.Action)...)
	args = append(args, strings.Fields(base)...)
	if r.Action == "state show" || r.Action == "state rm" {
		args = append(args, r.Resource)
	}
	for _, o := range r.Opts {
		if indexOf(actionOpts[r.Action], o) < 0 {
			return nil, fmt.Errorf("option %q not valid for %s", o, r.Action)
		}
		if o == "target" {
			args = append(args, "-target="+r.Resource)
		} else {
			args = append(args, "-"+o)
		}
	}
	return args, nil
}

// discover lists terragrunt units (terragrunt.hcl with a source =) under every first-level dir of root.
func discover(root string) []Unit {
	var units []Unit
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repoDir := filepath.Join(root, e.Name())
		filepath.WalkDir(repoDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "terragrunt.hcl" {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && sourceRe.Match(b) {
				rel, _ := filepath.Rel(repoDir, filepath.Dir(p))
				units = append(units, Unit{e.Name(), rel, filepath.Dir(p), "terragrunt"})
			}
			return nil
		})
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Repo+units[i].Rel < units[j].Repo+units[j].Rel })
	return units
}

// unitFromDir accepts any directory holding a terragrunt unit or plain *.tf files.
func unitFromDir(dir string) (Unit, error) {
	abs, _ := filepath.Abs(dir)
	tool := ""
	if b, err := os.ReadFile(filepath.Join(abs, "terragrunt.hcl")); err == nil && sourceRe.Match(b) {
		tool = "terragrunt"
	} else if hasUnitBelow(abs) { // stack dir: `run --all` walks the child units
		tool = "terragrunt"
	} else if tf, _ := filepath.Glob(filepath.Join(abs, "*.tf")); len(tf) > 0 {
		tool = "terraform"
		if _, err := exec.LookPath("terraform"); err != nil {
			if _, err := exec.LookPath("tofu"); err == nil {
				tool = "tofu"
			}
		}
	} else {
		return Unit{}, fmt.Errorf("%s is neither a terragrunt unit nor a terraform directory", abs)
	}
	top := abs
	c := exec.Command("git", "rev-parse", "--show-toplevel")
	c.Dir = abs
	if out, err := c.Output(); err == nil {
		top = strings.TrimSpace(string(out))
	}
	rel, _ := filepath.Rel(top, abs)
	return Unit{filepath.Base(top), rel, abs, tool}, nil
}

// hasUnitBelow reports whether any subdirectory holds a terragrunt unit.
func hasUnitBelow(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "terragrunt.hcl" && filepath.Dir(p) != dir {
			if b, err := os.ReadFile(p); err == nil && sourceRe.Match(b) {
				found = true
			}
		}
		return nil
	})
	return found
}

func gitBranch(dir string) string {
	c := exec.Command("git", "branch", "--show-current")
	c.Dir = dir
	out, err := c.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return "nobranch"
	}
	return strings.ReplaceAll(strings.TrimSpace(string(out)), "/", "-")
}

// fzf picks one item; resolves without a TTY when query is exact or a single substring hit.
func fzf(items []string, prompt, query string) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("nothing to pick for %s", prompt)
	}
	if query != "" {
		var hits []string
		for _, it := range items {
			if it == query {
				return it, nil
			}
			if strings.Contains(strings.ToLower(it), strings.ToLower(query)) {
				hits = append(hits, it)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
	}
	if _, err := exec.LookPath("fzf"); err != nil {
		return "", fmt.Errorf("%s ambiguous or missing (install fzf or pass it explicitly)", strings.TrimSuffix(prompt, "> "))
	}
	args := []string{"--prompt", prompt, "--height", "40%", "--reverse", "--select-1", "--exit-0"}
	if query != "" {
		args = append(args, "--query", query)
	}
	c := exec.Command("fzf", args...)
	c.Stdin = strings.NewReader(strings.Join(items, "\n"))
	c.Stderr = os.Stderr
	out, err := c.Output()
	s := strings.TrimSpace(string(out))
	if err != nil || s == "" {
		return "", errors.New("nothing selected")
	}
	return s, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func keys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func remove(ss []string, s string) []string {
	var out []string
	for _, x := range ss {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// ssoValid asks IAM Identity Center directly with the newest SSO access token, like terraform does.
// `aws sts get-caller-identity` is not a valid test: the CLI answers from cached role credentials.
// No SSO cache at all (static keys, aws login) => nothing to check.
func ssoValid() error {
	files, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".aws", "sso", "cache", "*.json"))
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a.ModTime().After(b.ModTime())
	})
	for _, f := range files {
		var t struct{ StartUrl, AccessToken, Region string }
		if b, err := os.ReadFile(f); err != nil || json.Unmarshal(b, &t) != nil || t.StartUrl == "" {
			continue
		}
		if t.Region == "" {
			t.Region = env("AWS_REGION", "eu-west-3")
		}
		out, err := exec.Command("aws", "sso", "list-accounts", "--access-token", t.AccessToken, "--region", t.Region, "--max-items", "1").CombinedOutput()
		if err != nil {
			return fmt.Errorf("SSO session invalid: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	return nil
}
