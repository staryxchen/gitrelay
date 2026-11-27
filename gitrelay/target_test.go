package main

import (
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
