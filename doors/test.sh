#!/bin/sh
# Runs the reference doors against a stand-in for Hatchet's webhook address.
set -eu
cd "$(dirname "$0")"

port=18997
secret=door-test-secret
log=$(mktemp)
python3 standin_hatchet.py "$port" "$secret" "$log" &
standin=$!
trap 'kill "$standin" 2>/dev/null; rm -f "$log"' EXIT
# Wait for it to listen.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if python3 -c "import socket; socket.create_connection(('127.0.0.1', $port), 1)" 2>/dev/null; then break; fi
  sleep 0.3
done

status=0
echo "Cloudflare Worker"
node cloudflare-worker/test.mjs "http://127.0.0.1:$port" "$secret" "$log" || status=1
echo "AWS Lambda"
(cd aws-lambda && python3 test_handler.py "http://127.0.0.1:$port" "$secret" "$log") || status=1
exit $status
