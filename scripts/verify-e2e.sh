#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<EOF
usage: $0 [--public | --loopback] [--skip-large] <RELAY_URL>

Run end-to-end checks against a live gitrelay endpoint.

Examples:
  $0 https://relay.example.com
  $0 http://127.0.0.1:8080

Environment overrides:
  E2E_REPO        Public GitHub repo for clone/fetch/push checks (default: staryxchen/gitrelay)
  E2E_LARGE_REPO  Public GitHub repo for shallow streaming check (default: golang/example)
  E2E_TIMEOUT     Per-request curl timeout in seconds (default: 15)
EOF
}

mode="auto"
skip_large=false

while (( $# > 0 )); do
  case "$1" in
    --public)
      mode="public"
      shift
      ;;
    --loopback)
      mode="loopback"
      shift
      ;;
    --skip-large)
      skip_large=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "error: unknown flag '$1'" >&2
      usage
      exit 1
      ;;
    *)
      break
      ;;
  esac
done

if (( $# != 1 )); then
  usage
  exit 1
fi

relay_url="${1%/}"
if [[ ! "$relay_url" =~ ^https?://[^[:space:]]+$ ]]; then
  echo "error: RELAY_URL must start with http:// or https://, got '${relay_url}'" >&2
  exit 1
fi

for cmd in git curl; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "error: required command '$cmd' is not installed" >&2
    exit 1
  fi
done

if [[ "$mode" == "auto" ]]; then
  if [[ "$relay_url" =~ ^http://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?$ ]]; then
    mode="loopback"
  else
    mode="public"
  fi
fi

repo="${E2E_REPO:-staryxchen/gitrelay}"
large_repo="${E2E_LARGE_REPO:-golang/example}"
curl_timeout="${E2E_TIMEOUT:-15}"

if [[ ! "$curl_timeout" =~ ^[0-9]+$ ]] || (( curl_timeout <= 0 )); then
  echo "error: E2E_TIMEOUT must be a positive integer, got '${curl_timeout}'" >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

pass_count=0
fail_count=0

pass() {
  pass_count=$((pass_count + 1))
  printf '[PASS] %s\n' "$1"
}

fail() {
  fail_count=$((fail_count + 1))
  printf '[FAIL] %s\n' "$1" >&2
}

http_status() {
  local extra_args=()
  while (( $# > 1 )); do
    extra_args+=("$1")
    shift
  done
  local url="$1"
  local code
  set +e
  code="$(curl -sS --max-time "$curl_timeout" -o /dev/null -w '%{http_code}' "${extra_args[@]}" "$url" 2>/dev/null)"
  set -e
  if [[ -z "$code" ]]; then
    code="000"
  fi
  printf '%s' "$code"
}

now_ms() {
  local ns
  ns="$(date +%s%3N 2>/dev/null || true)"
  if [[ "$ns" =~ ^[0-9]+$ ]]; then
    printf '%s' "$ns"
  else
    printf '%s000' "$(date +%s)"
  fi
}

echo "Target: ${relay_url} (mode=${mode}, repo=${repo})"

# 1. Clone without .git and fetch --prune
t0="$(now_ms)"
head1_full=""
head1=""
if GIT_TERMINAL_PROMPT=0 git clone --quiet "${relay_url}/https://github.com/${repo}" "${tmp_dir}/repo" 2>"${tmp_dir}/clone1.err"; then
  head1_full="$(git -C "${tmp_dir}/repo" rev-parse HEAD)"
  head1="$(git -C "${tmp_dir}/repo" rev-parse --short HEAD)"
  if GIT_TERMINAL_PROMPT=0 git -C "${tmp_dir}/repo" fetch --prune --quiet 2>"${tmp_dir}/fetch.err"; then
    dt=$(( $(now_ms) - t0 ))
    pass "clone (no .git) + fetch --prune: HEAD=${head1} (${dt}ms)"
  else
    fail "fetch --prune failed: $(tr '\n' ' ' < "${tmp_dir}/fetch.err")"
  fi
else
  fail "clone (no .git) failed: $(tr '\n' ' ' < "${tmp_dir}/clone1.err")"
fi

# 2. Clone with .git suffix
t0="$(now_ms)"
if GIT_TERMINAL_PROMPT=0 git clone --quiet "${relay_url}/https://github.com/${repo}.git" "${tmp_dir}/repo-dotgit" 2>"${tmp_dir}/clone2.err"; then
  head2_full="$(git -C "${tmp_dir}/repo-dotgit" rev-parse HEAD)"
  head2="$(git -C "${tmp_dir}/repo-dotgit" rev-parse --short HEAD)"
  dt=$(( $(now_ms) - t0 ))
  if [[ -z "$head1_full" || "$head1_full" == "$head2_full" ]]; then
    pass "clone (.git): HEAD=${head2} (${dt}ms)"
  elif git -C "${tmp_dir}/repo-dotgit" merge-base --is-ancestor "$head1_full" "$head2_full" 2>/dev/null; then
    pass "clone (.git): HEAD=${head2} (advanced from ${head1}, ${dt}ms)"
  else
    fail "clone (.git) HEAD unrelated to initial clone: ${head2} vs ${head1}"
  fi
else
  fail "clone (.git) failed: $(tr '\n' ' ' < "${tmp_dir}/clone2.err")"
fi

# 3. Shallow streaming clone of larger multi-branch repo
if [[ "$skip_large" == "false" ]]; then
  t0="$(now_ms)"
  if GIT_TERMINAL_PROMPT=0 git clone --depth=1 --quiet "${relay_url}/https://github.com/${large_repo}" "${tmp_dir}/large" 2>"${tmp_dir}/large.err"; then
    large_head="$(git -C "${tmp_dir}/large" rev-parse --short HEAD)"
    dt=$(( $(now_ms) - t0 ))
    pass "shallow clone (${large_repo}): HEAD=${large_head} (${dt}ms)"
  else
    fail "shallow clone (${large_repo}) failed: $(tr '\n' ' ' < "${tmp_dir}/large.err")"
  fi
fi

# 4. Internal endpoints (/healthz and /metrics)
if [[ "$mode" == "public" ]]; then
  hz_code="$(http_status "${relay_url}/healthz")"
  mt_code="$(http_status "${relay_url}/metrics")"
  if [[ "$hz_code" == "404" && "$mt_code" == "404" ]]; then
    pass "internal endpoints hidden on public URL (/healthz=404, /metrics=404)"
  else
    fail "expected /healthz=404 and /metrics=404 on public URL, got /healthz=${hz_code} /metrics=${mt_code}"
  fi
else
  hz_body="$(curl -fsS --max-time "$curl_timeout" "${relay_url}/healthz" 2>/dev/null || true)"
  mt_body="$(curl -fsS --max-time "$curl_timeout" "${relay_url}/metrics" 2>/dev/null || true)"
  if [[ "$hz_body" == "ok" ]] && grep -q '^gitrelay_requests_total ' <<<"$mt_body"; then
    pass "internal endpoints healthy on loopback (/healthz=ok, /metrics valid)"
  else
    fail "loopback /healthz or /metrics check failed"
  fi
fi

# 5. Reject HTTP CONNECT tunnel
connect_out="${tmp_dir}/connect.err"
set +e
connect_code="$(curl --http1.1 -sS --max-time "$curl_timeout" -o /dev/null -w '%{http_code}' -X CONNECT "${relay_url}/github.com:443" 2>"$connect_out")"
connect_rc=$?
set -e
if [[ "$connect_code" == "405" || "$connect_code" == "400" || "$connect_code" == "403" ]]; then
  pass "HTTP CONNECT rejected (HTTP ${connect_code})"
elif (( connect_rc != 0 )) && grep -qiE 'PROTOCOL_ERROR|405|400' "$connect_out"; then
  pass "HTTP CONNECT rejected at protocol layer (curl exit ${connect_rc})"
else
  fail "HTTP CONNECT unexpected result: code=${connect_code} rc=${connect_rc} err=$(tr '\n' ' ' < "$connect_out")"
fi

# 6. Reject non-Git archive download path
archive_code="$(http_status "${relay_url}/https://github.com/${repo}/archive/refs/heads/main.tar.gz")"
if [[ "$archive_code" == "400" ]]; then
  pass "non-Git archive path rejected (HTTP 400)"
else
  fail "expected HTTP 400 for archive path, got ${archive_code}"
fi

# 7. Read-only boundary (git push rejected)
if [[ -d "${tmp_dir}/repo/.git" ]]; then
  if GIT_TERMINAL_PROMPT=0 git -C "${tmp_dir}/repo" push origin HEAD:refs/heads/gitrelay-e2e-should-fail >"${tmp_dir}/push.out" 2>&1; then
    fail "git push unexpectedly succeeded"
  elif grep -qiE 'error: 400|error: 403|error: 405|unsupported GitHub path|not allowed' "${tmp_dir}/push.out"; then
    pass "read-only boundary enforced (git push rejected)"
  else
    fail "git push failed with unexpected reason: $(tr '\n' ' ' < "${tmp_dir}/push.out")"
  fi
else
  fail "skipped git push check because initial clone failed"
fi

total=$((pass_count + fail_count))
echo "Summary: ${pass_count}/${total} passed, ${fail_count} failed"
if (( fail_count > 0 )); then
  exit 1
fi
