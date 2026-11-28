package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTargetBoundaries(t *testing.T) {
	tests := []struct {
		method, path string
		ok           bool
		action       gitAction
		owner, repo  string
		wantURL      string
	}{
		{
			"GET", "/https://github.com/o/r.git/info/refs?service=git-upload-pack",
			true, actionInfoRefs, "o", "r",
			"https://github.com/o/r.git/info/refs?service=git-upload-pack",
		},
		{
			"GET", "/https://github.com/o/r/info/refs?service=git-upload-pack",
			true, actionInfoRefs, "o", "r",
			"https://github.com/o/r.git/info/refs?service=git-upload-pack",
		},
		{
			"GET", "/https:/github.com/o/my.repo/info/refs?service=git-upload-pack",
			true, actionInfoRefs, "o", "my.repo",
			"https://github.com/o/my.repo.git/info/refs?service=git-upload-pack",
		},
		{
			"POST", "/https://github.com/o/r.git/git-upload-pack",
			true, actionUploadPack, "o", "r",
			"https://github.com/o/r.git/git-upload-pack",
		},
		{
			"POST", "/https://github.com/o/r/git-upload-pack",
			true, actionUploadPack, "o", "r",
			"https://github.com/o/r.git/git-upload-pack",
		},
		{"GET", "/https://github.com/o/.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/%2e.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/%2e%2e.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/r.git.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https:///github.com/o/r/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https:/evil.test/o/r/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/r/releases/download/v1/a.zip", false, 0, "", "", ""},
		{"HEAD", "/https://github.com/o/r/archive/refs/heads/main.tar.gz", false, 0, "", "", ""},
		{"GET", "/https://raw.githubusercontent.com/o/r/main/a.txt", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/r.git/info/refs?service=git-receive-pack", false, 0, "", "", ""},
		{"POST", "/https://github.com/o/r.git/git-receive-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/r", false, 0, "", "", ""},
		{"GET", "/https://api.github.com/repos/o/r", false, 0, "", "", ""},
		{"GET", "/https://github.com.evil.test/o/r.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com:443/o/r.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://u:p@github.com/o/r.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/http://github.com/o/r.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://127.0.0.1/o/r.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/r.git/info/%2e%2e/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://github.com/o/r%5c.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"GET", "/https://codeload.github.com/o/r/zip/main", false, 0, "", "", ""},
		{"POST", "/https://github.com/o/r.git/git-upload-pack?extra=1", false, 0, "", "", ""},
		{"HEAD", "/https://github.com/o/r.git/info/refs?service=git-upload-pack", false, 0, "", "", ""},
		{"DELETE", "/https://github.com/o/r.git/git-upload-pack", false, 0, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			got, err := parseTarget(r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
			if (err == nil) != tt.ok {
				t.Fatalf("target=%+v err=%v", got, err)
			}
			if tt.ok && (got.Action != tt.action || got.Owner != tt.owner || got.Repo != tt.repo || got.UpstreamURL.String() != tt.wantURL) {
				t.Fatalf("got=%+v url=%q, want owner=%q repo=%q action=%d url=%q",
					got, got.UpstreamURL.String(), tt.owner, tt.repo, tt.action, tt.wantURL)
			}
		})
	}
}

func mustTarget(t *testing.T, method, path string) target {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	got, err := parseTarget(r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	if err != nil {
		t.Fatalf("parse %s %s: %v", method, path, err)
	}
	return got
}

func TestParseRedirectTargetBoundaries(t *testing.T) {
	const (
		info     = "/https://github.com/o/r.git/info/refs?service=git-upload-pack"
		infoBare = "/https://github.com/o/r/info/refs?service=git-upload-pack"
		upload   = "/https://github.com/o/r.git/git-upload-pack"
		want     = "https://github.com/new/repo.git/info/refs?service=git-upload-pack"
		bareLoc  = "https://github.com/new/repo/info/refs?service=git-upload-pack"
		relLoc   = "/new/repo/info/refs?service=git-upload-pack"
	)
	current := mustTarget(t, http.MethodGet, info)
	bare := mustTarget(t, http.MethodGet, infoBare)
	post := mustTarget(t, http.MethodPost, upload)
	for _, tt := range []struct {
		name string
		code int
		loc  string
	}{
		{"301_absolute_git", 301, want},
		{"301_absolute_bare", 301, bareLoc},
		{"301_relative", 301, relLoc},
		{"302_absolute_git", 302, want},
		{"307_absolute_bare", 307, bareLoc},
		{"308_relative", 308, relLoc},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRedirectTarget(current, tt.code, []string{tt.loc})
			if err != nil {
				t.Fatal(err)
			}
			if got.Action != actionInfoRefs || got.Owner != "new" || got.Repo != "repo" ||
				got.UpstreamURL.String() != want || got.RelayPath() != "/"+want {
				t.Fatalf("loc=%q got owner=%s repo=%s url=%s path=%s", tt.loc, got.Owner, got.Repo, got.UpstreamURL, got.RelayPath())
			}
		})
	}

	rejects := []struct {
		name string
		cur  target
		code int
		locs []string
		loop bool
	}{
		{"status_300", current, 300, []string{want}, false},
		{"status_303", current, 303, []string{want}, false},
		{"status_304", current, 304, []string{want}, false},
		{"post_301", post, 301, []string{want}, false},
		{"post_307", post, 307, []string{want}, false},
		{"missing", current, 301, nil, false},
		{"empty", current, 302, []string{""}, false},
		{"whitespace_leading", current, 301, []string{" " + want}, false},
		{"whitespace_trailing", current, 301, []string{want + " "}, false},
		{"duplicate", current, 302, []string{want, "https://github.com/other/repo.git/info/refs?service=git-upload-pack"}, false},
		{"scheme_relative", current, 302, []string{"//evil.test/new/repo.git/info/refs?service=git-upload-pack"}, false},
		{"http", current, 301, []string{"http://github.com/new/repo.git/info/refs?service=git-upload-pack"}, false},
		{"dotdot_relative", current, 302, []string{"/../new/repo.git/info/refs?service=git-upload-pack"}, false},
		{"loop_bare_to_git", bare, 301, []string{"https://github.com/o/r.git/info/refs?service=git-upload-pack"}, true},
		{"loop_git_to_git", current, 301, []string{"https://github.com/o/r.git/info/refs?service=git-upload-pack"}, true},
		{"cross_host", current, 301, []string{"https://evil.test/new/repo.git/info/refs?service=git-upload-pack"}, false},
		{"extra_query", current, 301, []string{"https://github.com/new/repo.git/info/refs?service=git-upload-pack&sig=SECRET"}, false},
		{"receive_pack_query", current, 302, []string{"https://github.com/new/repo.git/info/refs?service=git-receive-pack"}, false},
		{"upload_pack", current, 307, []string{"https://github.com/new/repo.git/git-upload-pack"}, false},
		{"cr_or_lf_control", current, 301, []string{want + "\r"}, false},
		{"empty_fragment_absolute", current, 301, []string{want + "#"}, false},
		{"empty_fragment_relative", current, 301, []string{relLoc + "#"}, false},
	}
	for _, tt := range rejects {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRedirectTarget(tt.cur, tt.code, tt.locs)
			if tt.loop {
				if !errors.Is(err, errRedirectLoop) {
					t.Fatalf("got=%+v err=%v", got, err)
				}
				return
			}
			if err == nil || errors.Is(err, errRedirectLoop) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}
