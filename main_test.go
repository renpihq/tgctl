package main

import (
	"strings"
	"testing"
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
