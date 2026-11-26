package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs(t *testing.T) {
	tg := Unit{Abs: "/u", Tool: "terragrunt"}
	tf := Unit{Abs: "/u", Tool: "terraform"}
	cases := []struct {
		u    Unit
		req  Req
		want string
		err  bool
	}{
		{tg, Req{Action: "plan"}, "--working-dir /u run --non-interactive -- plan -no-color", false},
		{tg, Req{Action: "apply", All: true}, "--working-dir /u run --non-interactive --all -- apply -no-color -auto-approve", false},
		{tg, Req{Action: "init", Opts: []string{"reconfigure", "upgrade"}, SourceUpdate: true}, "--working-dir /u run --non-interactive --source-update -- init -no-color -reconfigure -upgrade", false},
		{tg, Req{Action: "plan", Opts: []string{"target"}, Resource: "aws_lb.this[0]"}, "--working-dir /u run --non-interactive -- plan -no-color -target=aws_lb.this[0]", false},
		{tg, Req{Action: "plan", Opts: []string{"target"}}, "", true},
		{tg, Req{Action: "state show", Resource: "module.x.aws_instance.y"}, "--working-dir /u run --non-interactive -- state show module.x.aws_instance.y", false},
		{tg, Req{Action: "state rm", Resource: "a.b"}, "--working-dir /u run --non-interactive -- state rm a.b", false},
		{tg, Req{Action: "plan", Opts: []string{"upgrade"}}, "", true},
		{tg, Req{Action: "rm -rf"}, "", true},
		{tf, Req{Action: "plan"}, "-chdir=/u plan -no-color", false},
		{tf, Req{Action: "plan", All: true}, "", true},
	}
	for _, c := range cases {
		got, err := buildArgs(c.req, c.u)
		if (err != nil) != c.err {
			t.Errorf("%+v: err=%v", c.req, err)
			continue
		}
		if !c.err && strings.Join(got, " ") != c.want {
			t.Errorf("%+v:\n got  %s\n want %s", c.req, strings.Join(got, " "), c.want)
		}
	}
}

func TestLogPath(t *testing.T) {
	now := time.Date(2026, 11, 19, 14, 5, 6, 0, time.UTC)
	got := logPath("/logs", Unit{Repo: "infra", Rel: "dev/waf", Abs: "/nonexistent"}, Req{Action: "state rm", Profile: "p"}, now)
	if want := "/logs/infra/dev/waf/2026/11/19/state_rm/nobranch/2026-11-19T14-05-06-p-tf-state_rm.log"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestColorWriter(t *testing.T) {
	var out bytes.Buffer
	w := &colorWriter{w: &out}
	// split writes mid-line: only complete lines are coloured and flushed
	w.Write([]byte("  # aws_x.this will be upd"))
	w.Write([]byte("ated in-place\n      + a = 1\n# OK in 3s\nplain\n"))
	got := out.String()
	for _, want := range []string{
		"\x1b[1;94m  # aws_x.this will be updated in-place\x1b[0m\n",
		"\x1b[92m      + a = 1\x1b[0m\n",
		"\x1b[1;92;48;5;236m# OK in 3s\x1b[0m\n",
		"plain\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestLogPage(t *testing.T) {
	rec := httptest.NewRecorder()
	render(rec, logView{Title: "x.log", Log: "/l/x.log", Raw: "/raw", Confirm: `Really rerun "apply"?`})
	live := rec.Body.String()
	for _, c := range []struct{ s, want string }{
		{live, "const RAW=\"/raw\",confirmMsg=\"Really rerun \\\"apply\\\"?\";"}, {live, `id="rrb"`}, {live, `/quit`},
	} {
		if !strings.Contains(c.s, c.want) {
			t.Errorf("missing %q", c.want)
		}
	}
	if strings.Contains(live, "ZgotmplZ") {
		t.Errorf("template mangled: %s", live)
	}
}
