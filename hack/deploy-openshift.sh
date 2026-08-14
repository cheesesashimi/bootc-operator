#!/usr/bin/env bash
# deploy-openshift.sh - Deploy bootc-operator into an OpenShift cluster.
#
# Usage:
#   IMG=<pullspec> hack/deploy-openshift.sh
#
# Required environment variable:
#   IMG  - the bootc-operator container image pullspec to deploy
#
# Prerequisites in PATH: oc or kubectl, yq, jq, make
#
# The script modifies config/rbac/daemon_role.yaml and
# config/daemon/daemon.yaml in-place (idempotently) but does NOT commit
# the changes.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# 1. Validate prerequisites
# ---------------------------------------------------------------------------

echo "==> Checking prerequisites..."

for cmd in yq jq make; do
    if ! command -v "${cmd}" &>/dev/null; then
        echo "ERROR: '${cmd}' not found in PATH" >&2
        exit 1
    fi
done

if command -v oc &>/dev/null; then
    OC=oc
elif command -v kubectl &>/dev/null; then
    OC=kubectl
else
    echo "ERROR: neither 'oc' nor 'kubectl' found in PATH" >&2
    exit 1
fi

echo "    Using '${OC}' as the Kubernetes CLI."

if [[ -z "${IMG:-}" ]]; then
    echo "ERROR: IMG environment variable must be set to the bootc-operator image pullspec." >&2
    exit 1
fi

echo "    IMG=${IMG}"

# ---------------------------------------------------------------------------
# 2. Scale down the cluster-version-operator
# ---------------------------------------------------------------------------

echo "==> Scaling down cluster-version-operator..."
"${OC}" scale deployment/cluster-version-operator --replicas 0 \
    -n openshift-cluster-version

# ---------------------------------------------------------------------------
# 3. Scale down the machine-config-operator
# ---------------------------------------------------------------------------

echo "==> Scaling down machine-config-operator..."
"${OC}" scale deployment/machine-config-operator --replicas 0 \
    -n openshift-machine-config-operator

# ---------------------------------------------------------------------------
# 4. Delete machine-config-controller and the MCO daemonsets
# ---------------------------------------------------------------------------

echo "==> Deleting machine-config-controller deployment..."
"${OC}" delete deployment/machine-config-controller \
    -n openshift-machine-config-operator --ignore-not-found

echo "==> Deleting machine-config-daemon daemonset..."
"${OC}" delete daemonset/machine-config-daemon \
    -n openshift-machine-config-operator --ignore-not-found

echo "==> Deleting machine-config-server daemonset..."
"${OC}" delete daemonset/machine-config-server \
    -n openshift-machine-config-operator --ignore-not-found

# ---------------------------------------------------------------------------
# 5. Patch config/rbac/daemon_role.yaml - ensure OpenShift SCC rule is present
# ---------------------------------------------------------------------------

DAEMON_ROLE="${REPO_ROOT}/config/rbac/daemon_role.yaml"

echo "==> Patching ${DAEMON_ROLE} (idempotent)..."
SCC_COUNT="$(yq '.rules | map(select(.apiGroups[] == "security.openshift.io")) | length' \
    "${DAEMON_ROLE}")"
if [[ "${SCC_COUNT}" -eq 0 ]]; then
    yq -i '.rules += [{"apiGroups": ["security.openshift.io"], "resourceNames": ["privileged"], "resources": ["securitycontextconstraints"], "verbs": ["use"]}]' \
        "${DAEMON_ROLE}"
    echo "    SCC rule added."
else
    echo "    SCC rule already present; skipping."
fi

# ---------------------------------------------------------------------------
# 6. Patch config/daemon/daemon.yaml - add required-scc annotation
# ---------------------------------------------------------------------------

DAEMON_YAML="${REPO_ROOT}/config/daemon/daemon.yaml"

echo "==> Patching ${DAEMON_YAML} (idempotent)..."
yq -i '
  .spec.template.metadata.annotations["openshift.io/required-scc"] = "privileged"
' "${DAEMON_YAML}"

# ---------------------------------------------------------------------------
# 7. Deploy via make
# ---------------------------------------------------------------------------

echo "==> Running 'make deploy' with IMG=${IMG}..."
IMG="${IMG}" make -C "${REPO_ROOT}" deploy

# ---------------------------------------------------------------------------
# 8. Retrieve OS image pullspecs from all MachineConfigPools and create
#    one BootcNodePool per MCP.
# ---------------------------------------------------------------------------

echo "==> Retrieving MachineConfigPools and their OS image pullspecs..."

# Collect MCP names as a newline-separated list.
MCP_NAMES="$("${OC}" get mcp -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"

if [[ -z "${MCP_NAMES}" ]]; then
    echo "ERROR: No MachineConfigPools found in the cluster." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# 9. Create one BootcNodePool CR per MachineConfigPool
# ---------------------------------------------------------------------------

echo "==> Applying BootcNodePool CRs..."

while IFS= read -r MCP_NAME; do
    [[ -z "${MCP_NAME}" ]] && continue

    MC_NAME="$("${OC}" get "mcp/${MCP_NAME}" -o json | jq -r '.spec.configuration.name')"
    OS_IMAGE="$("${OC}" get "mc/${MC_NAME}" -o json | jq -r '.spec.osImageURL')"

    if [[ -z "${OS_IMAGE}" || "${OS_IMAGE}" == "null" ]]; then
        echo "WARNING: Could not retrieve osImageURL for MCP '${MCP_NAME}' (mc/${MC_NAME}); skipping." >&2
        continue
    fi

    echo "    MCP '${MCP_NAME}': ${OS_IMAGE}"

    "${OC}" apply -f - <<EOF
apiVersion: node.bootc.dev/v1alpha1
kind: BootcNodePool
metadata:
  name: ${MCP_NAME}
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/${MCP_NAME}: ""
  image:
    ref: ${OS_IMAGE}
EOF
done <<< "${MCP_NAMES}"

echo "==> Done. bootc-operator is deployed and BootcNodePools are configured."
