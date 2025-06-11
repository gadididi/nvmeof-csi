# NVMeoF CSI

## About

This repo contains a CSI ([Container Storage Interface](https://github.com/container-storage-interface/)) plugin for Kubernetes, designed to provision and manage NVMe over Fabrics (NVMeoF) volumes.

It dynamically provisions NVMe namespaces on the storage backend and exposes them to Kubernetes workloads using NVMeoF.

Please see [NvmeOF](https://github.com/ceph/ceph-nvmeof)
for detailed introduction.

## Supported platforms

This plugin conforms to [CSI Spec v1.11.0](https://github.com/container-storage-interface/spec/blob/v1.11.0/spec.md).
It is currently developed and tested only on Kubernetes.

This plugin supports `x86_64` and `Arm64` architectures.

## Project status

Status: **Beta**

## Prerequisites

NVMeoF-CSI is currently developed and tested with `Go 1.24`, `Docker 28.2` and `Kubernetes 1.25.0` on `CentOS Stream 9`.

Before deploying the NVMeoF CSI driver, ensure that your storage backend has an NVMe over Fabrics subsystem installed and reachable from your Kubernetes nodes. Follow this guide to install and configure NVMeoF:

* [NVMe-oF Setup Guide (nvme-cli)](https://linux-nvme.github.io/nvme-cli/nvme-cli.html#nvmeof)

## Setup

### Build

Compile the Go-based CSI driver binary and build/push its container image:

```bash
# Build the CSI driver binary
$ make all

# Build and push the Docker image to your registry
$ docker build -t quay.io/your-org/nvmeof-csi:latest .
$ docker push quay.io/your-org/nvmeof-csi:latest
```

## Usage

Example deployment files can be found in deploy/kubernetes directory.

| File                   | Purpose                                   |
| ---------------------- | ----------------------------------------- |
| `storageclass.yaml`    | StorageClass for the NVMeoF CSI driver    |
| `controller.yaml`      | StatefulSet for CSI Controller service    |
| `node.yaml`            | DaemonSet for CSI Node service            |
| `controller-rbac.yaml` | RBAC for CSI Controller                   |
| `node-rbac.yaml`       | RBAC for CSI Node                         |
| `config-map.yaml`      | NVMeoF backend configuration              |
| `secret.yaml`          | Access credentials                        |
| `driver.yaml`          | CSIDriver object                          |
| `testpod.yaml`         | Example Pod + PVC to verify functionality |

---
### Deploy

1. **Start your Kubernetes cluster** (e.g., via Minikube):

   ```bash
   cd scripts
   sudo ./minikube.sh up
   sudo ln -sf /var/lib/minikube/binaries/v1.25.0/kubectl /usr/local/bin/kubectl
   ```

2. **Deploy CSI services**:

   ```bash
   cd deploy/kubernetes
   ./deploy.sh
   kubectl get pods
   ```

3. **Run a test workload**:

   ```bash
   kubectl apply -f testpod.yaml

   # Verify PersistentVolumes and Claims
   kubectl get pv
   kubectl get pvc

   # Verify the test pod
   kubectl get pods

   # Check the NVMe-oF volume mount inside the pod
   kubectl exec spdkcsi-test -- mount | grep nvme
   ```

## Teardown

Clean up the test resources and cluster:

```bash
# Delete test workload
kubectl delete -f deploy/kubernetes/testpod.yaml

# Tear down CSI services
cd deploy/kubernetes
./deploy.sh teardown

# Clean up Kubernetes cluster
cd scripts
sudo ./minikube.sh clean
```

---

## Architecture & Flow

Below are two detailed ASCII diagrams showing:

1. **Controller Side** – how `kube-controller-manager` calls your CSI Controller, which in turn talks to the NVMe-oF Gateway.
2. **Node Side** – how `kubelet` interacts with your CSI Node to perform attach/mount operations.

### 1. Controller & NVMe-oF Gateway

```
+--------------------------------------------------+
| Kubernetes Control Plane                         |
|  +--------------------------------------------+  |
|  | CSI Controller Deployment (nvmeofcsi)     |  |
|  |  • CreateVolume()                         |  |
|  |  • DeleteVolume()                         |  |
|  |  • ControllerPublishVolume()              |  |
|  +--------------------------------------------+  |
+--------------------------------------------------+
                   │
         CSI gRPC  │                         NVMe-oF Gateway SDK/gRPC
  (drivername=csi.nvmeof.io)           (e.g. management IP:5500)
                   ▼
+--------------------------------------------------+
| NVMe-oF Gateway gRPC Server                      |
|  • NamespaceAdd(volumeName, size, parameters)    |
|  • NamespaceDel(namespaceID)                     |
|  • ListSubsystems(), ListNamespaces()            |
+--------------------------------------------------+
```

**Flow**:

1. **CreateVolume** → CSI Controller
2. Controller issues **NamespaceAdd** → NVMe-oF Gateway
3. Gateway provisions an NVMe namespace and returns NQN, TRADDR, TRSVCID, transport.
4. CSI Controller records these in `VolumeContext`.
5. On `ControllerPublishVolume`, Controller may export additional attachment details.

---

### 2. Node DaemonSet & Kubelet

```
+-----------------------------+        Unix Socket                        +-----------------------------+
| kubelet (Pod volume plugin) | <--------------------------------------> | CSI Node Server (_out/nvmeofcsi) |
+-----------------------------+    /var/lib/kubelet/plugins/ \            +-----------------------------+
                                            csi.nvmeof.io/     
    ▲                    │
    │ NodeGetInfo        │
    │ NodeStageVolume    │
    │  • run `nvme discover` + `nvme connect` using VolumeContext  
    │ NodePublishVolume  │
    │  • bind-mount device into Pod’s filesystem
    │ NodeUnstageVolume  │
    │ NodeUnpublishVolume│
    │                    ▼
+------------------------------------------------------------------+
|    NVMe-oF Target(s)                                             |
|    • Subsystems, Namespaces exported over TCP transports         |
+------------------------------------------------------------------+
```

**Flow**:

1. kubelet invokes **NodeStageVolume** → CSI Node
2. Node runs `nvme discover`, `nvme connect` with
   – **nqn** (subsystem name)
   – **traddr** (target address)
   – **trsvcid** (port)
   – **transport**, etc.
3. On success, device node (e.g. `/dev/nvmeXnY`) appears on host.
4. **NodePublishVolume** → bind-mounts that block device into the Pod’s mount path.
5. Tear-down via **NodeUnpublishVolume** / **NodeUnstageVolume**.

---

## Communication and Contribution

Please join [SPDK community](https://spdk.io/community/) for communication and contribution.

Project progress is tracked in [Trello board](https://trello.com/b/nBujJzya/kubernetes-integration).
