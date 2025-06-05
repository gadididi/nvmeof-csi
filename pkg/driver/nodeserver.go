/*
Copyright (c) Arm Limited and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	osExec "os/exec"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	"k8s.io/utils/exec"
	"k8s.io/utils/mount"

	csicommon "nvmeof-csi/pkg/csi-common"
	"nvmeof-csi/pkg/util"
)

type nodeServer struct {
	csi.UnimplementedNodeServer
	defaultImpl   *csicommon.DefaultNodeServer
	mounter       mount.Interface
	volumeLocks   *util.VolumeLocks
	xpuConnClient *grpc.ClientConn
	xpuConfigInfo *util.XpuConfig
}

func newNodeServer(d *csicommon.CSIDriver) (*nodeServer, error) {
	ns := &nodeServer{
		defaultImpl: csicommon.NewDefaultNodeServer(d),
		mounter:     mount.New(""),
		volumeLocks: util.NewVolumeLocks(),
	}

	return ns, nil
}

func (ns *nodeServer) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath() // use this directory to persistently store VolumeContext
	stagingTargetPath := getStagingTargetPath(req)

	isStaged, err := ns.isStaged(stagingTargetPath)
	if err != nil {
		klog.Errorf("failed to check isStaged, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if isStaged {
		klog.Warning("volume already staged")
		return &csi.NodeStageVolumeResponse{}, nil
	}

	var initiator util.SpdkCsiInitiator
	if ns.xpuConnClient != nil {
		vc := req.GetVolumeContext()
		vc["stagingParentPath"] = stagingParentPath
		initiator, err = util.NewSpdkCsiXpuInitiator(vc, ns.xpuConnClient, ns.xpuConfigInfo)
	} else {
		initiator, err = util.NewSpdkCsiInitiator(req.GetVolumeContext())
	}
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	devicePath, err := initiator.Connect() // idempotent
	if err != nil {
		klog.Errorf("failed to connect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err != nil {
			initiator.Disconnect() //nolint:errcheck // ignore error
		}
	}()
	if err = ns.stageVolume(devicePath, stagingTargetPath, req); err != nil { // idempotent
		klog.Errorf("failed to stage volume, volumeID: %s devicePath:%s err: %v", volumeID, devicePath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	// stash VolumeContext to stagingParentPath (useful during Unstage as it has no
	// VolumeContext passed to the RPC as per the CSI spec)
	err = util.StashVolumeContext(req.GetVolumeContext(), stagingParentPath)
	if err != nil {
		klog.Errorf("failed to stash volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	stagingParentPath := req.GetStagingTargetPath()
	stagingTargetPath := getStagingTargetPath(req)

	isStaged, err := ns.isStaged(stagingTargetPath)
	if err != nil {
		klog.Errorf("failed to check isStaged, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !isStaged {
		klog.Warning("volume already unstaged")
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	err = ns.deleteMountPoint(stagingTargetPath) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", stagingTargetPath, err)
		return nil, status.Errorf(codes.Internal, "unstage volume %s failed: %s", volumeID, err)
	}

	volumeContext, err := util.LookupVolumeContext(stagingParentPath)
	if err != nil {
		klog.Errorf("failed to lookup volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	var initiator util.SpdkCsiInitiator
	if ns.xpuConnClient != nil {
		vc := volumeContext
		vc["stagingParentPath"] = stagingParentPath
		initiator, err = util.NewSpdkCsiXpuInitiator(vc, ns.xpuConnClient, ns.xpuConfigInfo)
	} else {
		initiator, err = util.NewSpdkCsiInitiator(volumeContext)
	}
	if err != nil {
		klog.Errorf("failed to create spdk initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	err = initiator.Disconnect() // idempotent
	if err != nil {
		klog.Errorf("failed to disconnect initiator, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := util.CleanUpVolumeContext(stagingParentPath); err != nil {
		klog.Errorf("failed to clean up volume context, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func findDeviceByNQN(nqn string) (string, error) {
	entries, err := filepath.Glob("/dev/disk/by-id/nvme-*")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		resolved, err := filepath.EvalSymlinks(entry)
		if err == nil && strings.Contains(resolved, nqn) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("no device found for NQN %s", nqn)
}

func createBlockFileIfNotExists(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return fmt.Errorf("failed to create block file: %v", err)
		}
		defer f.Close()
	}
	return nil
}

func (ns *nodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	// Lock per volume
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	// Extract NVMe-oF connection info
	nqn := req.GetVolumeContext()["nqn"]
	traddr := req.GetVolumeContext()["traddr"]
	trsvcid := req.GetVolumeContext()["trsvcid"]
	transport := req.GetVolumeContext()["transport"]

	if nqn == "" || traddr == "" || trsvcid == "" || transport == "" {
		return nil, status.Errorf(codes.InvalidArgument, "missing NVMe connection parameters")
	}

	klog.Infof("Connecting volume %s via NVMe: nqn=%s, traddr=%s, trsvcid=%s, transport=%s",
		volumeID, nqn, traddr, trsvcid, transport)

	// 1. Run `nvme connect`
	cmd := osExec.Command("nvme", "connect",
		"-t", transport,
		"-n", nqn,
		"-a", traddr,
		"-s", trsvcid)
	klog.Infof("Connecting NVMe volume with command: %v", cmd.Args)
	if output, err := cmd.CombinedOutput(); err != nil {
		klog.Errorf("failed to connect volume %s: %v, output: %s", volumeID, err, string(output))
		return nil, status.Errorf(codes.Internal, "nvme connect failed: %v", err)
	}

	// 2. Discover device path
	devicePath, err := findDeviceByNQN(nqn)
	if err != nil {
		return nil, fmt.Errorf("failed to find NVMe device for NQN %s: %v", nqn, err)
	}

	// 3. If block mode, create a bind mount (raw block usage)
	if req.GetVolumeCapability().GetBlock() != nil {

		// Ensure parent directory of targetPath exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0750); err != nil {
			return nil, fmt.Errorf("failed to create target path parent dir: %v", err)
		}

		// Ensure the target file exists
		if err := createBlockFileIfNotExists(targetPath); err != nil {
			return nil, err
		}
		// Bind-mount the block device to the target path
		if err := ns.mounter.Mount(devicePath, targetPath, "", []string{"bind"}); err != nil {
			return nil, status.Errorf(codes.Internal, "bind mount failed: %v", err)
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// 4. If mount mode: just expose the device, no mount
	klog.Infof("Skipping mount for volume %s: device %s available", volumeID, devicePath)
	return &csi.NodePublishVolumeResponse{}, nil

	// klog.Infof("Successfully connected volume %s", volumeID)
	// return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	unlock := ns.volumeLocks.Lock(volumeID)
	defer unlock()

	err := ns.deleteMountPoint(req.GetTargetPath()) // idempotent
	if err != nil {
		klog.Errorf("failed to delete mount point, targetPath: %s err: %v", req.GetTargetPath(), err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

// must be idempotent
//
//nolint:cyclop // many cases in switch increases complexity
func (ns *nodeServer) stageVolume(devicePath, stagingPath string, req *csi.NodeStageVolumeRequest) error {
	mounted, err := ns.createMountPoint(stagingPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	fsType := req.GetVolumeCapability().GetMount().GetFsType()
	mntFlags := req.GetVolumeCapability().GetMount().GetMountFlags()

	switch req.VolumeCapability.AccessMode.Mode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		mntFlags = append(mntFlags, "ro")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return errors.New("unsupported MULTI_NODE_MULTI_WRITER AccessMode")
	case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER:
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
	case csi.VolumeCapability_AccessMode_UNKNOWN:
	}

	klog.Infof("mount %s to %s, fstype: %s, flags: %v", devicePath, stagingPath, fsType, mntFlags)
	mounter := mount.SafeFormatAndMount{Interface: ns.mounter, Exec: exec.New()}
	err = mounter.FormatAndMount(devicePath, stagingPath, fsType, mntFlags)
	if err != nil {
		return err
	}
	return nil
}

// isStaged if stagingPath is a mount point, it means it is already staged, and vice versa
func (ns *nodeServer) isStaged(stagingPath string) (bool, error) {
	unmounted, err := mount.IsNotMountPoint(ns.mounter, stagingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		klog.Warningf("check is stage error: %v", err)
		return true, err
	}
	return !unmounted, nil
}

// must be idempotent
// func (ns *nodeServer) publishVolume(stagingPath string, req *csi.NodePublishVolumeRequest) error {

// }

// create mount point if not exists, return whether already mounted
func (ns *nodeServer) createMountPoint(path string) (bool, error) {
	unmounted, err := mount.IsNotMountPoint(ns.mounter, path)
	if os.IsNotExist(err) {
		unmounted = true
		err = os.MkdirAll(path, 0o755)
	}
	if !unmounted {
		klog.Infof("%s already mounted", path)
	}
	return !unmounted, err
}

// unmount and delete mount point, must be idempotent
func (ns *nodeServer) deleteMountPoint(path string) error {
	unmounted, err := mount.IsNotMountPoint(ns.mounter, path)
	if os.IsNotExist(err) {
		klog.Infof("%s already deleted", path)
		return nil
	}
	if err != nil {
		return err
	}
	if !unmounted {
		err = ns.mounter.Unmount(path)
		if err != nil {
			return err
		}
	}
	return os.RemoveAll(path)
}

func getStagingTargetPath(req interface{}) string {
	switch vr := req.(type) {
	case *csi.NodeStageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodeUnstageVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	case *csi.NodePublishVolumeRequest:
		return vr.GetStagingTargetPath() + "/" + vr.GetVolumeId()
	}
	return ""
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return ns.defaultImpl.NodeGetInfo(ctx, req)
}
