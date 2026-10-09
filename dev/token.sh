#!/bin/sh
# Prints an env file with worker credentials for the dev Hatchet stack.
#   dev/token.sh > dev/.env
set -eu
cd "$(dirname "$0")"
token=$(docker compose -p starci-dev exec -T hatchet ./hatchet-admin token create \
  --config ./config --name starci-dev --tenant-id 707d0855-80ab-4e1f-a156-f1c4546cbf52 | tail -n 1)
printf 'HATCHET_CLIENT_TOKEN=%s\nHATCHET_CLIENT_TLS_STRATEGY=none\n' "$token"
