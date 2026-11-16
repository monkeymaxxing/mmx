#!/bin/sh
# Every stand-in program that stubTools (official_extra_modules_test.go) makes
# is a link to this file: incus, sfdisk, rabbitmqctl, and the others. A test
# links it under the program's name in a directory of its own, and writes
# what the program does, as a shell script, into that directory's
# subdirectory "bodies" under the same name. This file notes the call in the
# directory's file "calls" and runs that body, which reads its arguments as
# "$@" and can keep files in the directory $STUBS.
#
# One file for all stand-ins, and not a new script for each: a machine can
# take half a second for the first start of a new executable file, as macOS
# does, and a link to a file that has started before starts at once.
STUBS=${0%/*}
printf '%s\n' "${0##*/} $*" >> "$STUBS/calls"
. "$STUBS/bodies/${0##*/}"
