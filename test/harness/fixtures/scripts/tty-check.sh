#!/bin/sh
# SPDX-FileCopyrightText: 2026 Mauktik Gandhi
# SPDX-License-Identifier: Apache-2.0
# T1 fixture (WI 5.3, TC-98): reports whether any standard stream is a
# terminal, whether there is a controlling terminal, and what a read of
# stdin gets. Under Nexus every stream is a pipe or /dev/null, there is no
# controlling terminal, and the read gets EOF at once.
echo "tty: $(tty 2>&1)"
for fd in 0 1 2; do
	if [ -t "$fd" ]; then
		echo "fd$fd: terminal"
	else
		echo "fd$fd: not a terminal"
	fi
done
if (: </dev/tty) 2>/dev/null; then
	echo "ctty: present"
else
	echo "ctty: none"
fi
if read -r line; then
	echo "read: got a line"
else
	echo "read: EOF"
fi
