#!/bin/sh
# Re-records transfer/testdata/golden-v0.98.0-beta.json and
# golden-v0.98.0-beta-inline.ndjson by running writeGoldenRecords through the
# released v0.98.0-beta writer. Run it after changing writeGoldenRecords; the
# output must then be committed with the change.
#
#   transfer/testdata/golden-recorder/record.sh
#
# The generator is copied in from ../../golden_records_test.go at run time and
# removed afterwards, so the recorder can never run a stale copy of it.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
trap 'rm -f "$here/records.go"' EXIT
sed -e 's/^package transfer_test$/package main/' "$here/../../golden_records_test.go" > "$here/records.go"
cd "$here"
go run . "$here/.."
