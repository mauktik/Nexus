#!/bin/sh
# T1 fixture (WI 5.3, TC-95): reports a failure and exits non-zero, as the
# user's scripts are expected to (A-6).
echo "fail: starting"
echo "fail: something went wrong" >&2
exit 3
