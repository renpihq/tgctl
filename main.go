// tgctl runs one terragrunt/terraform action on one unit, with a log file per run.
//
//	tgctl [UNIT] [ACTION] [--profile P] [--all] [--source-update] [--opt X]... [--resource R]
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

//go:embed ui
var ui embed.FS
var logTmpl = template.Must(template.ParseFS(ui, "ui/log.html"))

// logView feeds ui/log.html: Live = run page (rerun/quit buttons), else the browse view.
type logView struct {
	Title, Log, Raw, Confirm string
	Live                     bool
}

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

  tgctl logs        browse all past logs under $TGCTL_LOGS in the browser (use ./logs for a unit named logs)

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
	if len(args) == 1 && args[0] == "logs" {
		return serveLogs(logRoot)
	}
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
	if err := stsCheck(r.Profile); err != nil {
		return err
	}

	// remember the profile, open the log, run
	last[u.Repo+"/"+u.Rel] = r.Profile
	os.MkdirAll(filepath.Dir(memo), 0o755)
	if b, e := json.MarshalIndent(last, "", "  "); e == nil {
		os.WriteFile(memo, b, 0o644)
	}
	now := time.Now()
	f, err := openLog(logRoot, u, r, argv, now)
	if err != nil {
		return err
	}
	l := &live{logFile: f.Name(), running: true}
	if dangerous[r.Action] {
		l.confirm = fmt.Sprintf("Really rerun %s on %s/%s with %s?", r.Action, u.Repo, u.Rel, r.Profile)
	}
	l.rerun = func() (string, error) {
		now := time.Now()
		f, err := openLog(logRoot, u, r, argv, now)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(term, "\n%s rerun %s %s\n\n", strings.Repeat("─", 30), now.Format("15:04:05"), strings.Repeat("─", 30))
		fmt.Fprintf(os.Stderr, "> rerun: %s\n", f.Name())
		go func() {
			// no interactive login from the browser: a stale session is reported in the log
			e := ssoValid()
			if e == nil {
				e = stsCheck(r.Profile)
			}
			if e != nil {
				fmt.Fprintf(io.MultiWriter(f, term), "\n# FAILED: %v — run: aws sso login --profile %s\n", e, r.Profile)
				f.Close()
			} else {
				e = execute(u, r, argv, f, now, nil)
			}
			l.end(e)
		}()
		return f.Name(), nil
	}
	stop := serveLog(l)
	l.end(execute(u, r, argv, f, now, os.Stdin))
	stop()
	return l.err
}

// term is stdout with per-line ANSI colouring (same rules as the page) when it is a TTY
// and NO_COLOR is unset; the log file always gets plain text.
var term io.Writer = os.Stdout

func init() {
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
		term = &colorWriter{w: os.Stdout}
	}
}

type colorWriter struct {
	w   io.Writer
	buf []byte // partial line not yet flushed
}

var lineColors = []struct {
	re  *regexp.Regexp
	esc string
}{
	{regexp.MustCompile(`^# OK in`), "1;92;48;5;236"},
	{regexp.MustCompile(`^# FAILED`), "1;91;48;5;236"},
	{regexp.MustCompile(`^\s*(Error|│ Error|ERRO|\* Failed)|# FAILED`), "1;91"},
	{regexp.MustCompile(`^\s*(Warning|│ Warning|WARN)`), "33"},
	{regexp.MustCompile(`^\s*(Plan:|Apply complete|No changes)`), "1;97;48;5;236"},
	{regexp.MustCompile(`^\s*# .* (will|must) be `), "1;94"},
	{regexp.MustCompile(`^\s*#`), "90"},
	{regexp.MustCompile(`^\s*(\+|-/\+)`), "92"},
	{regexp.MustCompile(`^\s*-([^-]|$)`), "91"},
	{regexp.MustCompile(`^\s*~`), "93"},
}

func (c *colorWriter) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			break
		}
		line := string(c.buf[:i])
		c.buf = c.buf[i+1:]
		for _, lc := range lineColors {
			if lc.re.MatchString(line) {
				line = "\x1b[" + lc.esc + "m" + line + "\x1b[0m"
				break
			}
		}
		if _, err := io.WriteString(c.w, line+"\n"); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// stsCheck is the cheap sanity check after ssoValid: the profile resolves to an account.
func stsCheck(profile string) error {
	sts := exec.Command("aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text")
	sts.Env = append(os.Environ(), "AWS_PROFILE="+profile)
	if out, e := sts.CombinedOutput(); e != nil {
		return fmt.Errorf("profile %s: not logged in: %s", profile, strings.TrimSpace(string(out)))
	}
	return nil
}

// logPath: <logRoot>/<repo>/<unit>/<yyyy/mm/dd>/<action>/<branch>/<ts>-<profile>-tf-<action>.log
func logPath(logRoot string, u Unit, r Req, now time.Time) string {
	act := strings.ReplaceAll(r.Action, " ", "_")
	return filepath.Join(logRoot, u.Repo, u.Rel, now.Format("2006/01/02"), act, gitBranch(u.Abs),
		fmt.Sprintf("%s-%s-tf-%s.log", now.Format("2006-01-02T15-04-05"), r.Profile, act))
}

// openLog creates the log file for a run and writes its header (also to the terminal).
func openLog(logRoot string, u Unit, r Req, argv []string, now time.Time) (*os.File, error) {
	logFile := logPath(logRoot, u, r, now)
	os.MkdirAll(filepath.Dir(logFile), 0o755)
	f, err := os.Create(logFile)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(io.MultiWriter(f, term), "# %s\n# AWS_PROFILE=%s\n# %s %s\n# log: %s\n\n", now.Format(time.RFC3339), r.Profile, u.Tool, strings.Join(argv, " "), logFile)
	return f, nil
}

// execute runs the tool with output tee'd to f and the terminal, appends the footer, runs TGCTL_OPEN.
// stdin is os.Stdin from the CLI and nil (no TTY) for reruns from the page.
func execute(u Unit, r Req, argv []string, f *os.File, now time.Time, stdin io.Reader) error {
	defer f.Close()
	w := io.MultiWriter(f, term)
	c := exec.Command(u.Tool, argv...)
	c.Dir = u.Abs
	c.Env = append(os.Environ(), "AWS_PROFILE="+r.Profile, "TF_IN_AUTOMATION=1", "TF_DATA_DIR=", "TG_NO_COLOR=1", "TG_LOG_FORMAT=bare", "NO_COLOR=1", "TERM=dumb")
	c.Stdout, c.Stderr, c.Stdin = w, w, stdin
	runErr := c.Run()
	el := time.Since(now).Round(time.Second)
	if runErr != nil {
		fmt.Fprintf(w, "\n# FAILED after %s: %v\n", el, runErr)
		if b, _ := os.ReadFile(f.Name()); strings.Contains(string(b), "UnauthorizedException") || strings.Contains(string(b), "No valid credential sources") {
			fmt.Fprintf(os.Stderr, "\n>> AWS SSO session for %s is invalid (the CLI cache hid it): aws sso login --profile %s, then retry\n", r.Profile, r.Profile)
		}
	} else {
		fmt.Fprintf(w, "\n# OK in %s\n", el)
	}
	if op := os.Getenv("TGCTL_OPEN"); op != "" {
		exec.Command(op, f.Name()).Start()
	}
	return runErr
}

// live is the state behind the log page: the log shown now, whether a run is in progress,
// the result of the last run (= process exit status), and how to start the same run again.
type live struct {
	mu      sync.Mutex
	logFile string
	running bool
	err     error
	confirm string                 // confirm() text on the page for dangerous actions, "" otherwise
	rerun   func() (string, error) // creates the next log file, runs in the background, returns the log path
}

func (l *live) path() string  { l.mu.Lock(); defer l.mu.Unlock(); return l.logFile }
func (l *live) end(err error) { l.mu.Lock(); l.running, l.err = false, err; l.mu.Unlock() }

// serveLog serves the running log with a live-updating page (ui/log.html), opens the browser,
// and returns a stop func that waits for Enter in the terminal or the Quit button on the page.
func serveLog(l *live) func() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		lf := l.path()
		render(w, logView{Title: filepath.Base(lf), Log: lf, Raw: "/raw", Confirm: l.confirm, Live: true})
	})
	mux.HandleFunc("/raw", rawHandler(func(*http.Request) (string, error) { return l.path(), nil }))
	mux.HandleFunc("/rerun", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		l.mu.Lock()
		if l.running {
			l.mu.Unlock()
			http.Error(w, "already running", http.StatusConflict)
			return
		}
		l.running = true
		l.mu.Unlock()
		p, err := l.rerun()
		if err != nil {
			l.end(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		l.mu.Lock()
		l.logFile = p
		l.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, p)
	})
	return serve(mux, "live")
}

// serveLogs is `tgctl logs`: a browser over every log under logRoot (ui/logs.html), each one
// opened in the same viewer as a live run minus rerun/quit.
func serveLogs(logRoot string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := ui.ReadFile("ui/logs.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		}
		list := []entry{}
		filepath.WalkDir(logRoot, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".log") {
				rel, _ := filepath.Rel(logRoot, p)
				list = append(list, entry{filepath.ToSlash(rel), logStatus(p)})
			}
			return nil
		})
		sort.Slice(list, func(i, j int) bool { // newest first: file names start with the timestamp
			return filepath.Base(list[i].Path) > filepath.Base(list[j].Path)
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})
	logFile := underRoot(logRoot)
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		lf, err := logFile(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p := r.URL.Query().Get("p")
		render(w, logView{Title: filepath.Base(p), Log: lf, Raw: "/raw?p=" + url.QueryEscape(p)})
	})
	mux.HandleFunc("/raw", rawHandler(logFile))
	stop := serve(mux, "logs")
	stop()
	return nil
}

// serve adds /favicon.svg and /quit, listens on 127.0.0.1 (TGCTL_PORT pins the port, default random),
// opens the browser (TGCTL_BROWSER overrides) and returns a stop func that waits for Enter or /quit.
func serve(mux *http.ServeMux, what string) func() {
	ln, err := net.Listen("tcp", "127.0.0.1:"+env("TGCTL_PORT", "0"))
	if err != nil {
		return func() {}
	}
	quit := make(chan struct{}, 1)
	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(iconSVG)
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
	fmt.Fprintf(os.Stderr, "> %s: %s\n", what, url)
	openBrowser(url)
	return func() {
		fmt.Fprintf(os.Stderr, "> %s at %s — press Enter here or Quit on the page\n", what, url)
		go func() { fmt.Scanln(); quit <- struct{}{} }()
		<-quit
		srv.Close()
	}
}

// rawHandler serves a log file as text/plain for the page's fetch(), or ui/raw.html with ?view=1.
func rawHandler(file func(*http.Request) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("view") != "" {
			b, _ := ui.ReadFile("ui/raw.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(b)
			return
		}
		p, err := file(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, _ := os.ReadFile(p)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(b)
	}
}

// underRoot resolves ?p=<rel> to a file under root; anything escaping root is rejected.
func underRoot(root string) func(*http.Request) (string, error) {
	return func(r *http.Request) (string, error) {
		p := r.URL.Query().Get("p")
		if p == "" || !filepath.IsLocal(p) {
			return "", errors.New("bad path")
		}
		return filepath.Join(root, p), nil
	}
}

func render(w http.ResponseWriter, v logView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	logTmpl.Execute(w, v)
}

// logStatus reads the footer: ok | fail | run (no footer yet, or killed).
func logStatus(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return "run"
	}
	defer f.Close()
	st, _ := f.Stat()
	buf := make([]byte, 200)
	if st.Size() > 200 {
		f.Seek(st.Size()-200, io.SeekStart)
	}
	n, _ := f.Read(buf)
	switch tail := string(buf[:n]); {
	case strings.Contains(tail, "\n# OK in "):
		return "ok"
	case strings.Contains(tail, "\n# FAILED"):
		return "fail"
	}
	return "run"
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
