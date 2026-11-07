#!/bin/sh
# T1 fixture (WI 5.3, TC-97): prints the environment this script was
# executed with, exactly as execve passed it, one NAME=value per line. It
# reads /proc/$$/environ rather than running env, so nothing the shell
# itself sets or exports is reported.
tr '\0' '\n' </proc/$$/environ
