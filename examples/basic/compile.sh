#!/bin/sh
# Writes every basic example out for GitHub Actions, CircleCI and Temporal, into
# compiled/github/, compiled/circleci/ and compiled/temporal/. With --check,
# writes nothing and fails if any of them is not what its script compiles to
# now. A script that hands files from one step to another has no Temporal
# module yet, and is skipped for it.
#
#   examples/basic/compile.sh
#   examples/basic/compile.sh --check
#
# Uses `starci` from PATH, or STARCI if set. --offline: the examples name
# tools that need not be downloaded just to show what comes out.
set -eu

starci=${STARCI:-starci}
here=$(cd "$(dirname "$0")" && pwd)
cd "$here"

check=
[ "${1:-}" = "--check" ] && check=1

status=0
for script in *.ci.star; do
	name=${script%.ci.star}
	for target in github circleci; do
		out=compiled/$target/$name.yml
		if [ -n "$check" ]; then
			"$starci" compile --offline --target "$target" --file "$script" --check "$out" >/dev/null || {
				echo "stale: $out" >&2
				status=1
			}
		else
			mkdir -p "compiled/$target"
			"$starci" compile --offline --target "$target" --file "$script" >"$out"
		fi
	done
	# A directory, not a file; refused for a script that hands artifacts on.
	if grep -q 'artifacts *=' "$script"; then
		continue
	fi
	out=compiled/temporal/$name
	if [ -n "$check" ]; then
		"$starci" compile --offline --target temporal --file "$script" --check "$out" >/dev/null || {
			echo "stale: $out" >&2
			status=1
		}
	else
		"$starci" compile --offline --target temporal --file "$script" -o "$out" 2>/dev/null
	fi
done
exit $status
