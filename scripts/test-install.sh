#!/bin/sh
# Local tests for scripts/install.sh. Mocks uname/curl inside a temp dir;
# never touches the network or the real $HOME.
set -eu

script="$(cd "$(dirname "$0")" && pwd)/install.sh"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
pass=0
failn=0

ok() { pass=$((pass + 1)); echo "ok   - $1"; }
bad() { failn=$((failn + 1)); echo "FAIL - $1"; }

for tool in sha256sum tar; do
  command -v "$tool" > /dev/null 2>&1 || { echo "skip: $tool missing" >&2; exit 1; }
done

# Release fixtures: $root/release/<tag>/qdrive-linux-<arch>.tar.gz(.sha256)
make_release() { # tag arch content
  d="$root/release/$1"; mkdir -p "$d/pkg"
  printf '#!/bin/sh\necho %s\n' "$3" > "$d/pkg/qdrive"
  tar -czf "$d/qdrive-linux-$2.tar.gz" -C "$d/pkg" qdrive
  rm -rf "$d/pkg"
  (cd "$d" && sha256sum "qdrive-linux-$2.tar.gz" > "qdrive-linux-$2.tar.gz.sha256")
}
make_release latest amd64 latest-amd64
make_release latest arm64 latest-arm64
make_release v1.2.3 amd64 v123-amd64
make_release corrupt amd64 good
echo tampered >> "$root/release/corrupt/qdrive-linux-amd64.tar.gz"
make_release wrong-entry amd64 good
printf '%064d  other-file\n' 0 > "$root/release/wrong-entry/qdrive-linux-amd64.tar.gz.sha256"

mock="$root/mock"; mkdir -p "$mock"
cat > "$mock/uname" <<'M'
#!/bin/sh
case "$1" in -s) echo "${MOCK_OS:-Linux}" ;; -m) echo "$MOCK_ARCH" ;; *) echo Linux ;; esac
M
cat > "$mock/curl" <<'M'
#!/bin/sh
url=""; out=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift ;; -*) ;; *) url="$1" ;; esac
  shift
done
echo "$url" >> "$MOCK_LOG"
case "$url" in
  https://github.com/zzfn/QDrive/releases/latest/download/*) tag=latest ;;
  https://github.com/zzfn/QDrive/releases/download/*) tag="${url#https://github.com/zzfn/QDrive/releases/download/}"; tag="${tag%%/*}" ;;
  *) exit 22 ;;
esac
[ -n "${MOCK_TAG_OVERRIDE:-}" ] && tag="$MOCK_TAG_OVERRIDE"
src="$MOCK_RELEASE/$tag/${url##*/}"
[ -f "$src" ] || exit 22
cp "$src" "$out"
M
printf '#!/bin/sh\necho "$*" >> "$MOCK_LOG.sudo"\nexit 1\n' > "$mock/sudo"
chmod +x "$mock/uname" "$mock/curl" "$mock/sudo"

n=0
run() { # arch [env...] ; sets out, rc, home, dest
  n=$((n + 1)); case_dir="$root/case$n"; home="$case_dir/home"; dest="$case_dir/bin"
  mkdir -p "$home"; log="$case_dir/curl.log"; : > "$log"
  arch="$1"; shift
  set +e
  out="$(env -i PATH="$mock:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$home" QDRIVE_INSTALL_DIR="$dest" \
    MOCK_ARCH="$arch" MOCK_LOG="$log" MOCK_RELEASE="$root/release" "$@" /bin/sh "$script" 2>&1)"
  rc=$?
  set -e
}

# 1-2. both architectures, each with alias names
for pair in x86_64:amd64 amd64:amd64 aarch64:arm64 arm64:arm64; do
  m="${pair%%:*}"; a="${pair#*:}"
  run "$m"
  if [ $rc -eq 0 ] && [ "$("$dest/qdrive")" = "latest-$a" ] &&
     grep -qx "https://github.com/zzfn/QDrive/releases/latest/download/qdrive-linux-$a.tar.gz.sha256" "$log"; then
    ok "installs $a for uname -m=$m"
  else bad "installs $a for uname -m=$m: $out"; fi
done

# 3. specific version
run x86_64 QDRIVE_VERSION=v1.2.3
if [ $rc -eq 0 ] && [ "$("$dest/qdrive")" = "v123-amd64" ] &&
   grep -qx "https://github.com/zzfn/QDrive/releases/download/v1.2.3/qdrive-linux-amd64.tar.gz" "$log"; then
  ok "QDRIVE_VERSION selects tag"
else bad "QDRIVE_VERSION: $out"; fi

# 4. checksum failure keeps existing binary
n1=$((n + 1)); mkdir -p "$root/case$n1/bin"; echo original > "$root/case$n1/bin/qdrive"
run x86_64 MOCK_TAG_OVERRIDE=corrupt
if [ $rc -ne 0 ] && [ "$(cat "$dest/qdrive")" = original ] &&
   [ "$(ls -A "$dest")" = qdrive ] && echo "$out" | grep -q checksum; then
  ok "checksum failure leaves existing file intact"
else bad "checksum failure: rc=$rc $out"; fi

# A checksum for a different filename must not authorize the downloaded archive.
n1=$((n + 1)); mkdir -p "$root/case$n1/bin"; echo original > "$root/case$n1/bin/qdrive"
run x86_64 MOCK_TAG_OVERRIDE=wrong-entry
if [ $rc -ne 0 ] && [ "$(cat "$dest/qdrive")" = original ] && echo "$out" | grep -q checksum; then
  ok "rejects checksum for another file"
else bad "wrong checksum entry: rc=$rc $out"; fi

# An existing directory at the target path must not be treated as an install.
n1=$((n + 1)); mkdir -p "$root/case$n1/bin/qdrive"
run x86_64
if [ $rc -ne 0 ] && [ -d "$dest/qdrive" ] && [ -z "$(ls -A "$dest/qdrive")" ]; then
  ok "rejects directory at target"
else bad "directory at target: rc=$rc $out"; fi

# 5. unsupported architecture / OS
for m in i686 armv7l riscv64; do
  run "$m"
  if [ $rc -ne 0 ] && [ ! -e "$dest" ] && [ ! -s "$log" ] && echo "$out" | grep -q "unsupported architecture"; then
    ok "rejects $m"
  else bad "rejects $m: $out"; fi
done
run x86_64 MOCK_OS=Darwin
if [ $rc -ne 0 ] && [ ! -e "$dest" ] && echo "$out" | grep -q "unsupported system"; then ok "rejects non-Linux"
else bad "rejects non-Linux: $out"; fi

# 6. unpublished release fails clearly
run x86_64 QDRIVE_VERSION=v9.9.9
if [ $rc -ne 0 ] && [ ! -e "$dest" ] && echo "$out" | grep -q "not be published"; then ok "unpublished release fails"
else bad "unpublished release: $out"; fi

# 7. no writes into HOME when QDRIVE_INSTALL_DIR is set; no profile edits; PATH hint
run x86_64
if [ $rc -eq 0 ] && [ -z "$(ls -A "$home")" ] && echo "$out" | grep -q "not in your PATH"; then
  ok "does not write to HOME, hints missing PATH"
else bad "HOME untouched: $(ls -A "$home") $out"; fi
run x86_64 PATH="$mock:/usr/bin:/bin:/usr/sbin:/sbin:$root/case$((n + 1))/bin"
if [ $rc -eq 0 ] && ! echo "$out" | grep -q "not in your PATH"; then ok "no PATH hint when already on PATH"
else bad "PATH hint suppression: $out"; fi

# 8. default dir is $HOME/.local/bin (fake HOME)
n=$((n + 1)); home="$root/case$n/home"; mkdir -p "$home"; : > "$root/case$n/log"
set +e
out="$(env -i PATH="$mock:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$home" MOCK_ARCH=x86_64 MOCK_LOG="$root/case$n/log" \
  MOCK_RELEASE="$root/release" /bin/sh "$script" 2>&1)"; rc=$?
set -e
if [ $rc -eq 0 ] && [ -x "$home/.local/bin/qdrive" ] && [ "$(ls -A "$home")" = .local ]; then ok "default install dir"
else bad "default install dir: $out"; fi

# A truncated pipe must not execute a partially downloaded installer.
n=$((n + 1)); case_dir="$root/case$n"; mkdir -p "$case_dir/bin"
if head -n 35 "$script" | env -i PATH="$mock:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$case_dir" MOCK_LOG="$case_dir/log" MOCK_ARCH=x86_64 MOCK_RELEASE="$root/release" \
  QDRIVE_INSTALL_DIR="$case_dir/bin" /bin/sh > "$case_dir/out" 2>&1; then
  bad "truncated script unexpectedly succeeded"
elif [ -e "$case_dir/log" ] || [ -e "$case_dir/bin/qdrive" ]; then
  bad "truncated script executed installation steps"
else
  ok "truncated script does not execute"
fi

# 9. no sudo usage; POSIX sh syntax
if ! ls "$root"/case*/*.sudo > /dev/null 2>&1; then ok "never invokes sudo"; else bad "invoked sudo"; fi
if /bin/sh -n "$script"; then ok "/bin/sh -n"; else bad "/bin/sh -n"; fi
for s in dash posh; do
  if command -v "$s" > /dev/null 2>&1; then
    if "$s" -n "$script"; then ok "$s -n"; else bad "$s -n"; fi
  fi
done
if command -v shellcheck > /dev/null 2>&1; then
  if shellcheck -s sh "$script"; then ok "shellcheck -s sh"; else bad "shellcheck"; fi
fi

echo "$pass passed, $failn failed"
[ "$failn" -eq 0 ]
