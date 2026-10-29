#!/bin/sh
# Writes every basic example out for GitHub Actions and CircleCI, into
# compiled/github/ and compiled/circleci/. With --check, writes nothing and
# fails if any of them is not what its script compiles to now.
#
#   examples/basic/compile.sh
#   examples/basic/compile.sh --check
#
# Uses `starci` from PATH, or STARCI if set. --offline: the examples name
# tools that need not be downloaded just to show what comes out. A script
# that hands artifacts on is refused by both targets for now, and has no
# compiled files.
set -eu

starci=${STARCI:-starci}
here=$(cd "$(dirname "$0")" && pwd)
cd "$here"

check=
[ "${1:-}" = "--check" ] && check=1

# refused says whether a script is one compiling refuses for now.
refused() {
	! "$starci" compile --offline --target github --file "$1" >/dev/null 2>&1 &&
		"$starci" compile --offline --target github --file "$1" 2>&1 | grep -q "artifacts are not compiled"
}

status=0
for script in *.ci.star; do
	name=${script%.ci.star}
	if refused "$script"; then
		for target in github circleci; do
			if [ -e "compiled/$target/$name.yml" ]; then
				if [ -n "$check" ]; then
					echo "stale: compiled/$target/$name.yml, for a script compiling refuses" >&2
					status=1
				else
					rm "compiled/$target/$name.yml"
				fi
			fi
		done
		continue
	fi
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
