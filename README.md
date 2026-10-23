# csi-ReclaimSpace

A sidecar that lets [CSI-Addons](https://github.com/csi-addons/kubernetes-csi-addons)
`ReclaimSpaceJob`s trim the filesystem volumes of any CSI driver that attaches
and stages them.

## Why it exists

Thin-provisioned storage only gets blocks back when the filesystem on the
volume discards them. Most CSI drivers do not mount with `discard`, and the
node's `fstrim.timer` only covers `/etc/fstab`, so deleted data stays allocated
on the array. CSI-Addons schedules and reports a reclaim per PVC, but it needs
the driver to serve the CSI-Addons API — and most drivers don't.
`csi-reclaimspace` serves that API in pods of its own beside the driver, so the
driver's image and manifests stay untouched.

## How it works

```
CSI-Addons controller ──TLS──▶ k8s-sidecar ──unix socket──▶ csi-reclaimspace ──FITRIM──▶ volume filesystem
   (ReclaimSpaceJob)          (upstream)                   (this image)
```

- `MODE=node`, in a DaemonSet of its own on the driver's nodes: advertises
  ReclaimSpace ONLINE and runs `FITRIM` on the volume's staging path. It
  refuses any path that is not a mount root, so an unmounted volume never trims
  the node's own filesystem, and it runs one trim per volume at a time. A block
  volume fails its job: it has no filesystem to trim, and answering
  `Unimplemented` would make the CSI-Addons controller report a success.
- `MODE=controller`, in a one-pod Deployment of its own: advertises
  ReclaimSpace OFFLINE. After the node-side trim the CSI-Addons controller
  calls the driver's controller side through a leader, and fails the job if no
  leader advertises OFFLINE. It never trims.

The CSI-Addons controller finds the volume's node from the PV's attached
VolumeAttachment, and the `k8s-sidecar` sends its staging path
(`<stagingpath>/<driver>/<sha256(volumeHandle)>/globalmount`). So the driver
must use VolumeAttachments (`attachRequired: true`) and stage its volumes
(`NodeStageVolume`), as most block-storage drivers do. Without a staging step
nothing is mounted at that path and the trim is refused. Without a
VolumeAttachment, as for a PVC no pod uses, the controller skips the node and
the job reports success without trimming.

## Deploy

Prerequisites: Linux 5.8+ nodes (the mount-root check uses `statx`'s
`STATX_ATTR_MOUNT_ROOT`) and the CSI-Addons controller and CRDs
([kubernetes-csi-addons](https://github.com/csi-addons/kubernetes-csi-addons/tree/main/deploy/controller)).

Run two containers as a DaemonSet of their own, with the driver node plugin's
tolerations: the upstream `k8s-sidecar` and `csi-reclaimspace`. They share a
socket, and `csi-reclaimspace` must see the kubelet's staging mounts at the
same path as the host. Neither calls the driver, so they need no place in its
pods. Example pod spec with the OpenStack Cinder driver:

```yaml
containers:
- name: csi-addons
  image: quay.io/csiaddons/k8s-sidecar:v0.15.1
  args:
    - --node-id=$(NODE_NAME)
    - --stagingpath=/var/lib/kubelet/plugins/kubernetes.io/csi/
    - --csi-addons-address=/csi/csi-addons.sock
    - --controller-port=9070
    - --pod=$(POD_NAME)
    - --namespace=$(POD_NAMESPACE)
    - --pod-uid=$(POD_UID)
  env:
    - name: NODE_NAME
      valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
    - name: POD_NAME
      valueFrom: {fieldRef: {fieldPath: metadata.name}}
    - name: POD_NAMESPACE
      valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
    - name: POD_UID
      valueFrom: {fieldRef: {fieldPath: metadata.uid}}
  ports:
    - name: csi-addons
      containerPort: 9070
  volumeMounts:
    - name: socket-dir
      mountPath: /csi
- name: csi-reclaimspace
  image: ghcr.io/singular-group/csi-reclaimspace:<version>
  env:
    - name: DRIVER_NAME
      value: cinder.csi.openstack.org
    - name: CSI_ADDONS_SOCKET
      value: /csi/csi-addons.sock
    - name: MODE
      value: node
  securityContext:
    # FITRIM needs SYS_ADMIN; DAC_READ_SEARCH opens volume roots that an app
    # chowned to itself with mode 0700.
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: ["ALL"]
      add: ["SYS_ADMIN", "DAC_READ_SEARCH"]
  volumeMounts:
    - name: socket-dir
      mountPath: /csi
    - name: staging
      mountPath: /var/lib/kubelet/plugins/kubernetes.io/csi
      mountPropagation: HostToContainer
      readOnly: true
volumes:
- name: socket-dir
  emptyDir: {}
- name: staging
  hostPath:
    path: /var/lib/kubelet/plugins/kubernetes.io/csi
    type: DirectoryOrCreate
```

Run the same pair as a Deployment of its own with one replica and the
`Recreate` strategy: `MODE=controller` for `csi-reclaimspace`, no
`--stagingpath` for `k8s-sidecar`, and neither the staging mount nor any added
capability. Keep it out of a multi-replica controller plugin: the `k8s-sidecar`
names its `CSIAddonsNode` after the node and the Deployment, so two pods on one
node share it, and the CSI-Addons controller then fails to find the leader's
connection.

`DRIVER_NAME` must be the CSI driver's name (`kubectl get csidrivers`): the
`k8s-sidecar` registers the `CSIAddonsNode` under it. Each `k8s-sidecar` needs
this RBAC, bound to the service accounts of both workloads: CSIAddonsNode
get/create/update/watch, Pod and ReplicaSet get, PersistentVolume get,
TokenReview create, Secret get for volumes with a `nodeStageSecretRef`, and,
for the controller side, which elects a leader, Leases create (RBAC cannot
limit it by name) and get/update on its own lease (the driver name with every
character outside `[a-zA-Z0-9-]` replaced by `-`, plus `-csi-addons`), and
Events create/patch in its namespace. `csi-reclaimspace` calls no Kubernetes
API.

| Variable | Meaning |
|---|---|
| `DRIVER_NAME` | CSI driver name returned to CSI-Addons |
| `CSI_ADDONS_SOCKET` | Absolute path of the unix socket to serve |
| `MODE` | `node` or `controller` |

A missing or invalid variable makes the container exit with the reason.

## Usage

One-off reclaim of a PVC:

```yaml
apiVersion: csiaddons.openshift.io/v1alpha1
kind: ReclaimSpaceJob
metadata:
  name: reclaim-data
spec:
  target:
    persistentVolumeClaim: data
```

A large filesystem can trim for longer than the controller's default request
timeout (3 minutes). Raise it with `spec.timeout` (seconds, at least 60) on a
ReclaimSpaceJob; scheduled jobs use `reclaim-space-timeout` in the
controller's `csi-addons-config` ConfigMap. A retry while a trim is still
running gets `Aborted`, and the controller retries it later.

Weekly reclaim of every PVC of a StorageClass:

```yaml
metadata:
  annotations:
    reclaimspace.csiaddons.openshift.io/schedule: "@weekly"
```

The controller copies the schedule onto each PVC, so removing the annotation
does not stop it; set `reclaimspace.csiaddons.openshift.io/enable: "false"`
on the StorageClass to delete the scheduled jobs.

## Versions

Images are `ghcr.io/singular-group/csi-reclaimspace:<x.y.z.N>`: `x.y.z` is the
latest release tag and `N` the number of commits from it up to the last one
that changed the image. Each version is built once and never overwritten; pin
it. `:latest` follows `main`.

## Status

Tested in CI on every change: against a real kernel (loop-device ext4, FITRIM
through the gRPC API, with only the documented capabilities), and end to end
against the CSI-Addons controller in a kind cluster, where a `ReclaimSpaceJob`
must succeed and free the deleted blocks and a block volume's job must fail
(`test/e2e/run.sh`). Built against the kubernetes-csi-addons v0.15.1 API.
