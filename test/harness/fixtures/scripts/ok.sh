#!/bin/sh
# T1 fixture (WI 5.3, TC-95): prints its progress, as the user's deploy
# scripts do, on both streams, and succeeds.
echo "ok: starting"
echo "ok: step 1 of 2"
echo "ok: step 2 of 2" >&2
echo "ok: done"
