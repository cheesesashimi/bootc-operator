#!/usr/bin/env bash
# undeploy-openshift.sh - Restore an OpenShift cluster managed by bootc-operator
#                         back to its original OS image, then remove the operator.
#
# Usage:
#   hack/undeploy-openshift.sh
#
# What this script does:
#   1. For each MachineConfigPool, reads spec.osImageURL from its rendered
#      MachineConfig and ensures the matching BootcNodePool targets that image.
#   2. Waits for every BootcNodePool to reach UpToDate=True (AllUpdated).
#   3. Deletes all BootcNodePool CRs.
#   4. Removes the bootc-operator Deployment and DaemonSet.
#   5. Scales the cluster-version-operator back to 1 replica so it can
#      restore the MCO and other managed components.
#
# Prerequisites in PATH: oc or kubectl, jq

set -euo pipefail

# ---------------------------------------------------------------------------
# 1. Validate prerequisites
# ---------------------------------------------------------------------------

echo "==> Checking prerequisites..."

if ! command -v jq &>/dev/null; then
  echo "ERROR: 'jq' not found in PATH" >&2
  exit 1
fi

if command -v oc &>/dev/null; then
  OC=oc
elif command -v kubectl &>/dev/null; then
  OC=kubectl
else
  echo "ERROR: neither 'oc' nor 'kubectl' found in PATH" >&2
  exit 1
fi

echo "    Using '${OC}' as the Kubernetes CLI."

# How long to wait (in seconds) for each pool to become UpToDate before
# giving up. Override with WAIT_TIMEOUT_SECONDS=<n> in the environment.
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-3600}"

# ---------------------------------------------------------------------------
# 2. For each MachineConfigPool, reconcile the matching BootcNodePool image
# ---------------------------------------------------------------------------

echo "==> Reading MachineConfigPools and reconciling BootcNodePool images..."

MCP_NAMES="$("${OC}" get mcp -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"

if [[ -z "${MCP_NAMES}" ]]; then
  echo "ERROR: No MachineConfigPools found in the cluster." >&2
  exit 1
fi

while IFS= read -r MCP_NAME; do
  [[ -z "${MCP_NAME}" ]] && continue

  MC_NAME="$("${OC}" get "mcp/${MCP_NAME}" -o json | jq -r '.spec.configuration.name')"
  OS_IMAGE="$("${OC}" get "mc/${MC_NAME}" -o json | jq -r '.spec.osImageURL')"

  if [[ -z "${OS_IMAGE}" || "${OS_IMAGE}" == "null" ]]; then
    echo "WARNING: No osImageURL for MCP '${MCP_NAME}' (mc/${MC_NAME}); skipping." >&2
    continue
  fi

  echo "    MCP '${MCP_NAME}': ${OS_IMAGE}"

  # Check whether a BootcNodePool for this MCP already exists.
  if "${OC}" get bootcnodepool "${MCP_NAME}" &>/dev/null; then
    # Patch image.ref in-place if it differs.
    CURRENT_IMAGE="$("${OC}" get bootcnodepool "${MCP_NAME}" \
      -o jsonpath='{.spec.image.ref}')"
    if [[ "${CURRENT_IMAGE}" != "${OS_IMAGE}" ]]; then
      echo "    Updating BootcNodePool '${MCP_NAME}' image: ${CURRENT_IMAGE} -> ${OS_IMAGE}"
      "${OC}" patch bootcnodepool "${MCP_NAME}" \
        --type=merge \
        -p "{\"spec\":{\"image\":{\"ref\":\"${OS_IMAGE}\"}}}"
    else
      echo "    BootcNodePool '${MCP_NAME}' already targets the correct image."
    fi
  else
    echo "    Creating BootcNodePool '${MCP_NAME}'..."
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
  fi
done <<<"${MCP_NAMES}"

# ---------------------------------------------------------------------------
# 3. Wait for every BootcNodePool to reach UpToDate=True
# ---------------------------------------------------------------------------

echo "==> Waiting for all BootcNodePools to be UpToDate (timeout: ${WAIT_TIMEOUT_SECONDS}s)..."

POLL_INTERVAL=15
ELAPSED=0

while true; do
  ALL_UPDATED=true
  POOL_NAMES="$("${OC}" get bootcnodepool \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"

  while IFS= read -r POOL_NAME; do
    [[ -z "${POOL_NAME}" ]] && continue

    # Read the UpToDate condition status and reason.
    CONDITION="$("${OC}" get bootcnodepool "${POOL_NAME}" -o json |
      jq -r '
              (.status.conditions // [])
              | map(select(.type == "UpToDate"))
              | if length > 0
                then .[0] | "\(.status)/\(.reason)"
                else "Unknown/Unknown"
                end
            ')"

    STATUS="${CONDITION%%/*}"
    REASON="${CONDITION##*/}"

    NODE_COUNT="$("${OC}" get bootcnodepool "${POOL_NAME}" \
      -o jsonpath='{.status.nodeCount}' 2>/dev/null || echo "?")"
    UPDATED_COUNT="$("${OC}" get bootcnodepool "${POOL_NAME}" \
      -o jsonpath='{.status.updatedCount}' 2>/dev/null || echo "?")"

    echo "    [${POOL_NAME}] UpToDate=${STATUS} reason=${REASON}" \
      "nodes=${UPDATED_COUNT}/${NODE_COUNT}"

    if [[ "${STATUS}" != "True" || "${REASON}" != "AllUpdated" ]]; then
      ALL_UPDATED=false
    fi
  done <<<"${POOL_NAMES}"

  if [[ "${ALL_UPDATED}" == "true" ]]; then
    echo "    All BootcNodePools are UpToDate."
    break
  fi

  if ((ELAPSED >= WAIT_TIMEOUT_SECONDS)); then
    echo "ERROR: Timed out after ${WAIT_TIMEOUT_SECONDS}s waiting for BootcNodePools." >&2
    exit 1
  fi

  echo "    Waiting ${POLL_INTERVAL}s... (${ELAPSED}s elapsed)"
  sleep "${POLL_INTERVAL}"
  ((ELAPSED += POLL_INTERVAL)) || true
done

# ---------------------------------------------------------------------------
# 4. Delete all BootcNodePool CRs
# ---------------------------------------------------------------------------

echo "==> Deleting all BootcNodePool CRs..."
"${OC}" delete bootcnodepool --all --ignore-not-found

# ---------------------------------------------------------------------------
# 5. Remove the bootc-operator Deployment and DaemonSet
# ---------------------------------------------------------------------------

echo "==> Removing bootc-operator Deployment and DaemonSet..."
"${OC}" delete deployment \
  -n bootc-operator \
  -l app.kubernetes.io/name=bootc-operator \
  --ignore-not-found

"${OC}" delete daemonset \
  -n bootc-operator \
  -l app.kubernetes.io/name=bootc-operator \
  --ignore-not-found

# ---------------------------------------------------------------------------
# 6. Restore the cluster-version-operator to 1 replica
# ---------------------------------------------------------------------------

echo "==> Scaling cluster-version-operator back to 1 replica..."
"${OC}" scale deployment/cluster-version-operator --replicas 1 \
  -n openshift-cluster-version

echo "==> Done. The cluster-version-operator will restore MCO and other managed components."
