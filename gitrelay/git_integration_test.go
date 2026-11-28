package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRealGitAuthPassthrough(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable not installed; protocol boundary tests still run")
	}
	const authUser, authPAT, wrongPAT = "x-access-token", "ghp_integration_secret", "ghp_wrong_secret"
	validAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(authUser+":"+authPAT))
	cred := func(key string) string {
		return `!f() { if [ "$1" = get ]; then printf "username=x-access-token\npassword=%s\n" "$` + key + `"; fi; }; f`
	}
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "TEST_PAT="+authPAT, "TEST_WRONG_PAT="+wrongPAT)
	run := func(ok bool, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ok != (err == nil) {
			t.Fatalf("git %v ok=%v: %v\n%s", args, ok, err, out)
		}
		return string(out)
	}
	backend := filepath.Join(strings.TrimSpace(run(true, "--exec-path")), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	root, seed := t.TempDir(), t.TempDir()
	bare := filepath.Join(root, "o", "priv.git")
	run(true, "init", "--bare", "--template=", bare)
	run(true, "init", "--template=", seed)
	run(true, "-C", seed, "checkout", "-b", "main")
	commit := func(dir, content, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		run(true, "-C", dir, "add", "file.txt")
		run(true, "-C", dir, "-c", "user.name=Gitrelay Test", "-c", "user.email=test@example.invalid",
			"-c", "core.hooksPath="+os.DevNull, "commit", "--no-gpg-sign", "-m", msg)
	}
	commit(seed, "private-v1", "init")
	run(true, "-C", seed, "push", bare, "HEAD:main")
	run(true, "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main")

	var challenges, authedRefs, authedPack, leakedAuth, receivePack atomic.Int64
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "git-receive-pack") || strings.Contains(r.URL.RawQuery, "git-receive-pack") {
			receivePack.Add(1)
			http.Error(w, "no push", http.StatusForbidden)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "" {
			leakedAuth.Add(1)
		}
		if auth == validAuth {
			if r.Method == http.MethodGet && r.URL.Path == "/o/priv.git/info/refs" {
				authedRefs.Add(1)
			}
			if r.Method == http.MethodPost && r.URL.Path == "/o/priv.git/git-upload-pack" {
				authedPack.Add(1)
			}
			(&cgi.Handler{Path: backend, Root: "/", Dir: root, Env: []string{
				"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"),
			}}).ServeHTTP(w, r)
			return
		}
		challenges.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
	var proxy *httptest.Server
	t.Cleanup(func() {
		if proxy != nil {
			proxy.Close()
		}
	})
	serve := func(pass bool) *syncBuffer {
		t.Helper()
		if proxy != nil {
			proxy.Close()
		}
		for _, n := range []*atomic.Int64{&challenges, &authedRefs, &authedPack, &leakedAuth, &receivePack} {
			n.Store(0)
		}
		logs := &syncBuffer{}
		cfg := testConfig()
		cfg.AuthPassthrough, cfg.GitRequestBytes = pass, 1<<20
		proxy = httptest.NewServer(newHandler(cfg, handlerOptions{
			Client: client, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		}))
		return logs
	}
	finish := func(logs *syncBuffer) string {
		t.Helper()
		proxy.Close()
		return logs.String()
	}
	omit := func(logs, pat string) {
		t.Helper()
		for _, secret := range []string{pat, base64.StdEncoding.EncodeToString([]byte(pat)), base64.StdEncoding.EncodeToString([]byte(authUser + ":" + pat))} {
			if strings.Contains(logs, secret) {
				t.Fatal("credential appeared in logs")
			}
		}
		if !strings.Contains(logs, `"msg":"request"`) {
			t.Fatalf("missing request log: %s", logs)
		}
	}

	logs := serve(true)
	dst := filepath.Join(t.TempDir(), "clone")
	repo := "/https://github.com/o/priv"
	run(true, "-c", "credential.helper="+cred("TEST_PAT"), "clone", "--quiet", proxy.URL+repo, dst)
	if data, err := os.ReadFile(filepath.Join(dst, "file.txt")); err != nil || string(data) != "private-v1" {
		t.Fatalf("clone content = %q, err = %v", data, err)
	}
	commit(seed, "private-v2", "update")
	run(true, "-C", seed, "push", bare, "HEAD:main")
	run(true, "-C", dst, "-c", "credential.helper="+cred("TEST_PAT"), "fetch", "origin")
	if content := strings.TrimSpace(run(true, "-C", dst, "show", "origin/main:file.txt")); content != "private-v2" {
		t.Fatalf("fetch content = %q, want private-v2", content)
	}
	text := finish(logs)
	if challenges.Load() == 0 || authedRefs.Load() == 0 || authedPack.Load() == 0 {
		t.Fatalf("challenges=%d authedRefs=%d authedPack=%d", challenges.Load(), authedRefs.Load(), authedPack.Load())
	}
	omit(text, authPAT)

	logs = serve(true)
	run(false, "-c", "credential.helper="+cred("TEST_WRONG_PAT"), "clone", "--quiet", proxy.URL+repo, filepath.Join(t.TempDir(), "wrong"))
	text = finish(logs)
	if leakedAuth.Load() == 0 || authedRefs.Load() != 0 || authedPack.Load() != 0 {
		t.Fatalf("wrong credential leakedAuth=%d authedRefs=%d authedPack=%d", leakedAuth.Load(), authedRefs.Load(), authedPack.Load())
	}
	omit(text, wrongPAT)

	logs = serve(false)
	run(false, "-c", "credential.helper="+cred("TEST_PAT"), "clone", "--quiet", proxy.URL+repo, filepath.Join(t.TempDir(), "disabled-helper"))
	run(false, "-c", "http.extraHeader=Authorization: "+validAuth, "clone", "--quiet", proxy.URL+repo, filepath.Join(t.TempDir(), "disabled-header"))
	text = finish(logs)
	if leakedAuth.Load() != 0 || challenges.Load() == 0 {
		t.Fatalf("disabled leakedAuth=%d challenges=%d", leakedAuth.Load(), challenges.Load())
	}
	omit(text, authPAT)

	logs = serve(true)
	run(true, "-C", dst, "remote", "set-url", "origin", proxy.URL+repo)
	commit(dst, "push-attempt", "try-push")
	run(false, "-C", dst, "-c", "credential.helper="+cred("TEST_PAT"), "push", "origin", "HEAD:main")
	text = finish(logs)
	if receivePack.Load() != 0 || !strings.Contains(text, `"status":400`) {
		t.Fatalf("push receivePack=%d logs=%s", receivePack.Load(), text)
	}
	omit(text, authPAT)
}

type upstreamHit struct {
	method, path, host, auth string
}

func TestRealGitRedirectRenameAndSecurity(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable not installed; protocol boundary tests still run")
	}
	const authUser, authPAT = "x-access-token", "ghp_redirect_secret"
	validAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(authUser+":"+authPAT))
	cred := func(key string) string {
		return `!f() { if [ "$1" = get ]; then printf "username=x-access-token\npassword=%s\n" "$` + key + `"; fi; }; f`
	}
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "TEST_PAT="+authPAT)
	runGit := func(t *testing.T, timeout time.Duration, wantOK bool, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("git %v timed out: %v\n%s", args, ctx.Err(), out)
		}
		if wantOK != (err == nil) {
			t.Fatalf("git %v ok=%v: %v\n%s", args, wantOK, err, out)
		}
		return string(out)
	}
	backend := filepath.Join(strings.TrimSpace(runGit(t, 30*time.Second, true, "--exec-path")), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	root, seed := t.TempDir(), t.TempDir()
	newBare := filepath.Join(root, "neworg", "newrepo.git")
	privBare := filepath.Join(root, "neworg", "priv.git")
	runGit(t, 30*time.Second, true, "init", "--bare", "--template=", newBare)
	runGit(t, 30*time.Second, true, "init", "--bare", "--template=", privBare)
	runGit(t, 30*time.Second, true, "init", "--template=", seed)
	runGit(t, 30*time.Second, true, "-C", seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("redirect-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, 30*time.Second, true, "-C", seed, "add", "file.txt")
	runGit(t, 30*time.Second, true, "-C", seed, "-c", "user.name=Gitrelay Test", "-c", "user.email=test@example.invalid",
		"-c", "core.hooksPath="+os.DevNull, "commit", "--no-gpg-sign", "-m", "init")
	runGit(t, 30*time.Second, true, "-C", seed, "push", newBare, "HEAD:main")
	runGit(t, 30*time.Second, true, "-C", seed, "push", privBare, "HEAD:main")
	runGit(t, 30*time.Second, true, "--git-dir="+newBare, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, 30*time.Second, true, "--git-dir="+privBare, "symbolic-ref", "HEAD", "refs/heads/main")

	redirects := map[string]struct {
		status int
		loc    string
	}{
		"/oldorg/oldrepo.git/info/refs": {http.StatusMovedPermanently, "https://github.com/neworg/newrepo.git/info/refs?service=git-upload-pack"},
		"/hop1/r.git/info/refs":         {http.StatusMovedPermanently, "https://github.com/hop2/r.git/info/refs?service=git-upload-pack"},
		"/hop2/r.git/info/refs":         {http.StatusFound, "https://github.com/neworg/newrepo.git/info/refs?service=git-upload-pack"},
		"/self/r.git/info/refs":         {http.StatusMovedPermanently, "https://github.com/self/r.git/info/refs?service=git-upload-pack"},
		"/loopa/r.git/info/refs":        {http.StatusFound, "https://github.com/loopb/r.git/info/refs?service=git-upload-pack"},
		"/loopb/r.git/info/refs":        {http.StatusFound, "https://github.com/loopa/r.git/info/refs?service=git-upload-pack"},
		"/evil/r.git/info/refs":         {http.StatusFound, "https://evil.example/offhost/leak.git/info/refs?service=git-upload-pack"},
		"/oldorg/priv.git/info/refs":    {http.StatusMovedPermanently, "https://github.com/neworg/priv.git/info/refs?service=git-upload-pack"},
	}
	var hitMu sync.Mutex
	var hits []upstreamHit
	resetHits := func() {
		hitMu.Lock()
		hits = nil
		hitMu.Unlock()
	}
	snapshot := func() []upstreamHit {
		hitMu.Lock()
		defer hitMu.Unlock()
		out := make([]upstreamHit, len(hits))
		copy(out, hits)
		return out
	}
	serveGit := func(w http.ResponseWriter, r *http.Request) {
		(&cgi.Handler{Path: backend, Root: "/", Dir: root, Env: []string{
			"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"),
		}}).ServeHTTP(w, r)
	}
	client := localUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hitMu.Lock()
		hits = append(hits, upstreamHit{r.Method, r.URL.Path, r.Host, r.Header.Get("Authorization")})
		hitMu.Unlock()
		if spec, ok := redirects[r.URL.Path]; ok && r.URL.RawQuery == "service=git-upload-pack" {
			w.Header().Set("Location", spec.loc)
			w.WriteHeader(spec.status)
			w.Write([]byte("redirect-body"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/neworg/priv.git/") && r.Header.Get("Authorization") != validAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/neworg/newrepo.git/") || strings.HasPrefix(r.URL.Path, "/neworg/priv.git/") {
			serveGit(w, r)
			return
		}
		http.NotFound(w, r)
	})
	start := func(t *testing.T, mutate func(*config)) (*httptest.Server, *syncBuffer) {
		t.Helper()
		logs := &syncBuffer{}
		cfg := testConfig()
		cfg.GitRequestBytes = 1 << 20
		if mutate != nil {
			mutate(&cfg)
		}
		proxy := httptest.NewServer(newHandler(cfg, handlerOptions{
			Client: client, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		}))
		t.Cleanup(proxy.Close)
		return proxy, logs
	}
	omitPAT := func(t *testing.T, text string) {
		t.Helper()
		for _, secret := range []string{
			authPAT,
			base64.StdEncoding.EncodeToString([]byte(authPAT)),
			base64.StdEncoding.EncodeToString([]byte(authUser + ":" + authPAT)),
		} {
			if strings.Contains(text, secret) {
				t.Fatal("credential appeared in logs or output")
			}
		}
	}
	assertPosts := func(t *testing.T, repo string) {
		t.Helper()
		got := snapshot()
		posts := 0
		for _, hit := range got {
			if hit.method != http.MethodPost {
				continue
			}
			posts++
			if hit.path != repo || strings.Contains(hit.host, "evil.example") {
				t.Fatalf("POST path=%s host=%s hits=%+v", hit.path, hit.host, got)
			}
		}
		if posts == 0 {
			t.Fatalf("no POST, hits=%+v", got)
		}
	}

	proxy, _ := start(t, nil)
	work := t.TempDir()
	reused := filepath.Join(work, "bare")
	for _, tc := range []struct {
		name, raw string
		gets      []string
	}{
		{"bare", "/https://github.com/oldorg/oldrepo", []string{"/oldorg/oldrepo.git/info/refs"}},
		{"collapsed", "/https:/github.com/oldorg/oldrepo", []string{"/oldorg/oldrepo.git/info/refs"}},
		{"two_hop", "/https://github.com/hop1/r", []string{"/hop1/r.git/info/refs", "/hop2/r.git/info/refs"}},
	} {
		t.Run("clone/"+tc.name, func(t *testing.T) {
			resetHits()
			dst := filepath.Join(work, tc.name)
			runGit(t, 30*time.Second, true, "clone", "--quiet", proxy.URL+tc.raw, dst)
			data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
			if err != nil || string(data) != "redirect-v1" {
				t.Fatalf("content=%q err=%v", data, err)
			}
			got := snapshot()
			saw := make(map[string]bool, len(got))
			for _, hit := range got {
				if hit.method == http.MethodGet {
					saw[hit.path] = true
				}
			}
			for _, path := range tc.gets {
				if !saw[path] {
					t.Fatalf("missing GET %s hits=%+v", path, got)
				}
			}
			assertPosts(t, "/neworg/newrepo.git/git-upload-pack")
		})
	}

	t.Run("fetch", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("redirect-v2"), 0600); err != nil {
			t.Fatal(err)
		}
		runGit(t, 30*time.Second, true, "-C", seed, "add", "file.txt")
		runGit(t, 30*time.Second, true, "-C", seed, "-c", "user.name=Gitrelay Test", "-c", "user.email=test@example.invalid",
			"-c", "core.hooksPath="+os.DevNull, "commit", "--no-gpg-sign", "-m", "update")
		runGit(t, 30*time.Second, true, "-C", seed, "push", newBare, "HEAD:main")
		resetHits()
		runGit(t, 30*time.Second, true, "-C", reused, "fetch", "origin")
		if content := strings.TrimSpace(runGit(t, 30*time.Second, true, "-C", reused, "show", "origin/main:file.txt")); content != "redirect-v2" {
			t.Fatalf("fetch content=%q", content)
		}
		assertPosts(t, "/neworg/newrepo.git/git-upload-pack")
	})

	t.Run("self_loop", func(t *testing.T) {
		resetHits()
		loopProxy, logs := start(t, nil)
		out := runGit(t, 30*time.Second, false, "clone", loopProxy.URL+"/https://github.com/self/r", filepath.Join(t.TempDir(), "self"))
		got := snapshot()
		if len(got) != 1 || got[0].method != http.MethodGet || got[0].path != "/self/r.git/info/refs" {
			t.Fatalf("hits=%+v out=%s", got, out)
		}
		if !strings.Contains(out, "502") || !strings.Contains(logs.String(), `"error_kind":"upstream_redirect_loop"`) {
			t.Fatalf("out=%s logs=%s", out, logs.String())
		}
	})

	t.Run("cycle", func(t *testing.T) {
		resetHits()
		out := runGit(t, 30*time.Second, false, "clone", proxy.URL+"/https://github.com/loopa/r", filepath.Join(t.TempDir(), "cycle"))
		if !strings.Contains(strings.ToLower(out), "redirect") {
			t.Fatalf("out=%s", out)
		}
		if n := len(snapshot()); n == 0 || n > 25 {
			t.Fatalf("upstream calls=%d out=%s", n, out)
		}
	})

	t.Run("cross_host", func(t *testing.T) {
		resetHits()
		crossProxy, logs := start(t, func(cfg *config) { cfg.AuthPassthrough = true })
		out := runGit(t, 30*time.Second, false, "-c", "http.extraHeader=Authorization: "+validAuth,
			"clone", crossProxy.URL+"/https://github.com/evil/r", filepath.Join(t.TempDir(), "evil"))
		got := snapshot()
		if len(got) != 1 || got[0].method != http.MethodGet || got[0].path != "/evil/r.git/info/refs" || got[0].auth != validAuth || strings.Contains(got[0].host, "evil.example") {
			t.Fatalf("hits=%+v out=%s", got, out)
		}
		blob := logs.String() + out
		omitPAT(t, blob)
		if strings.Contains(blob, "evil.example") || strings.Contains(blob, "offhost") {
			t.Fatalf("off-host redirect leaked out=%s logs=%s", out, logs.String())
		}
		if !strings.Contains(out, "502") || !strings.Contains(logs.String(), `"error_kind":"upstream_redirect"`) {
			t.Fatalf("out=%s logs=%s", out, logs.String())
		}
	})

	t.Run("private_helper", func(t *testing.T) {
		resetHits()
		proxy, logs := start(t, func(cfg *config) { cfg.AuthPassthrough = true })
		dst := filepath.Join(t.TempDir(), "priv")
		runGit(t, 30*time.Second, true, "-c", "credential.helper="+cred("TEST_PAT"), "clone", "--quiet", proxy.URL+"/https://github.com/oldorg/priv", dst)
		data, err := os.ReadFile(filepath.Join(dst, "file.txt"))
		if err != nil || string(data) != "redirect-v1" {
			t.Fatalf("content=%q err=%v", data, err)
		}
		var sawOld, sawChallenge, sawAuthed bool
		for _, hit := range snapshot() {
			switch {
			case hit.path == "/oldorg/priv.git/info/refs":
				sawOld = true
			case strings.HasPrefix(hit.path, "/neworg/priv.git/") && hit.auth == "":
				sawChallenge = true
			case hit.method == http.MethodPost && hit.path == "/neworg/priv.git/git-upload-pack" && hit.auth == validAuth:
				sawAuthed = true
			}
		}
		if !sawOld || !sawChallenge || !sawAuthed {
			t.Fatalf("hits=%+v", snapshot())
		}
		omitPAT(t, logs.String())
	})
}
