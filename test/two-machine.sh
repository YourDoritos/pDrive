#!/usr/bin/env bash
#
# Two-machine convergence test against a live Proton Drive account.
#
# Simulates two devices on one box by giving each its own XDG tree and sync
# folder, so each keeps an independent state database and event cursor while
# talking to the same account. That is what makes it a real test of the sync
# protocol rather than of one process talking to itself.
#
# Two things it deliberately does NOT do:
#
#   * It does not copy session.enc. Each real device has its own Proton
#     session; two copies of one session file would race on refresh-token
#     rotation and log each other out — the failure that cost us a session in
#     Phase 1. Both machines symlink the one real session file instead, so
#     auth is shared and rotations are always current.
#
#   * It does not run the daemon. Every step is an explicit `pdrive sync`, so
#     the order of events is known and a failure points at one operation.
#     A running daemon would also pull the test files into the real folder.
#
# Everything it creates lives under a single prefix and is removed at the end,
# and the account is checked before and after.
#
# usage: test/two-machine.sh [--keep]

set -uo pipefail

PREFIX="pdrive-2m-test"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

PDRIVE="${PDRIVE:-$HOME/.local/bin/pdrive}"
REAL_SESSION="$HOME/.local/state/pdrive/session.enc"
LAB="$(mktemp -d /tmp/pdrive-2m.XXXXXX)"

PASS=0
FAIL=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
note() { printf '  ---- %s\n' "$*"; }

# --- machine plumbing -------------------------------------------------------

setup_machine() {
    local name=$1
    local base="$LAB/$name"
    mkdir -p "$base"/{config/pdrive,state/pdrive,data,cache,files}
    # Shared auth, independent sync state.
    ln -sf "$REAL_SESSION" "$base/state/pdrive/session.enc"
    cat > "$base/config/pdrive/config.toml" <<EOF
[sync]
root = "$base/files"
EOF
}

# run <machine> <args...> — invoke pdrive as that machine
run() {
    local name=$1; shift
    local base="$LAB/$name"
    XDG_CONFIG_HOME="$base/config" \
    XDG_STATE_HOME="$base/state" \
    XDG_DATA_HOME="$base/data" \
    XDG_CACHE_HOME="$base/cache" \
    "$PDRIVE" "$@" 2>&1
}

sync() { run "$1" sync --quiet; }

files() { echo "$LAB/$1/files"; }

# --- assertions -------------------------------------------------------------

expect_content() {
    local machine=$1 rel=$2 want=$3 what=$4
    local path; path="$(files "$machine")/$rel"
    if [ ! -e "$path" ]; then bad "$what (missing on $machine: $rel)"; return; fi
    local got; got="$(cat "$path")"
    if [ "$got" = "$want" ]; then ok "$what"
    else bad "$what (on $machine, $rel = '$got', want '$want')"; fi
}

expect_absent() {
    local machine=$1 rel=$2 what=$3
    if [ -e "$(files "$machine")/$rel" ]; then bad "$what (still present on $machine: $rel)"
    else ok "$what"; fi
}

expect_glob() {
    local machine=$1 pattern=$2 what=$3
    # shellcheck disable=SC2086
    if compgen -G "$(files "$machine")/$pattern" > /dev/null; then ok "$what"
    else bad "$what (nothing matching '$pattern' on $machine)"; fi
}

# --- lifecycle --------------------------------------------------------------

DAEMON_WAS_ACTIVE=0
cleanup() {
    say "Cleanup"

    if [ "$KEEP" = "1" ]; then
        note "--keep given; leaving $LAB and the remote test files in place"
    else
        # Remove the test tree on A and push the deletions, so the account is
        # left exactly as it was found.
        rm -rf "$(files A)/$PREFIX" 2>/dev/null
        run A sync --quiet >/dev/null 2>&1
        note "removed remote test files (they are in Proton's trash)"
        rm -rf "$LAB"
    fi

    if [ "$DAEMON_WAS_ACTIVE" = "1" ]; then
        systemctl --user start pdrived 2>/dev/null && note "restarted pdrived"
    fi

    printf '\n\033[1m%d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
    [ "$FAIL" -gt 0 ] && exit 1
    exit 0
}
trap cleanup EXIT INT TERM

# --- start ------------------------------------------------------------------

say "Setup"

if [ ! -x "$PDRIVE" ]; then
    echo "pdrive not found at $PDRIVE (set PDRIVE=...)"; exit 1
fi
if [ ! -f "$REAL_SESSION" ]; then
    echo "not logged in — run \`pdrive\` first"; exit 1
fi

if systemctl --user is-active pdrived >/dev/null 2>&1; then
    DAEMON_WAS_ACTIVE=1
    systemctl --user stop pdrived
    note "stopped pdrived for the duration of the test"
fi

setup_machine A
setup_machine B
note "machine A: $(files A)"
note "machine B: $(files B)"

note "seeding both machines from the account"
sync A >/dev/null
sync B >/dev/null

BEFORE_A="$(ls -1 "$(files A)" | grep -v "^$PREFIX" | sort)"
note "account holds $(echo "$BEFORE_A" | grep -c .) pre-existing item(s)"

D="$PREFIX"
mkdir -p "$(files A)/$D"

# --- 1. create on A, appears on B ------------------------------------------

say "1. A creates a file, B receives it"
echo "written on A" > "$(files A)/$D/one.txt"
sync A >/dev/null
sync B >/dev/null
expect_content B "$D/one.txt" "written on A" "A -> B: new file"

# --- 2. edit on B, appears on A --------------------------------------------

say "2. B edits it, A receives the edit"
echo "edited on B" > "$(files B)/$D/one.txt"
sync B >/dev/null
sync A >/dev/null
expect_content A "$D/one.txt" "edited on B" "B -> A: edit"

# --- 3. nested folders ------------------------------------------------------

say "3. A creates nested folders, B mirrors the structure"
mkdir -p "$(files A)/$D/deep/deeper"
echo "buried" > "$(files A)/$D/deep/deeper/nested.txt"
sync A >/dev/null
sync B >/dev/null
expect_content B "$D/deep/deeper/nested.txt" "buried" "A -> B: nested folders"

# --- 4. rename is a move, not a re-upload -----------------------------------

say "4. A renames, B follows"
mv "$(files A)/$D/one.txt" "$(files A)/$D/renamed.txt"
MOVE_OUT="$(run A sync)"
sync B >/dev/null
expect_content B "$D/renamed.txt" "edited on B" "A -> B: rename"
expect_absent B "$D/one.txt" "A -> B: old name gone"
if echo "$MOVE_OUT" | grep -q '1 moved'; then
    ok "rename used a remote move, not a re-upload"
else
    bad "rename was not a move: $(echo "$MOVE_OUT" | grep 'up:' || echo "$MOVE_OUT" | head -2)"
fi

# --- 5. delete on B, gone on A ---------------------------------------------

say "5. B deletes, A removes its copy"
rm "$(files B)/$D/renamed.txt"
sync B >/dev/null
sync A >/dev/null
expect_absent A "$D/renamed.txt" "B -> A: deletion"

# --- 6. concurrent edit: both versions survive ------------------------------

say "6. Both machines edit the same file: nothing is lost"
echo "shared original" > "$(files A)/$D/shared.txt"
sync A >/dev/null
sync B >/dev/null

# Diverge without either machine seeing the other.
echo "A's version" > "$(files A)/$D/shared.txt"
echo "B's version" > "$(files B)/$D/shared.txt"
sync A >/dev/null           # A wins the race to upload
CONFLICT_OUT="$(run B sync)" # B must preserve its own copy

if echo "$CONFLICT_OUT" | grep -qi conflict; then
    ok "B reported a conflict"
else
    bad "B did not report a conflict: $(echo "$CONFLICT_OUT" | tail -3)"
fi
expect_content B "$D/shared.txt" "A's version" "conflict: remote version takes the canonical path"
expect_glob B "$D/shared*conflict*" "conflict: B's own version preserved beside it"

# The preserved copy must reach the account, so neither version is stranded.
sync B >/dev/null
sync A >/dev/null
expect_glob A "$D/shared*conflict*" "conflict: preserved copy propagated to A"
expect_content A "$D/shared.txt" "A's version" "conflict: A's canonical file intact"

# --- 7. convergence ---------------------------------------------------------

say "7. Both machines converge"
sync A >/dev/null; sync B >/dev/null
sync A >/dev/null; sync B >/dev/null

LIST_A="$(cd "$(files A)" && find . -type f | sort)"
LIST_B="$(cd "$(files B)" && find . -type f | sort)"
if [ "$LIST_A" = "$LIST_B" ]; then
    ok "A and B hold identical file trees ($(echo "$LIST_A" | grep -c .) files)"
else
    bad "the machines did not converge"
    diff <(echo "$LIST_A") <(echo "$LIST_B") | head -20
fi

# --- 8. the account's own files are untouched -------------------------------

say "8. Pre-existing files were not disturbed"
AFTER_A="$(ls -1 "$(files A)" | grep -v "^$PREFIX" | sort)"
if [ "$BEFORE_A" = "$AFTER_A" ]; then
    ok "the account's own files are unchanged"
else
    bad "pre-existing files changed"
    diff <(echo "$BEFORE_A") <(echo "$AFTER_A")
fi
