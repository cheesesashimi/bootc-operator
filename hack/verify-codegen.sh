#!/usr/bin/env bash

# Verifies that the generated client code in pkg/generated is up to date with the
# API types. Fails if running update-codegen.sh would produce a diff.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

DIFFROOT="${SCRIPT_ROOT}/pkg/generated"
TMP_DIFFROOT="$(mktemp -d)"
trap 'rm -rf "${TMP_DIFFROOT}"' EXIT

if [[ -d "${DIFFROOT}" ]]; then
    cp -a "${DIFFROOT}" "${TMP_DIFFROOT}/generated"
fi

"${SCRIPT_ROOT}/hack/update-codegen.sh"

if ! diff -Naupr "${DIFFROOT}" "${TMP_DIFFROOT}/generated" >/dev/null; then
    echo "ERROR: generated client is out of date. Run hack/update-codegen.sh" >&2
    diff -Naupr "${DIFFROOT}" "${TMP_DIFFROOT}/generated" || true
    exit 1
fi

echo "Generated client is up to date."
