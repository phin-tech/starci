#!/bin/sh
# Builds or serves the documentation with Zensical, which reads mkdocs.yml.
#
#   dev/docs.sh          build into site/, failing on a broken link
#   dev/docs.sh serve    serve it, rebuilding as pages change
#
# docs/examples is a copy of examples/ with a page written for each example,
# made here and not kept in git: Zensical does not follow a link to a
# directory outside docs/.
set -eu

cd "$(dirname "$0")/.."
rm -rf docs/examples
cp -R examples docs/examples
python3 dev/docs-examples.py

exec uvx --from 'zensical==0.0.73' zensical "${1:-build}"
