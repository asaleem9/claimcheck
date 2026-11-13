#!/usr/bin/env bash
# Creates a kind cluster on Kubernetes v1.37 with kubernetes-sigs/dra-example-driver,
# which publishes 8 mock GPUs per worker node. No real hardware needed.
set -euo pipefail

CLUSTER="${CLUSTER:-claimcheck}"
K8S_VERSION="${K8S_VERSION:-v1.37.0}"
DRIVER_VERSION="${DRIVER_VERSION:-v0.5.0}"
# GPU_PARTITIONS > 0 publishes each mock GPU as that many partitions plus a
# full device, sharing memory and compute counters.
GPU_PARTITIONS="${GPU_PARTITIONS:-0}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# KIND_CONFIG picks the cluster layout, e.g. hack/kind/kind-gang.yaml for the
# PodGroup API.
KIND_CONFIG="${KIND_CONFIG:-${HERE}/kind.yaml}"
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
    --config "${KIND_CONFIG}" --wait 2m
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
  --set image.tag="${DRIVER_VERSION}" --set kubeletPlugin.gpuPartitions="${GPU_PARTITIONS}" \
  --set deviceClass.extendedResourceName=example.com/gpu --wait --timeout 5m

echo "waiting for ResourceSlices from both workers..."
for _ in $(seq 60); do
  n=$(kubectl get resourceslices -o name 2>/dev/null | wc -l | tr -d ' ')
  counters=$(kubectl get resourceslices -o jsonpath='{range .items[*]}{.spec.sharedCounters[0].name}{"\n"}{end}' 2>/dev/null | grep -c . || true)
  if [[ "${n}" -ge 2 && ( "${GPU_PARTITIONS}" == 0 && "${counters}" == 0 || "${GPU_PARTITIONS}" != 0 && "${counters}" -ge 2 ) ]]; then
    break
  fi
  sleep 2
done
kubectl get resourceslices
echo "cluster kind-${CLUSTER} is ready"
