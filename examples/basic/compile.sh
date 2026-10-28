#!/bin/sh
# Writes every basic example out for GitHub Actions and CircleCI, into
# compiled/github/ and compiled/circleci/. With --check, writes nothing and
# fails if any of them is not what its script compiles to now.
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
done
exit $status
