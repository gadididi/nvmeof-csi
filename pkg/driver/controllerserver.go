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
	"fmt"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog"

	csicommon "nvmeof-csi/pkg/csi-common"
	"nvmeof-csi/pkg/util"
	gatewaypb "nvmeof-csi/proto"
)

type controllerServer struct {
	csi.UnimplementedControllerServer
	defaultImpl   *csicommon.DefaultControllerServer
	gatewayClient gatewaypb.GatewayClient
	grpcConn      *grpc.ClientConn
	volumeLocks   *util.VolumeLocks
}

func (cs *controllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	volumeID := req.GetName()
	unlock := cs.volumeLocks.Lock(volumeID)
	defer unlock()

	csiVolume, err := cs.createVolume(req)
	if err != nil {
		klog.Errorf("failed to create volume, volumeID: %s err: %v", volumeID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Optional: populate volume context with parameters
	if csiVolume.VolumeContext == nil {
		csiVolume.VolumeContext = make(map[string]string)
	}
	for k, v := range req.GetParameters() {
		csiVolume.VolumeContext[k] = v
	}

	return &csi.CreateVolumeResponse{Volume: csiVolume}, nil
}

func (cs *controllerServer) createVolume(req *csi.CreateVolumeRequest) (*csi.Volume, error) {
	var (
		resp *gatewaypb.NsidStatus
		err  error
	)
	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 {
		klog.Warningln("invalid volume size, defaulting to 1GiB")
		size = 1 * 200 * 1024 * 1024 // 200MB
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build namespace_add_req
	nsReq := &gatewaypb.NamespaceAddReq{
		RbdPoolName:       "mypool", // adjust as needed
		RbdImageName:      req.GetName(),
		SubsystemNqn:      "nqn.2016-06.io.spdk:cnode1.mygroup1", // adjust if needed
		BlockSize:         4096,
		CreateImage:       proto.Bool(true),
		Size:              proto.Uint64(uint64(size)),
		NoAutoVisible:     proto.Bool(false),
		DisableAutoResize: proto.Bool(false),
	}

	// Call Gateway
	resp, err = cs.gatewayClient.NamespaceAdd(ctx, nsReq)
	if err != nil {
		return nil, fmt.Errorf("gateway NamespaceAdd failed: %w", err)
	}
	if resp.GetStatus() != 0 {
		return nil, fmt.Errorf("gateway NamespaceAdd returned error: %s", resp.GetErrorMessage())
	}

	// Construct and return CSI volume
	vol := &csi.Volume{
		VolumeId:      "nqn.2016-06.io.spdk:cnode1.mygroup1", //TODO - change it maybe to resp.GetNsid() or similar ??
		CapacityBytes: size,
		//VolumeContext: req.GetParameters(),
		VolumeContext: map[string]string{
			"nqn":       nsReq.SubsystemNqn,
			"traddr":    "10.242.64.32", // Replace with real target IP or use resp.GetTraddr()
			"trsvcid":   "4420",         // Standard NVMe-oF port, or use from response
			"transport": "tcp",          // Or "rdma" if you're using it
			"image":     req.GetName(),  // add this!
		},
		ContentSource: req.GetVolumeContentSource(),
	}
	return vol, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// make sure we support all requested caps
	for _, cap := range req.VolumeCapabilities {
		supported := false
		for _, accessMode := range cs.defaultImpl.Driver.GetVolumeCapabilityAccessModes() {
			if cap.GetAccessMode().GetMode() == accessMode.GetMode() {
				supported = true
				break
			}
		}
		if !supported {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	// Example: Call Gateway list_namespaces, find the one matching the VolumeId
	// and send the UUID to the target node (e.g. via Node info in req)

	klog.Infof("Publishing volume %s to node %s", req.VolumeId, req.NodeId)

	nsListReq := &gatewaypb.ListNamespacesReq{
		Subsystem: req.VolumeId, // same as used in VolumeId
	}

	nsListResp, err := cs.gatewayClient.ListNamespaces(ctx, nsListReq)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}

	var targetUUID string
	for _, ns := range nsListResp.GetNamespaces() {
		// print the ns
		klog.Infof("Found namespace: %s, UUID: %s, Image: %s", ns.GetNsSubsystemNqn(), ns.GetUuid(), ns.GetRbdImageName())
		if ns.GetRbdImageName() == req.VolumeContext["image"] {
			targetUUID = ns.GetUuid()
			break
		}
	}
	if targetUUID == "" {
		return nil, fmt.Errorf("UUID not found for volume %s", req.VolumeId)
	}

	// You could now "notify" the node, or embed the UUID in context for NodePublishVolume
	publishContext := map[string]string{
		"uuid":      targetUUID,
		"nqn":       req.VolumeId,
		"traddr":    req.VolumeContext["traddr"],
		"trsvcid":   req.VolumeContext["trsvcid"],
		"transport": req.VolumeContext["transport"],
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: publishContext,
	}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	// TODO: Add logic to ControllerUnpublishVolume the volume from your backend (e.g., via gRPC to Gateway)

	// Log success
	klog.Infof("ControllerUnpublishVolume: %s", volumeID)

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	// TODO: Add logic to delete the volume from your backend (e.g., via gRPC to Gateway)

	// Log success
	klog.Infof("Deleted volume: %s", volumeID)

	return &csi.DeleteVolumeResponse{}, nil
}

func newControllerServer(d *csicommon.CSIDriver) (*controllerServer, error) {
	// Connect to Gateway gRPC server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "10.242.64.32:5500", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Gateway gRPC server: %w", err)
	}

	server := &controllerServer{
		defaultImpl:   csicommon.NewDefaultControllerServer(d),
		grpcConn:      conn,
		gatewayClient: gatewaypb.NewGatewayClient(conn),
		volumeLocks:   util.NewVolumeLocks(),
	}

	return server, nil
}

func (cs *controllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).Info("Forwarding ControllerGetCapabilities to defaultImpl")
	return cs.defaultImpl.ControllerGetCapabilities(ctx, req)
}
