#!/usr/bin/env bash
# End to end against the real CSI-Addons controller, in a throwaway kind
# cluster: a ReclaimSpaceJob must succeed through the upstream k8s-sidecar and
# this image, and FITRIM must free the volume's deleted blocks on the backing
# file. Needs docker, kind, kubectl, and network access to
# raw.githubusercontent.com, quay.io and Docker Hub.
#   test/e2e/run.sh <image>     (KEEP=1 keeps the cluster for debugging)
set -euo pipefail

IMAGE=${1:?usage: test/e2e/run.sh <image>}
CLUSTER=csi-reclaimspace-e2e
NODE=$CLUSTER-control-plane
NS=csi-reclaimspace-e2e
DRIVER=e2e.csi-reclaimspace.test
HANDLE=e2e-volume
HERE=$(cd "$(dirname "$0")" && pwd)

KUBECONFIG=$(mktemp)
export KUBECONFIG
diagnose() {
  echo "--- FAILED; state for diagnosis:" >&2
  kubectl -n "$NS" get pvc,lease,reclaimspacejob,csiaddonsnode -o yaml >&2 || true
  kubectl -n "$NS" logs --all-containers --prefix -l 'app in (e2e-node,e2e-controller)' --tail=50 >&2 || true
  kubectl -n csi-addons-system logs deploy/csi-addons-controller-manager --tail=50 >&2 || true
}
cleanup() {
  rc=$?
  [ "$rc" -eq 0 ] || diagnose
  if [ "${KEEP:-}" = 1 ]; then
    echo "kept cluster $CLUSTER; KUBECONFIG=$KUBECONFIG"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -f "$KUBECONFIG"
  fi
}
trap cleanup EXIT

node() { docker exec "$NODE" sh -c "$1"; }

kind create cluster --name "$CLUSTER" --wait 120s
kind load docker-image "$IMAGE" --name "$CLUSTER"

kubectl apply --server-side -k "$HERE/csi-addons"
kubectl -n csi-addons-system rollout status deploy/csi-addons-controller-manager --timeout=180s

sed -e "s|__IMAGE__|$IMAGE|g" -e "s|__NODE__|$NODE|g" "$HERE/manifests.yaml" | kubectl apply -f -
kubectl patch volumeattachment csi-reclaimspace-e2e --subresource=status --type=merge -p '{"status":{"attached":true}}'
kubectl -n "$NS" rollout status ds/e2e-node --timeout=180s
kubectl -n "$NS" rollout status deploy/e2e-controller --timeout=180s

# The volume is mounted only now, with the pods running, so the sidecar sees
# it through HostToContainer propagation. Its root is owned by another user
# with mode 0700, as an app's volume often is, so opening it needs
# DAC_READ_SEARCH. 20M of data to keep, 200M written and deleted to free.
stage=/var/lib/kubelet/plugins/kubernetes.io/csi/$DRIVER/$(printf %s "$HANDLE" | sha256sum | cut -d' ' -f1)/globalmount
node "mkdir -p /var/e2e $stage && truncate -s 1G /var/e2e/vol.img && mkfs.ext4 -q /var/e2e/vol.img && mount -o loop /var/e2e/vol.img $stage"
node "dd if=/dev/urandom of=$stage/keep bs=1M count=20 status=none"
node "dd if=/dev/urandom of=$stage/junk bs=1M count=200 status=none && sync"
node "md5sum $stage/keep > /var/e2e/keep.md5 && rm $stage/junk && sync"
node "chown 999:999 $stage && chmod 0700 $stage"
before=$(node "stat -c %b /var/e2e/vol.img")

# Without the bound PVC or the leader's lease the controller fails a job at
# once; without the node's CSIAddonsNode it retries for minutes. Wait for all.
kubectl -n "$NS" wait pvc/e2e --for=jsonpath='{.status.phase}'=Bound --timeout=60s
for _ in $(seq 60); do
  [ "$(kubectl -n "$NS" get csiaddonsnode -o name | wc -l)" -eq 2 ] && break
  sleep 2
done
[ "$(kubectl -n "$NS" get csiaddonsnode -o name | wc -l)" -eq 2 ] || { echo "ERROR: want 2 CSIAddonsNodes" >&2; false; }
kubectl -n "$NS" wait csiaddonsnode --all --for=jsonpath='{.status.state}'=Connected --timeout=120s
leader=$(kubectl -n "$NS" get pod -l app=e2e-controller -o jsonpath='{.items[0].metadata.name}')
for _ in $(seq 60); do
  kubectl -n "$NS" get lease -o jsonpath='{.items[*].spec.holderIdentity}' | grep -qw "$leader" && break
  sleep 2
done
kubectl -n "$NS" get lease -o jsonpath='{.items[*].spec.holderIdentity}' | grep -qw "$leader" || { echo "ERROR: controller sidecar holds no lease" >&2; false; }

kubectl apply -f - <<EOF
apiVersion: csiaddons.openshift.io/v1alpha1
kind: ReclaimSpaceJob
metadata:
  name: e2e
  namespace: $NS
spec:
  target:
    persistentVolumeClaim: e2e
EOF
# Both outcomes set completionTime; waiting on it fails fast on a failed job.
kubectl -n "$NS" wait reclaimspacejob/e2e --for=jsonpath='{.status.completionTime}' --timeout=300s
result=$(kubectl -n "$NS" get reclaimspacejob/e2e -o jsonpath='{.status.result}: {.status.message}')
echo "ReclaimSpaceJob: $result"
[ "${result%%:*}" = Succeeded ] || { echo "ERROR: job did not succeed" >&2; false; }

node "sync"
after=$(node "stat -c %b /var/e2e/vol.img")
freed=$(( (before - after) * 512 / 1048576 ))
echo "backing file: $((before * 512 / 1048576)) MiB -> $((after * 512 / 1048576)) MiB allocated (${freed} MiB freed)"
[ "$freed" -ge 150 ] || { echo "ERROR: want at least 150 MiB freed" >&2; false; }
node "md5sum -c --quiet /var/e2e/keep.md5"
node "umount $stage && e2fsck -fn /var/e2e/vol.img >/dev/null"
echo "PASS: ReclaimSpaceJob succeeded end to end and FITRIM freed the deleted blocks"
