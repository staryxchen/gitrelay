#!/usr/bin/env bash
set -euo pipefail

if (( $# != 4 )); then
  echo "usage: $0 <nightly|release> <tag> <sha> <dist_dir>" >&2
  exit 1
fi

mode="$1"
tag="$2"
sha="$3"
dist_dir="$4"

case "$mode" in
  nightly)
    if [[ "$tag" != "nightly" ]]; then
      echo "error: nightly mode requires tag 'nightly', got '${tag}'" >&2
      exit 1
    fi
    ;;
  release)
    if [[ ! "$tag" =~ ^v[0-9][0-9A-Za-z._+-]*$ ]]; then
      echo "error: release mode requires tag matching ^v[0-9], got '${tag}'" >&2
      exit 1
    fi
    ;;
  *)
    echo "error: mode must be 'nightly' or 'release', got '${mode}'" >&2
    exit 1
    ;;
esac

if [[ ! "$sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "error: sha must match ^[0-9a-f]{40}$, got '${sha}'" >&2
  exit 1
fi

sums_file="${dist_dir}/SHA256SUMS"
if [[ ! -d "$dist_dir" || ! -s "$sums_file" ]]; then
  echo "error: missing or empty ${sums_file}" >&2
  exit 1
fi

expected_binaries=()
while IFS= read -r line || [[ -n "$line" ]]; do
  if [[ ! "$line" =~ ^[0-9a-fA-F]{64}[[:space:]][[:space:]*]([^/[:space:]]+)$ ]]; then
    echo "error: malformed entry in SHA256SUMS: '${line}'" >&2
    exit 1
  fi
  asset_name="${BASH_REMATCH[1]}"
  if [[ "$asset_name" == "." || "$asset_name" == ".." || "$asset_name" == "SHA256SUMS" ]]; then
    echo "error: invalid asset name in SHA256SUMS: '${asset_name}'" >&2
    exit 1
  fi
  if [[ ! -f "${dist_dir}/${asset_name}" ]]; then
    echo "error: missing asset listed in SHA256SUMS: '${dist_dir}/${asset_name}'" >&2
    exit 1
  fi
  expected_binaries+=("$asset_name")
done < "$sums_file"

if (( ${#expected_binaries[@]} == 0 )); then
  echo "error: SHA256SUMS contains no assets" >&2
  exit 1
fi

(
  cd "$dist_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c SHA256SUMS >/dev/null
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 -c SHA256SUMS >/dev/null
  else
    echo "error: neither sha256sum nor shasum is installed" >&2
    exit 1
  fi
)

expected_names=("${expected_binaries[@]}" "SHA256SUMS")
asset_paths=()
for name in "${expected_names[@]}"; do
  asset_paths+=("${dist_dir}/${name}")
done

if [[ "$mode" == "release" ]]; then
  remote_refs="$(git ls-remote origin "refs/tags/${tag}" "refs/tags/${tag}^{}")"
  remote_sha="$(awk -v direct="refs/tags/${tag}" -v peeled="refs/tags/${tag}^{}" '
    $2 == peeled { p = $1 }
    $2 == direct { d = $1 }
    END { print (p != "" ? p : d) }
  ' <<<"$remote_refs")"
  if [[ -z "$remote_sha" || "$remote_sha" != "$sha" ]]; then
    echo "error: remote tag ${tag} resolves to '${remote_sha:-missing}', expected '${sha}'" >&2
    exit 1
  fi
fi

err_file="$(mktemp)"
notes_file=""
cleanup() {
  rm -f "$err_file"
  if [[ -n "$notes_file" ]]; then
    rm -f "$notes_file"
  fi
}
trap cleanup EXIT

release_exists=false
existing_assets=()
if view_out="$(gh release view "$tag" --json assets --jq '.assets[].name' 2>"$err_file")"; then
  release_exists=true
  if [[ -n "$view_out" ]]; then
    mapfile -t existing_assets <<<"$view_out"
  fi
else
  err_text="$(<"$err_file")"
  if grep -Eiq 'release not found' <<<"$err_text"; then
    release_exists=false
  else
    echo "$err_text" >&2
    exit 1
  fi
fi

is_expected_asset() {
  local candidate="$1"
  local item
  for item in "${expected_names[@]}"; do
    if [[ "$item" == "$candidate" ]]; then
      return 0
    fi
  done
  return 1
}

if [[ "$mode" == "nightly" ]]; then
  git push --force origin "${sha}:refs/tags/nightly"

  notes_file="$(mktemp)"
  built_at="$(TZ=UTC date '+%Y-%m-%dT%H:%M:%SZ')"
  {
    printf 'Automated nightly build from commit `%s` (%s).\n\n' "$sha" "$built_at"
    printf '### SHA256SUMS\n\n```text\n'
    cat "$sums_file"
    printf '```\n'
  } > "$notes_file"

  if [[ "$release_exists" == true ]]; then
    gh release upload nightly "${asset_paths[@]}" --clobber
    if (( ${#existing_assets[@]} > 0 )); then
      for existing in "${existing_assets[@]}"; do
        if ! is_expected_asset "$existing"; then
          gh release delete-asset nightly "$existing" --yes
        fi
      done
    fi
    gh release edit nightly --target "$sha" --prerelease --latest=false --title "Nightly (${sha:0:7})" --notes-file "$notes_file"
  else
    gh release create nightly "${asset_paths[@]}" --verify-tag --prerelease --latest=false --title "Nightly (${sha:0:7})" --notes-file "$notes_file"
  fi
else
  if [[ "$tag" == *-* ]]; then
    prerelease_flag="--prerelease"
    latest_flag="--latest=false"
  else
    prerelease_flag="--prerelease=false"
    latest_flag="--latest"
  fi

  if [[ "$release_exists" == true ]]; then
    gh release upload "$tag" "${asset_paths[@]}" --clobber
    gh release edit "$tag" "$prerelease_flag" "$latest_flag" --title "$tag"
  else
    gh release create "$tag" "${asset_paths[@]}" --verify-tag "$prerelease_flag" "$latest_flag" --title "$tag" --generate-notes
  fi
fi
