#!/bin/sh
# Privacy guard (see docs/security.md): fails when repository content contains
# any of the maintainer's private identifiers (tailnet name, tailnet IPs, device
# and folder IDs, host names, user names, personal paths). The identifiers come
# from a denylist that is never committed, and this script never prints them.
#
# Usage: scripts/privacy-check.sh [--staged | --untracked | --history | --identity] [--require]
#   (default)    tracked files in the working tree
#   --staged     the staged content (the index); used by .githooks/pre-commit
#   --untracked  tracked files plus untracked files that are not ignored
#   --history    every commit reachable from any ref: diffs (merges and binary
#                files included) and messages
#   --identity   the author and committer identity the next commit would use
#   --require    exit 2 when no denylist is available (default: skip with a notice)
#
# Commit author and committer names and emails are not part of --history: they
# are the public identity of the account that pushes, chosen by the maintainer
# (spec D3, e.g. a GitHub noreply address), not repository content. --identity
# checks them on demand; .githooks/pre-commit runs it and only warns.
#
# Denylist: newline-separated literal strings, matched case-insensitively, taken
# from $PRIVACY_DENYLIST (the CI secret) or else from .git/info/privacy-denylist
# (local and untracked). Blank lines and lines starting with '#' are ignored.
#
# Output names the file and line numbers (or the commit and file) of each match,
# never the matching text. A path that itself contains denylisted text is withheld.
# Exit status: 0 clean or skipped, 1 denylisted text found, 2 usage or git error.
set -eu

mode=tree
require=0
for arg in "$@"; do
    case $arg in
        --staged) mode=staged ;;
        --untracked) mode=untracked ;;
        --history) mode=history ;;
        --identity) mode=identity ;;
        --require) require=1 ;;
        -h | --help)
            sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            printf 'privacy-check: unknown option: %s\n' "$arg" >&2
            exit 2
            ;;
    esac
done

top=$(git rev-parse --show-toplevel 2>/dev/null) || {
    echo "privacy-check: not inside a git work tree" >&2
    exit 2
}
cd "$top"

umask 077
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t privacy-check)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 2' HUP INT TERM
pat="$tmp/patterns"

# Normalise the denylist: drop CRs, trim, skip blanks and comments. An empty
# pattern would match every line, so it must never reach git grep.
normalise() {
    awk '{ sub(/\r$/, ""); sub(/^[ \t]+/, ""); sub(/[ \t]+$/, "");
           if ($0 != "" && substr($0, 1, 1) != "#") print }'
}
if [ -n "${PRIVACY_DENYLIST:-}" ]; then
    source_name='the PRIVACY_DENYLIST variable'
    printf '%s\n' "$PRIVACY_DENYLIST" | normalise >"$pat"
else
    local_list=$(git rev-parse --git-path info/privacy-denylist)
    source_name=$local_list
    if [ -f "$local_list" ]; then
        normalise <"$local_list" >"$pat"
    else
        : >"$pat"
    fi
fi
if [ ! -s "$pat" ]; then
    if [ "$require" -eq 1 ]; then
        echo "privacy-check: no denylist patterns (set PRIVACY_DENYLIST or create .git/info/privacy-denylist)" >&2
        exit 2
    fi
    echo "privacy-check: no denylist (set PRIVACY_DENYLIST or create .git/info/privacy-denylist); skipped."
    exit 0
fi
count=$(wc -l <"$pat" | tr -d ' ')

# denied_lines prints the numbers of the stdin lines that contain a pattern
# (case-insensitive, literal). It uses awk rather than grep -F -i -f, which
# crashes in some Git for Windows builds.
denied_lines() {
    awk -v patfile="$pat" '
        BEGIN { while ((getline l < patfile) > 0) { n++; p[n] = tolower(l) } if (n == 0) exit 2 }
        { s = tolower($0); for (i = 1; i <= n; i++) if (index(s, p[i])) { print NR; next } }'
}

# show_path prints a path, or a stand-in when the path itself is denylisted
# (or cannot be checked).
show_path() {
    if hit=$(printf '%s\n' "$1" | denied_lines) && [ -z "$hit" ]; then
        printf '%s' "$1"
    else
        printf '(a path that contains denylisted text; git ls-files entry %s)' "$2"
    fi
}

if [ "$mode" = identity ]; then
    # git var fails when no identity is configured; that is not a privacy problem.
    ids=$( (git var GIT_AUTHOR_IDENT && git var GIT_COMMITTER_IDENT) 2>/dev/null) || ids=
    if [ -z "$ids" ]; then
        echo "privacy-check: no commit identity configured; nothing to check."
        exit 0
    fi
    hit=$(printf '%s\n' "$ids" | denied_lines) || exit 2
    if [ -n "$hit" ]; then
        echo "privacy-check: the commit author or committer identity (git var GIT_AUTHOR_IDENT / GIT_COMMITTER_IDENT) contains denylisted text (not shown)."
        echo "Commits made now carry it; set a repo-local identity, e.g. a GitHub noreply email, if it should stay private."
        exit 1
    fi
    echo "privacy-check: commit identity is clean ($count patterns from $source_name)."
    exit 0
fi

if [ "$mode" = history ]; then
    if [ -z "$(git rev-list --all -n 1)" ]; then
        echo "privacy-check: no commits yet; history is clean."
        exit 0
    fi
    # %x01 marks the start of each commit's message. Author and committer are
    # left out on purpose (see --identity above).
    #   -m      diff merge commits against each parent, so text introduced
    #           while resolving a merge is scanned (plain -p shows no merge diff)
    #   --text  show binary blobs as text instead of "Binary files differ"
    #   --root  diff the root commit even when log.showRoot is false
    #   prefixes fixed so user diff settings cannot break the path parsing
    git log --all -p -m --text --root --no-color --no-ext-diff --no-textconv \
        --src-prefix=a/ --dst-prefix=b/ \
        --format='%x01commit %H%n%B' >"$tmp/raw" || exit 2
    # NUL bytes from binary blobs become spaces: some awks stop reading a line
    # at a NUL, and a line break could fake a commit header. The C locale makes
    # awk compare bytes, so invalid UTF-8 in binary blobs cannot trip it up;
    # case folding is then ASCII-only (non-ASCII letters must match exactly).
    tr '\000' ' ' <"$tmp/raw" >"$tmp/log"
    LC_ALL=C awk -v patfile="$pat" '
        BEGIN {
            while ((getline line < patfile) > 0) { n++; p[n] = tolower(line) }
            where = "(before the first commit header)"
        }
        function denied(s,   i, ls) {
            ls = tolower(s)
            for (i = 1; i <= n; i++) if (index(ls, p[i])) return 1
            return 0
        }
        substr($0, 1, 1) == "\001" {
            commit = substr($0, 9, 12)
            where = "commit message"
        }
        /^diff --git / {
            path = $0
            k = index(path, " b/")
            if (k == 0) k = index(path, " \"b/")
            path = substr(path, k + 1)
            gsub(/^"?b\//, "", path); gsub(/"$/, "", path)
            where = denied(path) ? "(a path that contains denylisted text)" : path
        }
        denied($0) {
            key = commit " " where
            if (!(key in seen)) { seen[key] = 1; hits[++h] = key }
        }
        END {
            if (h == 0) exit 0
            print "privacy-check: denylisted text found in history (the patterns are not shown):"
            for (i = 1; i <= h; i++) print "  " hits[i]
            exit 1
        }
    ' "$tmp/log" || rc=$?
    if [ "${rc:-0}" -eq 0 ]; then
        echo "privacy-check: history is clean ($count patterns from $source_name)."
        exit 0
    fi
    [ "$rc" -eq 1 ] || exit 2
    echo "Rewriting history is needed to remove it; do not push these commits."
    exit 1
fi

case $mode in
    staged) scope=--cached ;;
    untracked) scope=--untracked ;;
    *) scope= ;;
esac

# 1. File contents, including binary files (-a).
rc=0
# shellcheck disable=SC2086 # $scope is empty or one option
git grep $scope -a -l -z -F -i -f "$pat" >"$tmp/files" 2>"$tmp/err" || rc=$?
if [ "$rc" -gt 1 ]; then
    cat "$tmp/err" >&2
    exit 2
fi

# 2. Path names.
case $mode in
    staged | tree) git ls-files -z --cached >"$tmp/paths" ;;
    untracked) git ls-files -z --cached --others --exclude-standard >"$tmp/paths" ;;
esac
tr '\0' '\n' <"$tmp/paths" | denied_lines >"$tmp/badpaths" || exit 2

found=0
if [ "$rc" -eq 0 ]; then
    found=1
    echo "privacy-check: denylisted text found (the patterns are not shown):"
    tr '\0' '\n' <"$tmp/files" | while IFS= read -r f; do
        # shellcheck disable=SC2086
        lines=$(git grep $scope -I -h -n -F -i -f "$pat" -- ":(literal)$f" | cut -d: -f1 | paste -sd, - || true)
        entry=$(tr '\0' '\n' <"$tmp/paths" | F=$f awk '$0 == ENVIRON["F"] { print NR; exit }')
        if [ -n "$lines" ]; then
            printf '  %s: line %s\n' "$(show_path "$f" "$entry")" "$lines"
        else
            printf '  %s: binary file\n' "$(show_path "$f" "$entry")"
        fi
    done
fi
if [ -s "$tmp/badpaths" ]; then
    found=1
    echo "privacy-check: file names that contain denylisted text (entries of git ls-files):"
    while IFS= read -r n; do
        printf '  entry %s\n' "$n"
    done <"$tmp/badpaths"
fi
if [ "$found" -eq 1 ]; then
    echo "Remove the personal data before committing. The denylist has $count patterns from $source_name."
    exit 1
fi
echo "privacy-check: clean ($mode, $count patterns from $source_name)."
