package main

import (
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealGitCloneAndFetch(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable not installed; protocol boundary tests still run")
	}
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	execDir := strings.TrimSpace(run("--exec-path"))
	backend := filepath.Join(execDir, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	root, seed := t.TempDir(), t.TempDir()
	bare := filepath.Join(root, "o", "r.git")
	run("init", "--bare", "--template=", bare)
	run("init", "--template=", seed)
	run("-C", seed, "checkout", "-b", "main")
	commit := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		run("-C", seed, "add", "file.txt")
		run("-C", seed, "-c", "user.name=Gitrelay Test", "-c", "user.email=test@example.invalid",
			"-c", "core.hooksPath="+os.DevNull, "commit", "--no-gpg-sign", "-m", "test")
		run("-C", seed, "push", bare, "main")
	}
	commit("initial")
	run("--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main")
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("proxy auth reached git backend")
		}
		handler := cgi.Handler{
			Path: backend, Root: "/", Dir: root,
			Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol")},
		}
		handler.ServeHTTP(w, r)
	})
	c := testConfig(t)
	s := testService(t, c, client)
	proxy := httptest.NewServer(s)
	defer proxy.Close()
	auth := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
	// Fake test credentials only, kept out of process arguments and never sent
	// to anything except the controlled local proxy.
	env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	clone := filepath.Join(t.TempDir(), "clone")
	run("clone", proxy.URL+"/https://github.com/o/r.git", clone)
	data, err := os.ReadFile(filepath.Join(clone, "file.txt"))
	if err != nil || string(data) != "initial" {
		t.Fatal("clone content mismatch", err)
	}
	commit("updated")
	run("-C", clone, "fetch", "origin")
	if content := strings.TrimSpace(run("-C", clone, "show", "origin/main:file.txt")); content != "updated" {
		t.Fatal("fetch did not receive new commit", content)
	}
	if used, count := s.cache.stats(); used != 0 || count != 0 {
		t.Fatal("real git traffic cached", used, count)
	}
}
