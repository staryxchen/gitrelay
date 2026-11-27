package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func TestHealthcheckURL(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		ok       bool
	}{
		{"", "http://127.0.0.1:8080/healthz", true},
		{"127.0.0.1:18080", "http://127.0.0.1:18080/healthz", true},
		{"[::1]:9090", "http://[::1]:9090/healthz", true},
		{"bad", "", false},
	} {
		got, err := healthcheckURL(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("healthcheckURL(%q) = (%q, %v), want (%q, ok=%v)", tt.in, got, err, tt.want, tt.ok)
		}
	}
}

func TestParseCommand(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want cliCommand
		ok   bool
	}{
		{nil, cmdServe, true},
		{[]string{}, cmdServe, true},
		{[]string{"healthcheck"}, cmdHealthcheck, true},
		{[]string{"--version"}, cmdVersion, true},
		{[]string{"version"}, cmdVersion, true},
		{[]string{"-v"}, 0, false},
		{[]string{"-version"}, 0, false},
		{[]string{"--help"}, 0, false},
		{[]string{"serve"}, 0, false},
		{[]string{"--version", "extra"}, 0, false},
		{[]string{"version", "extra"}, 0, false},
		{[]string{"healthcheck", "extra"}, 0, false},
	} {
		got, err := parseCommand(tt.args)
		if (err == nil) != tt.ok || (tt.ok && got != tt.want) {
			t.Errorf("parseCommand(%q) = (%v, %v), want (%v, ok=%v)", tt.args, got, err, tt.want, tt.ok)
		}
	}
}

func TestResolveBuildInfoAndString(t *testing.T) {
	const sha = "ba17b5c6e5f955ecca2f934455391465344b22c2"
	cleanBI := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: sha},
			{Key: "vcs.modified", Value: "false"},
			{Key: "vcs.time", Value: "2026-11-27T14:50:50Z"},
		},
	}
	dirtyBI := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: sha},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	emptyRevBI := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.modified", Value: "true"},
		},
	}

	for _, tt := range []struct {
		name            string
		ver, rev, built string
		bi              *debug.BuildInfo
		want            buildInfo
		wantStr         string
	}{
		{
			name:    "defaults without buildinfo",
			ver:     "dev",
			rev:     "",
			built:   "unknown",
			bi:      nil,
			want:    buildInfo{Version: "dev", Commit: "unknown", BuildTime: "unknown"},
			wantStr: "gitrelay dev (commit unknown, built unknown)",
		},
		{
			name:    "whitespace falls back",
			ver:     "  ",
			rev:     "\t",
			built:   " ",
			bi:      emptyRevBI,
			want:    buildInfo{Version: "dev", Commit: "unknown", BuildTime: "unknown"},
			wantStr: "gitrelay dev (commit unknown, built unknown)",
		},
		{
			name:    "clean vcs fallback ignores vcs.time",
			ver:     "dev",
			rev:     "",
			built:   "unknown",
			bi:      cleanBI,
			want:    buildInfo{Version: "dev", Commit: sha, BuildTime: "unknown"},
			wantStr: "gitrelay dev (commit " + sha + ", built unknown)",
		},
		{
			name:    "explicit unknown commit not clobbered by vcs",
			ver:     "dev",
			rev:     "unknown",
			built:   "unknown",
			bi:      cleanBI,
			want:    buildInfo{Version: "dev", Commit: "unknown", BuildTime: "unknown"},
			wantStr: "gitrelay dev (commit unknown, built unknown)",
		},
		{
			name:    "dirty vcs fallback",
			ver:     "",
			rev:     "",
			built:   "",
			bi:      dirtyBI,
			want:    buildInfo{Version: "dev", Commit: sha + "-dirty", BuildTime: "unknown"},
			wantStr: "gitrelay dev (commit " + sha + "-dirty, built unknown)",
		},
		{
			name:    "explicit ldflags preserved verbatim",
			ver:     " v1.2.3 ",
			rev:     " 0123456789abcdef0123456789abcdef01234567 ",
			built:   " 2026-11-27T15:00:00Z ",
			bi:      dirtyBI,
			want:    buildInfo{Version: "v1.2.3", Commit: "0123456789abcdef0123456789abcdef01234567", BuildTime: "2026-11-27T15:00:00Z"},
			wantStr: "gitrelay v1.2.3 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-11-27T15:00:00Z)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBuildInfo(tt.ver, tt.rev, tt.built, tt.bi)
			if got != tt.want {
				t.Fatalf("resolveBuildInfo() = %+v, want %+v", got, tt.want)
			}
			if got.String() != tt.wantStr {
				t.Fatalf("String() = %q, want %q", got.String(), tt.wantStr)
			}
		})
	}
}

func TestRunCLI(t *testing.T) {
	info := buildInfo{
		Version:   "v0.1.0",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		BuildTime: "2026-11-27T15:00:00Z",
	}
	wantLine := "gitrelay v0.1.0 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-11-27T15:00:00Z)\n"

	t.Run("version and --version match and ignore invalid LISTEN_ADDR", func(t *testing.T) {
		t.Setenv("LISTEN_ADDR", "invalid")
		var outFlag, errFlag bytes.Buffer
		codeFlag := runCLI(
			[]string{"--version"},
			&outFlag,
			&errFlag,
			info,
			func() error { return checkHealth(os.Getenv("LISTEN_ADDR")) },
			func() error {
				_, err := loadConfig()
				return err
			},
		)
		if codeFlag != 0 || outFlag.String() != wantLine || errFlag.Len() != 0 {
			t.Fatalf("--version = (code=%d, stdout=%q, stderr=%q)", codeFlag, outFlag.String(), errFlag.String())
		}

		var outSub, errSub bytes.Buffer
		codeSub := runCLI(
			[]string{"version"},
			&outSub,
			&errSub,
			info,
			func() error { return errors.New("unexpected healthcheck call") },
			func() error { return errors.New("unexpected server call") },
		)
		if codeSub != 0 || outSub.String() != outFlag.String() || errSub.Len() != 0 {
			t.Fatalf("version = (code=%d, stdout=%q, stderr=%q)", codeSub, outSub.String(), errSub.String())
		}
	})

	t.Run("unknown arguments exit 2 and write stderr", func(t *testing.T) {
		for _, args := range [][]string{{"--unknown"}, {"version", "extra"}} {
			var stdout, stderr bytes.Buffer
			called := false
			code := runCLI(
				args,
				&stdout,
				&stderr,
				info,
				func() error { called = true; return nil },
				func() error { called = true; return nil },
			)
			if code != 2 || called || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("runCLI(%q) = (code=%d, called=%v, stdout=%q, stderr=%q)", args, code, called, stdout.String(), stderr.String())
			}
		}
	})

	t.Run("healthcheck and server propagate status codes", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runCLI([]string{"healthcheck"}, &stdout, &stderr, info, func() error { return nil }, func() error { return errors.New("boom") }); code != 0 {
			t.Fatalf("healthcheck ok code = %d, want 0", code)
		}
		if code := runCLI([]string{"healthcheck"}, &stdout, &stderr, info, func() error { return errors.New("down") }, func() error { return nil }); code != 1 {
			t.Fatalf("healthcheck fail code = %d, want 1", code)
		}
		if code := runCLI(nil, &stdout, &stderr, info, func() error { return errors.New("down") }, func() error { return nil }); code != 0 {
			t.Fatalf("serve ok code = %d, want 0", code)
		}
		if code := runCLI(nil, &stdout, &stderr, info, func() error { return nil }, func() error { return errors.New("bind failed") }); code != 1 {
			t.Fatalf("serve fail code = %d, want 1", code)
		}
	})
}

func TestVersionLDFlags(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gitrelay")
	const (
		injVer   = "v1.2.3"
		injSHA   = "0123456789abcdef0123456789abcdef01234567"
		injBuilt = "2026-11-27T15:00:00Z"
		wantOut  = "gitrelay v1.2.3 (commit 0123456789abcdef0123456789abcdef01234567, built 2026-11-27T15:00:00Z)\n"
	)
	ldflags := "-X main.version=" + injVer + " -X main.commit=" + injSHA + " -X main.buildTime=" + injBuilt
	build := exec.Command("go", "build", "-ldflags="+ldflags, "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -ldflags failed: %v\n%s", err, out)
	}

	for _, arg := range []string{"--version", "version"} {
		stdout, stderr, exitCode := execBinary(t, bin, []string{"LISTEN_ADDR=invalid"}, arg)
		if exitCode != 0 || stdout != wantOut || stderr != "" {
			t.Fatalf("%s = (code=%d, stdout=%q, stderr=%q), want (0, %q, \"\")", arg, exitCode, stdout, stderr, wantOut)
		}
	}

	for _, badArgs := range [][]string{{"--unknown-flag"}, {"version", "extra"}} {
		stdout, stderr, exitCode := execBinary(t, bin, nil, badArgs...)
		if exitCode != 2 || stdout != "" || stderr == "" {
			t.Fatalf("%q = (code=%d, stdout=%q, stderr=%q), want exit 2 and stderr", badArgs, exitCode, stdout, stderr)
		}
	}
}

func execBinary(t *testing.T, bin string, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}
	t.Fatalf("exec %s %v failed unexpectedly: %v", bin, args, err)
	return "", "", -1
}
