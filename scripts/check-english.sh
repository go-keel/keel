#!/usr/bin/env bash
# Fails when a tracked text file contains Chinese characters or full-width punctuation, outside the paths listed in
# scripts/english-allow.list. This repository's own docs, comments and messages are English; deliberate test
# data and the like are the listed exceptions, each with its reason.
set -euo pipefail
cd "$(dirname "$0")/.."

allow=scripts/english-allow.list
patterns=()
while IFS= read -r line || [ -n "$line" ]; do
	line=${line%%#*}
	line=$(printf '%s' "$line" | tr -d '[:space:]')
	[ -n "$line" ] && patterns+=("$line")
done < "$allow"

allowed() {
	local f=$1 p
	for p in "${patterns[@]}"; do
		# shellcheck disable=SC2053 # glob match on purpose
		[[ $f == $p ]] && return 0
	done
	return 1
}

found=0
while IFS= read -r -d '' f; do
	allowed "$f" && continue
	[ -f "$f" ] || continue
	if out=$(perl -CSD -ne 'print "$ARGV:$.: $_" if /[\p{Han}\x{3000}-\x{303F}\x{FF01}-\x{FF60}]/' "$f" 2>/dev/null) && [ -n "$out" ]; then
		printf '%s\n' "$out"
		found=1
	fi
done < <(git ls-files -z --cached --others --exclude-standard)

if [ "$found" -ne 0 ]; then
	echo "Chinese text above: this repository's own content is English. Translate it, or, for deliberate test data,"
	echo "add the path to $allow with the reason."
	exit 1
fi
echo "english ok"
