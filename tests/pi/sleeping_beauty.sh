#!/bin/bash
# Sleeping Beauty's enchanted sleep — a phase31 TC-f (busy-indicator) test fixture.
#
# Purpose: simulate a long-running FOREGROUND task so the test can observe the busy indicator while the
# session is occupied. The wait lives inside this script and ends deterministically when the test creates
# the "prince" sentinel ("<script-dir>/prince-arrived"); a hard MAX ceiling guarantees the script always
# returns even if the sentinel never appears, so a run can never hang.
#
# The test copies this into the session working directory and runs it as ./sleeping_beauty.sh, so the
# sentinel resolves to that same directory.
DIR="$(cd "$(dirname "$0")" && pwd)"
SENTINEL="$DIR/prince-arrived"
MAX="${1:-240}"   # hard ceiling in seconds; the prince (sentinel) normally wakes her long before this
echo "Sleeping Beauty ate the poisoned apple and fell into an enchanted sleep."
for ((waited = 0; waited < MAX; waited++)); do
  [ -e "$SENTINEL" ] && break
  sleep 1
done
echo "The prince arrived and woke Sleeping Beauty; she opened her eyes."
