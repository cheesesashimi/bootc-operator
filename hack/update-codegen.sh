#!/usr/bin/env bash

# Generates the typed clientset, listers, informers and applyconfigurations for
# the bootc-operator CRD API group (node.bootc.dev/v1alpha1) under pkg/generated.
#
# The k8s.io/code-generator commands are invoked via `go run`. Because this
# project uses the single-group kubebuilder layout (api/v1alpha1 rather than
# api/<group>/<version>), the generators are called directly with the
# fully-qualified input package and an empty --input-base; kube_codegen.sh's
# path heuristics assume the multi-group layout and are not used here.

set -o errexit
set -o nounset
set -o pipefail

# Force module mode so `go run` can resolve the generator commands from the
# module cache even though the vendor dir only contains the packages imported by
# tools.go.
export GOFLAGS="${GOFLAGS:-} -mod=mod"

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULE="github.com/bootc-dev/bootc-operator"
API_PKG="${MODULE}/api/v1alpha1"
OUT_PKG="${MODULE}/pkg/generated"
OUT_DIR="${SCRIPT_ROOT}/pkg/generated"
BOILERPLATE="${SCRIPT_ROOT}/hack/boilerplate.generated.go.txt"
PLURALS="BootcNode:bootcnodes,BootcNodePool:bootcnodepools"

gen() { go run "k8s.io/code-generator/cmd/$1" "${@:2}"; }

cd "${SCRIPT_ROOT}"
rm -rf "${OUT_DIR}"
mkdir -p "${OUT_DIR}"

# NOTE: deepcopy (zz_generated.deepcopy.go) is owned by controller-gen via
# `make generate` and is intentionally NOT regenerated here.

echo "Generating applyconfigurations..."
gen applyconfiguration-gen \
    --go-header-file "${BOILERPLATE}" \
    --output-dir "${OUT_DIR}/applyconfiguration" \
    --output-pkg "${OUT_PKG}/applyconfiguration" \
    "${API_PKG}"

echo "Generating clientset..."
gen client-gen \
    --go-header-file "${BOILERPLATE}" \
    --clientset-name "versioned" \
    --input-base "" \
    --input "${API_PKG}" \
    --output-dir "${OUT_DIR}/clientset" \
    --output-pkg "${OUT_PKG}/clientset" \
    --apply-configuration-package "${OUT_PKG}/applyconfiguration" \
    --plural-exceptions "${PLURALS}"

echo "Generating listers..."
gen lister-gen \
    --go-header-file "${BOILERPLATE}" \
    --output-dir "${OUT_DIR}/listers" \
    --output-pkg "${OUT_PKG}/listers" \
    --plural-exceptions "${PLURALS}" \
    "${API_PKG}"

echo "Generating informers..."
gen informer-gen \
    --go-header-file "${BOILERPLATE}" \
    --versioned-clientset-package "${OUT_PKG}/clientset/versioned" \
    --listers-package "${OUT_PKG}/listers" \
    --output-dir "${OUT_DIR}/informers" \
    --output-pkg "${OUT_PKG}/informers" \
    --plural-exceptions "${PLURALS}" \
    "${API_PKG}"

echo "Code generation complete: ${OUT_DIR}"
