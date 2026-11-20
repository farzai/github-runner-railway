#!/bin/sh
# Test command for mutator: test one mutant's package, then log its verdict.
#
# mutator runs each mutant in a worker directory under target/mutation-workers: symlinks to the real tree plus a
# private copy of the mutated file, and this script runs there. Go's build cache is safe to share, so mutants of one
# file run in parallel (gate.py sets --max-workers).
#
# mutator prints its verdicts only once a file is done, so a run the gate stops at the budget would report none.
# This script therefore appends "<line><TAB>killed|survived" to QUALITY_MUTANT_LOG for each mutant it finishes;
# <line> is where the mutated copy first differs from the original in QUALITY_ROOT, which is the line of the
# mutated token. A run on the unchanged file (mutator's baseline, or the gate's timing run) differs nowhere and logs
# nothing.
#
# Only the mutated file's own package is tested, as mutator does for Go by default. The coverage profile is built
# the same way (go test without -coverpkg), so a line counts as covered only when its own package's tests run it.
#
# Everything comes from the environment gate.py sets, so no file name is ever part of a command string:
#   MUTANT_FILE           the source file, relative to the repository root
#   QUALITY_ROOT          the repository root, where the original file is
#   QUALITY_MUTANT_LOG    the file the verdicts are appended to
set -eu

file=${MUTANT_FILE:?MUTANT_FILE is not set}
root=${QUALITY_ROOT:?QUALITY_ROOT is not set}
log=${QUALITY_MUTANT_LOG:?QUALITY_MUTANT_LOG is not set}

# cmp exits 0 when the files are equal, 1 when they differ (and prints "... line N"; GNU says byte, macOS char),
# and 2 on trouble, which logs nothing. When one file is a prefix of the other it prints only "EOF on <file>" to
# stderr, so the line comes from the files: the first difference is just past the end of the shorter one.
line=
differ=0
difference=$(cmp -- "$root/$file" "$file") || differ=$?
if [ "$differ" -eq 1 ]; then
  line=$(printf '%s\n' "$difference" | sed -n 's/.* line \([0-9][0-9]*\)$/\1/p')
  if [ -z "$line" ]; then
    shorter=$file
    [ "$(wc -c < "$root/$file")" -lt "$(wc -c < "$file")" ] && shorter=$root/$file
    line=$(($(wc -l < "$shorter") + 1))
  fi
fi

code=0
go test -count=1 "./$(dirname -- "$file")" || code=$?

if [ -n "$line" ]; then
  if [ "$code" -eq 0 ]; then verdict=survived; else verdict=killed; fi
  printf '%s\t%s\n' "$line" "$verdict" >> "$log"
fi
exit "$code"
