#!/bin/sh
# Writes every basic example out for GitHub Actions, CircleCI, Argo and
# Temporal, into compiled/github/, compiled/circleci/, compiled/argo/ and
# compiled/temporal/. With --check,
# writes nothing and fails if any of them is not what its script compiles to
# now. A script one of those cannot carry over (files handed from one step to
# another, packages, a service's files) is skipped for it.
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

# The images the Argo files name; they are placeholders.
init=registry.example/starci-init:latest
bare=registry.example/starci-bare:latest

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
	# Not every script can be a pod; a refusal is not a stale file.
	if "$starci" compile --offline --target argo --init-image "$init" --bare-image "$bare" --file "$script" >/dev/null 2>&1; then
		out=compiled/argo/$name.yml
		if [ -n "$check" ]; then
			"$starci" compile --offline --target argo --init-image "$init" --bare-image "$bare" --file "$script" --check "$out" >/dev/null || {
				echo "stale: $out" >&2
				status=1
			}
		else
			mkdir -p compiled/argo
			"$starci" compile --offline --target argo --init-image "$init" --bare-image "$bare" --file "$script" >"$out"
		fi
	fi
	# A directory, not a file.
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
