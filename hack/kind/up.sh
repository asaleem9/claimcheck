#!/usr/bin/env bash
# Creates a kind cluster on Kubernetes v1.37 with kubernetes-sigs/dra-example-driver,
# which publishes 8 mock GPUs per worker node. No real hardware needed.
set -euo pipefail

CLUSTER="${CLUSTER:-claimcheck}"
K8S_VERSION="${K8S_VERSION:-v1.37.0}"
DRIVER_VERSION="${DRIVER_VERSION:-v0.5.0}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE="${HERE}/.cache"

# Use an installed helm if there is one (type -P ignores this function),
# otherwise build a pinned version with the Go toolchain.
helm() {
  if type -P helm >/dev/null 2>&1; then
    command helm "$@"
  else
    go run helm.sh/helm/v3/cmd/helm@v3.19.0 "$@"
  fi
}

if ! kind get clusters | grep -qx "${CLUSTER}"; then
  kind create cluster --name "${CLUSTER}" --image "kindest/node:${K8S_VERSION}" \
    --config "${HERE}/kind.yaml" --wait 2m
fi
kubectl config use-context "kind-${CLUSTER}" >/dev/null

if [[ ! -d "${CACHE}/dra-example-driver" ]]; then
  mkdir -p "${CACHE}"
  git clone --quiet --depth 1 --branch "${DRIVER_VERSION}" \
    https://github.com/kubernetes-sigs/dra-example-driver "${CACHE}/dra-example-driver" 2>/dev/null ||
    git clone --quiet --depth 1 https://github.com/kubernetes-sigs/dra-example-driver "${CACHE}/dra-example-driver"
fi

helm upgrade --install --create-namespace --namespace dra-example-driver dra-example-driver \
  "${CACHE}/dra-example-driver/deployments/helm/dra-example-driver" \
  --set image.tag="${DRIVER_VERSION}" --wait --timeout 5m

echo "waiting for ResourceSlices from both workers..."
for _ in $(seq 60); do
  n=$(kubectl get resourceslices -o name 2>/dev/null | wc -l | tr -d ' ')
  [[ "${n}" -ge 2 ]] && break
  sleep 2
done
kubectl get resourceslices
echo "cluster kind-${CLUSTER} is ready"
