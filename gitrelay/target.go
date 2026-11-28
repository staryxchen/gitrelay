package main

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const allowedUpstreamHost = "github.com"

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

	errRedirectLoop = errors.New("upstream redirect loop")
)

type gitAction uint8

const (
	actionInfoRefs gitAction = iota + 1
	actionUploadPack
)

type target struct {
	Owner       string
	Repo        string
	Action      gitAction
	UpstreamURL *url.URL
}

func (t target) RelayPath() string {
	return "/" + t.UpstreamURL.String()
}

func parseTarget(method, escapedPath, rawQuery string) (target, error) {
	raw := strings.TrimPrefix(escapedPath, "/")
	if strings.HasPrefix(raw, "https:/") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + strings.TrimPrefix(raw, "https:/")
	}
	if rawQuery != "" {
		raw += "?" + rawQuery
	}
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, errors.New("invalid upstream URL")
	}
	return parseUpstreamURL(method, u)
}

func parseUpstreamURL(method string, u *url.URL) (target, error) {
	if u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Fragment != "" || u.Host != allowedUpstreamHost ||
		strings.ContainsAny(u.Path, "\\\x00\r\n") {
		return target{}, errors.New("invalid upstream URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 3 || !ownerPattern.MatchString(parts[0]) || !repoPattern.MatchString(parts[1]) {
		return target{}, errors.New("invalid owner or repository")
	}
	for _, part := range parts {
		if part == "" {
			return target{}, errors.New("empty upstream path component")
		}
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if repo == "" || repo == "." || repo == ".." || strings.HasSuffix(repo, ".git") {
		return target{}, errors.New("invalid owner or repository")
	}
	owner := parts[0]
	canonicalRepo := "/" + owner + "/" + repo + ".git"
	if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" &&
		method == http.MethodGet && u.RawQuery == "service=git-upload-pack" {
		return target{
			Owner:  owner,
			Repo:   repo,
			Action: actionInfoRefs,
			UpstreamURL: &url.URL{
				Scheme:   "https",
				Host:     allowedUpstreamHost,
				Path:     canonicalRepo + "/info/refs",
				RawQuery: "service=git-upload-pack",
			},
		}, nil
	}
	if len(parts) == 3 && parts[2] == "git-upload-pack" &&
		method == http.MethodPost && u.RawQuery == "" {
		return target{
			Owner:  owner,
			Repo:   repo,
			Action: actionUploadPack,
			UpstreamURL: &url.URL{
				Scheme: "https",
				Host:   allowedUpstreamHost,
				Path:   canonicalRepo + "/git-upload-pack",
			},
		}, nil
	}
	return target{}, errors.New("unsupported GitHub path or HTTP method")
}

func parseRedirectTarget(current target, statusCode int, locations []string) (target, error) {
	switch statusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return target{}, errors.New("unsupported upstream redirect")
	}
	if current.Action != actionInfoRefs || len(locations) != 1 {
		return target{}, errors.New("unsupported upstream redirect")
	}
	raw := locations[0]
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "#\\\x00\r\n") {
		return target{}, errors.New("unsupported upstream redirect")
	}
	absolute := raw
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		absolute = "https://" + allowedUpstreamHost + raw
	} else if !strings.HasPrefix(raw, "https://") {
		return target{}, errors.New("unsupported upstream redirect")
	}
	u, err := url.Parse(absolute)
	if err != nil {
		return target{}, errors.New("unsupported upstream redirect")
	}
	next, err := parseUpstreamURL(http.MethodGet, u)
	if err != nil {
		return target{}, err
	}
	if next.UpstreamURL.String() == current.UpstreamURL.String() {
		return target{}, errRedirectLoop
	}
	return next, nil
}
