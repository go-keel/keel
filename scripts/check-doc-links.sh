#!/bin/sh
# Dead doc link check: scans every .md in the repo and checks that the in-repo paths it references exist.
#
# It checks only that paths exist and does **no semantic validation** (no permission codes, config keys or error
# reasons). Docs are narrative (they weigh trade-offs, give counter-examples, write "tried X, rejected"), and a
# validator cannot tell whether an identifier "states a fact" or "is an example"; false positives would soon get it
# switched off. A validator is also code that needs long-term care, and it rots along with the docs, just less
# visibly.
# The semantic level relies on the docs changing in the same commit as the behaviour they describe (AGENTS.md).
set -eu

cd "$(dirname "$0")/.."

# Criterion for a candidate path: the token contains `/` and its first segment is an entry that exists at the repo
# root. This criterion adapts to the repo layout without a maintained allow-list, and also excludes the three most
# common false hits: module paths (g.com/...), MIME types (text/event-stream) and URL paths (/v1/sys/...).
# Cost: a bare file name without `/` (such as a mistyped `NOTES.md` in some md) is not caught; that would need a
# regex guessing "path or noun", whose false-positive cost far outweighs the benefit.
roots=$(ls -A)

report=$(mktemp)
trap 'rm -f "$report"' EXIT

# UPGRADING.md is exempt as a whole: its job is to write "it used to be X, now it is Y", so deleted old paths
# necessarily do not exist, an inherent false positive this check cannot tell apart. The cost is that a mistyped
# **new** path in it is not caught either; new paths in migration guidance follow the other docs, which are checked as usual.
find . -name '*.md' \
	-not -path '*/node_modules/*' \
	-not -path '*/dist/*' \
	-not -path './bin/*' \
	-not -path './UPGRADING.md' \
	-not -path './.git/*' -print |
	while IFS= read -r f; do
		{
			# Only two reference forms are scanned: inline markdown links (anchor cut off) and inline code.
			# Titled `[x](path "title")` and reference-style `[x][ref]` / `[ref]: path`
			# are not scanned: the repo's docs have zero instances, and no regex is kept for hypothetical cases. Dead
			# links written in those two forms pass silently; add them when that happens.
			grep -noE '\]\([^)#]+' "$f" | sed 's/](//' || true
			grep -noE '`[^`]+`' "$f" | tr -d '`' || true
		} | while IFS= read -r hit; do
			line=${hit%%:*}
			tok=${hit#*:}
			# In the form of a Go call argument (kconfig.NewDynamic[…]("configs/app.yaml")) the wrapping quotes after
			# `](` are captured into the token; strip them and check it as a normal path
			tok=${tok#\"}
			tok=${tok%\"}
			case "$tok" in
			# Anything with placeholders, wildcards, spaces or command fragments is not a checkable path
			*' '* | *'<'* | *'>'* | *'*'* | *'{'* | *'|'* | *'('* | *'$'* | *'...'*) continue ;;
			# Go's package.ExportedIdentifier (data.Data, timefmt.RFC3339) is not a path
			*.[A-Z]*) continue ;;
			*/*) ;;
			*) continue ;;
			esac
			# The trailing slash of a directory reference (like docs/adr/) is stripped after the "contains /" check,
			# otherwise a single-segment directory name (contrib/) would be stripped to a bare name and skip the existence check
			tok=${tok%/}

			head=${tok%%/*}
			match=0
			for e in $roots; do
				if [ "$e" = "$head" ]; then
					match=1
					break
				fi
			done
			[ "$match" = 1 ] || continue

			# Both resolution bases count: markdown links are relative to their directory, while paths in prose and
			# inline code are by habit relative to the repo root. Either match counts as existing.
			[ -e "$tok" ] && continue
			[ -e "$(dirname "$f")/$tok" ] && continue
			# Files ignored by .gitignore (like configs/*.dev.yaml) are not supposed to be in the repo anyway
			git check-ignore -q "$tok" 2>/dev/null && continue

			printf '%s:%s: %s\n' "${f#./}" "$line" "$tok" >>"$report"
		done
	done

if [ -s "$report" ]; then
	echo "dead doc links (referenced in-repo paths do not exist):"
	cat "$report"
	exit 1
fi

echo "doc links ok"

# A bare `$var` directly followed by a multi-byte character in shell messages: bash 3.2 (macOS's built-in /bin/sh)
# under a UTF-8 locale takes the first byte of a character like "（" as part of the variable name, looks up a
# variable that does not exist, and exits on the spot under `set -eu`. Writing `${var}` fixes it.
#
# This must be guarded by a machine: it shows up only under a UTF-8 locale, environments with an empty LANG (CI
# containers, agent runners) are all green, and human review cannot see it; with script messages historically in Chinese,
# hitting it was only a matter of time. It has already broken once: a script branch that was meant to skip always
# crashed in a real terminal instead.
#
# LC_ALL=C makes grep look at bytes, and the negated set `[^<tab> -~]` is "not printable ASCII and not
# tab": the first byte of a multi-byte character falls in it, while `${var}` and `$var(` do not match.
#
# The tab is spliced in from a variable rather than typing a raw byte inside the bracket: a raw tab is invisible in
# diffs and editors, and one cannot tell whether it is a tab or a `\t` escape (someone has already misreported it once
# from the source). [:print:] / [:space:] are not used: BSD grep in the C locale also counts 0x80-0xFF as [:print:],
# which would disable this gate entirely, and passing silently is much harder to track down than a false alarm.
tab=$(printf '\t')
bad=$(LC_ALL=C grep -nE '\$[A-Za-z_][A-Za-z0-9_]*[^'"${tab}"' -~]' scripts/*.sh .githooks/* 2>/dev/null || true)
if [ -n "$bad" ]; then
	echo "shell variable directly followed by a multi-byte character (bash 3.2 + UTF-8 reports unbound variable on the spot; rewrite as \${var}):"
	printf '%s\n' "$bad" | sed 's/^/  /'
	exit 1
fi
