package main

import (
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRealGitCloneFetchAndCompatibility(t *testing.T) {
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
	commit := func(content, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		run("-C", seed, "add", "file.txt")
		run("-C", seed, "-c", "user.name=Gitrelay Test", "-c", "user.email=test@example.invalid",
			"-c", "core.hooksPath="+os.DevNull, "commit", "--no-gpg-sign", "-m", msg)
	}
	for i := 0; i < 60; i++ {
		commit(fmt.Sprintf("v-%d", i), fmt.Sprintf("commit-%d", i))
		run("-C", seed, "branch", fmt.Sprintf("branch-with-long-negotiation-name-%03d", i))
	}
	run("-C", seed, "push", "--all", bare)
	run("--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main")

	var sawGzipUploadPack atomic.Bool
	var sawPushAttempt atomic.Bool
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("client authorization reached git backend")
		}
		if !strings.HasPrefix(r.URL.Path, "/o/r.git/") {
			t.Errorf("upstream path not canonicalized with .git: %s", r.URL.Path)
		}
		if strings.Contains(r.URL.Path, "git-receive-pack") || strings.Contains(r.URL.RawQuery, "git-receive-pack") {
			sawPushAttempt.Store(true)
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Encoding") == "gzip" {
			sawGzipUploadPack.Store(true)
		}
		handler := cgi.Handler{
			Path: backend, Root: "/", Dir: root,
			Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol")},
		}
		handler.ServeHTTP(w, r)
	})
	cfg := testConfig()
	cfg.GitRequestBytes = 1 << 20
	h := newHandler(cfg, handlerOptions{Client: client})
	proxy := httptest.NewServer(h)
	defer proxy.Close()

	for _, tc := range []struct {
		name string
		url  string
		args []string
	}{
		{"with_git_suffix_and_auth_strip", proxy.URL + "/https://github.com/o/r.git", []string{"-c", "http.extraHeader=Authorization: Bearer should-not-leak"}},
		{"without_git_suffix", proxy.URL + "/https://github.com/o/r", nil},
		{"collapsed_single_slash", proxy.URL + "/https:/github.com/o/r", nil},
		{"protocol_v0", proxy.URL + "/https://github.com/o/r", []string{"-c", "protocol.version=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "clone")
			args := append(append([]string(nil), tc.args...), "clone", "--quiet", tc.url, dst)
			run(args...)
			data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
			if err != nil || string(data) != "v-59" {
				t.Fatalf("clone content = %q, err = %v", string(data), err)
			}
		})
	}
	if !sawGzipUploadPack.Load() {
		t.Fatal("expected multi-branch git clone to exercise gzipped POST git-upload-pack")
	}

	fetchClone := filepath.Join(t.TempDir(), "fetch")
	run("clone", "--quiet", proxy.URL+"/https://github.com/o/r", fetchClone)
	commit("updated", "commit-update")
	run("-C", seed, "push", bare, "main")
	run("-C", fetchClone, "fetch", "origin")
	if content := strings.TrimSpace(run("-C", fetchClone, "show", "origin/main:file.txt")); content != "updated" {
		t.Fatalf("fetch content = %q, want updated", content)
	}

	if err := os.WriteFile(filepath.Join(fetchClone, "file.txt"), []byte("push-attempt"), 0600); err != nil {
		t.Fatal(err)
	}
	run("-C", fetchClone, "add", "file.txt")
	run("-C", fetchClone, "-c", "user.name=Gitrelay Test", "-c", "user.email=test@example.invalid",
		"-c", "core.hooksPath="+os.DevNull, "commit", "--no-gpg-sign", "-m", "try-push")
	pushCmd := exec.Command(git, "-C", fetchClone, "push", "origin", "HEAD:main")
	pushCmd.Env = env
	if out, err := pushCmd.CombinedOutput(); err == nil {
		t.Fatalf("git push succeeded unexpectedly:\n%s", out)
	}
	if sawPushAttempt.Load() {
		t.Fatal("git-receive-pack reached upstream backend")
	}
}
