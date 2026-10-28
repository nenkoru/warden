#!/bin/sh
# Runs every Go fuzz target (func FuzzX in a _test.go file) for FUZZTIME
# each (default 20s). A new target is picked up without touching CI. A crash
# fails the run; Go writes the input to testdata/fuzz/FuzzX/ — commit it with
# the fix so it stays a regression test.
set -eu
cd "$(dirname "$0")/.."
FUZZTIME=${FUZZTIME:-20s}
n=0
for f in $(grep -rl --include='*_test.go' '^func Fuzz' . | sort); do
	pkg=./$(dirname "${f#./}")
	for t in $(sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\)(.*/\1/p' "$f"); do
		echo "== $pkg $t ($FUZZTIME)"
		go test -run='^$' -fuzz="^$t\$" -fuzztime="$FUZZTIME" "$pkg"
		n=$((n + 1))
	done
done
echo "== $n fuzz targets, no crash"
