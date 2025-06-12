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
	"encoding/base64"
	"encoding/json"
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

// VolumeIdentifier represents the structured data encoded in VolumeID
type VolumeIdentifier struct {
	NSID       uint32 `json:"nsid"`
	NQN        string `json:"nqn"`
	VolumeName string `json:"volume_name"` // Original PVC name for consistent locking
}

// Helper function to encode VolumeIdentifier into VolumeID
func encodeVolumeID(identifier VolumeIdentifier) (string, error) {
	jsonData, err := json.Marshal(identifier)
	if err != nil {
		return "", fmt.Errorf("failed to marshal volume identifier: %w", err)
	}
	return base64.StdEncoding.EncodeToString(jsonData), nil
}

// Helper function to decode VolumeID into VolumeIdentifier
func decodeVolumeID(volumeID string) (*VolumeIdentifier, error) {
	jsonData, err := base64.StdEncoding.DecodeString(volumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to decode volume ID: %w", err)
	}

	var identifier VolumeIdentifier
	err = json.Unmarshal(jsonData, &identifier)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal volume identifier: %w", err)
	}

	return &identifier, nil
}

func (cs *controllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	volumeName := req.GetName()
	unlock := cs.volumeLocks.Lock(volumeName)
	defer unlock()

	csiVolume, err := cs.createVolume(req)
	if err != nil {
		klog.Errorf("failed to create volume, volumeID: %s err: %v", volumeName, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Optional: populate volume context with parameters
	if csiVolume.VolumeContext == nil {
		csiVolume.VolumeContext = make(map[string]string)
	}
	for k, v := range req.GetParameters() {
		csiVolume.VolumeContext[k] = v
	}
	klog.Infof("Volume created successfully: %s with VolumeID: %s", volumeName, csiVolume.VolumeId)
	return &csi.CreateVolumeResponse{Volume: csiVolume}, nil
}

func (cs *controllerServer) createVolume(req *csi.CreateVolumeRequest) (*csi.Volume, error) {
	var (
		resp *gatewaypb.NsidStatus
		err  error
	)
	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 { // default size
		klog.Warningln("invalid volume size, defaulting to 1GiB")
		size = 1 * 200 * 1024 * 1024 // 200MB
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build namespace_add_req
	nsReq := &gatewaypb.NamespaceAddReq{
		RbdPoolName:       req.GetParameters()["RbdPoolName"],
		RbdImageName:      req.GetName(),
		SubsystemNqn:      req.GetParameters()["SubsystemNqn"],
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

	// Create structured volume identifier
	volumeIdentifier := VolumeIdentifier{
		NSID:       resp.GetNsid(),
		NQN:        nsReq.SubsystemNqn,
		VolumeName: req.GetName(), // Store original volume name for locking
	}

	// Encode to create VolumeID
	volumeID, err := encodeVolumeID(volumeIdentifier)
	if err != nil {
		return nil, fmt.Errorf("failed to encode volume ID: %w", err)
	}

	// Construct and return CSI volume
	vol := &csi.Volume{
		VolumeId:      volumeID, // contains NSID, NQN, and volume name
		CapacityBytes: size,
		VolumeContext: map[string]string{
			"nqn":       nsReq.SubsystemNqn,
			"traddr":    req.GetParameters()["traddr"],
			"trsvcid":   req.GetParameters()["trsvcid"],
			"transport": req.GetParameters()["transport"],
			"image":     nsReq.RbdImageName,
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
	klog.Infof("Publishing volume %s to node %s", req.VolumeId, req.NodeId)
	nqn := req.VolumeContext["nqn"]
	nsListReq := &gatewaypb.ListNamespacesReq{ //TODO - maybe i can create by Nsid and not Nqn
		Subsystem: nqn,
	}

	nsListResp, err := cs.gatewayClient.ListNamespaces(ctx, nsListReq)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}

	var targetUUID string
	imageName := req.VolumeContext["image"]
	for _, ns := range nsListResp.GetNamespaces() {
		// print the ns
		klog.Infof("Found namespace: %s, UUID: %s, Image: %s", ns.GetNsSubsystemNqn(), ns.GetUuid(), ns.GetRbdImageName())
		if ns.GetRbdImageName() == imageName {
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
		"nqn":       nqn,
		"traddr":    req.VolumeContext["traddr"],
		"trsvcid":   req.VolumeContext["trsvcid"],
		"transport": req.VolumeContext["transport"],
	}

	klog.Infof("Volume published successfully: %s with UUID: %s", req.VolumeId, targetUUID)
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: publishContext,
	}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.Infof("Unpublishing volume %s from node %s", req.VolumeId, req.NodeId)

	// For NVMe-oF, unpublishing is typically handled at the node level
	// Controller just acknowledges the request
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	identifier, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to decode volume ID: %v", err)

	}
	unlock := cs.volumeLocks.Lock(identifier.VolumeName)
	defer unlock()

	klog.Infof("Deleting volume: %s (NSID: %d, NQN: %s)", identifier.VolumeName, identifier.NSID, identifier.NQN)

	nsDelReq := &gatewaypb.NamespaceDeleteReq{
		Nsid:         identifier.NSID,
		SubsystemNqn: identifier.NQN,
	}
	gwCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cs.gatewayClient.NamespaceDelete(gwCtx, nsDelReq)
	if err != nil {
		klog.Errorf("gateway NamespaceDelete failed for volume %s: %v", identifier.VolumeName, err)
		return nil, status.Errorf(codes.Internal, "gateway NamespaceRemove failed: %v", err)
	}
	if resp.GetStatus() != 0 {
		klog.Errorf("gateway returned error for volume %s: %s", identifier.VolumeName, resp.GetErrorMessage())
		return nil, status.Errorf(codes.Internal, "gateway returned error: %s", resp.GetErrorMessage())
	}

	klog.Infof("Volume deleted successfully: %s", identifier.VolumeName)
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
