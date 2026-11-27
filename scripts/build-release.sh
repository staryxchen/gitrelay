#!/usr/bin/env bash
set -euo pipefail

TARGETS=("linux/amd64" "linux/arm64")
LDFLAGS="-s -w"

smoke=false
if [[ "${1:-}" == "--smoke" ]]; then
  smoke=true
  shift
fi

if (( $# > 1 )); then
  echo "usage: $0 [--smoke] [OUT_DIR]" >&2
  exit 1
fi

if (( $# == 1 )); then
  out_dir="$1"
else
  out_dir="dist"
fi

if [[ -z "$out_dir" || "$out_dir" == -* ]]; then
  echo "usage: $0 [--smoke] [OUT_DIR]" >&2
  exit 1
fi

caller_pwd="$(pwd -P)"
case "$out_dir" in
  /*) out_dir_target="$out_dir" ;;
  *) out_dir_target="${caller_pwd}/${out_dir}" ;;
esac

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
stage_dir="$(mktemp -d)"
server_pid=""

cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$stage_dir"
}
trap cleanup EXIT

sha256_compute() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    echo "error: neither sha256sum nor shasum is installed" >&2
    exit 1
  fi
}

sha256_check() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c "$@"
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c "$@"
  else
    echo "error: neither sha256sum nor shasum is installed" >&2
    exit 1
  fi
}

pick_free_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_STREAM); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
    return
  fi
  local p
  for p in $(seq 28080 28180); do
    if ! (echo >/dev/tcp/127.0.0.1/"$p") >/dev/null 2>&1; then
      echo "$p"
      return
    fi
  done
  echo "error: unable to find a free loopback port" >&2
  return 1
}

cd "$repo_root"

built_names=()
for target in "${TARGETS[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  bin_name="gitrelay-${os}-${arch}"
  bin_path="${stage_dir}/${bin_name}"

  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="$LDFLAGS" -o "$bin_path" ./gitrelay

  if command -v readelf >/dev/null 2>&1; then
    case "$arch" in
      amd64) expected_machine="X86-64" ;;
      arm64) expected_machine="AArch64" ;;
      *)
        echo "error: unsupported architecture '${arch}'" >&2
        exit 1
        ;;
    esac

    elf_header="$(LC_ALL=C readelf -h "$bin_path")"
    if ! grep -Eq "Machine:.*${expected_machine}" <<<"$elf_header"; then
      echo "error: ${bin_name} ELF Machine does not match ${expected_machine}" >&2
      exit 1
    fi

    if LC_ALL=C readelf -l "$bin_path" | grep -Eq '^[[:space:]]*INTERP([[:space:]]|$)'; then
      echo "error: ${bin_name} contains an ELF INTERP header and is not statically linked" >&2
      exit 1
    fi
  fi

  built_names+=("$bin_name")
done

mapfile -t sorted_names < <(printf '%s\n' "${built_names[@]}" | LC_ALL=C sort)

(
  cd "$stage_dir"
  sha256_compute "${sorted_names[@]}" > SHA256SUMS
  sha256_check SHA256SUMS >/dev/null
)

if [[ "$smoke" == true && "$(uname -s)" == "Linux" && "$(uname -m)" == "x86_64" ]]; then
  port="$(pick_free_port)"
  listen_addr="127.0.0.1:${port}"
  smoke_bin="${stage_dir}/gitrelay-linux-amd64"

  LISTEN_ADDR="$listen_addr" "$smoke_bin" >/dev/null 2>&1 &
  server_pid="$!"

  healthy=false
  for _ in $(seq 1 30); do
    if LISTEN_ADDR="$listen_addr" "$smoke_bin" healthcheck >/dev/null 2>&1; then
      healthy=true
      break
    fi
    if ! kill -0 "$server_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done

  if [[ "$healthy" != true ]]; then
    echo "error: smoke test healthcheck did not succeed on ${listen_addr}" >&2
    exit 1
  fi

  kill -TERM "$server_pid"
  wait "$server_pid"
  server_pid=""
fi

mkdir -p "$out_dir_target"
out_dir_abs="$(cd "$out_dir_target" && pwd -P)"

for name in "${sorted_names[@]}"; do
  tmp_dest="${out_dir_abs}/.${name}.tmp.$$"
  install -m 0755 "${stage_dir}/${name}" "$tmp_dest"
  mv -f "$tmp_dest" "${out_dir_abs}/${name}"
done

tmp_sums="${out_dir_abs}/.SHA256SUMS.tmp.$$"
install -m 0644 "${stage_dir}/SHA256SUMS" "$tmp_sums"
mv -f "$tmp_sums" "${out_dir_abs}/SHA256SUMS"

(
  cd "$out_dir_abs"
  sha256_check SHA256SUMS
)
