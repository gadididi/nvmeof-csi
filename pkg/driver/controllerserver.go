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

func (cs *controllerServer) createVolume(req *csi.CreateVolumeRequest) (*csi.Volume, error) {
	size := req.GetCapacityRange().GetRequiredBytes()
	if size == 0 {
		klog.Warningln("invalid volume size, defaulting to 1GiB")
		size = 1 * 1024 * 1024 * 1024 // 1GiB
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build namespace_add_req
	nsReq := &gatewaypb.NamespaceAddReq{
		RbdPoolName:       "mypool", // adjust as needed
		RbdImageName:      req.GetName(),
		SubsystemNqn:      "nqn.2023-01.io.spdk:csi", // adjust if needed
		BlockSize:         4096,
		CreateImage:       proto.Bool(true),
		Size:              proto.Uint64(uint64(size)),
		NoAutoVisible:     proto.Bool(false),
		DisableAutoResize: proto.Bool(false),
	}

	// Call Gateway
	resp, err := cs.gatewayClient.NamespaceAdd(ctx, nsReq)
	if err != nil {
		return nil, fmt.Errorf("gateway NamespaceAdd failed: %w", err)
	}
	if resp.GetStatus() != 0 {
		return nil, fmt.Errorf("gateway NamespaceAdd returned error: %s", resp.GetErrorMessage())
	}

	// Construct and return CSI volume
	vol := &csi.Volume{
		VolumeId:      fmt.Sprintf("nqn.2023-01.io.spdk:csi:%d", resp.GetNsid()),
		CapacityBytes: size,
		VolumeContext: req.GetParameters(),
		ContentSource: req.GetVolumeContentSource(),
	}
	return vol, nil
}

func newControllerServer(d *csicommon.CSIDriver) (*controllerServer, error) {
	// Connect to Gateway gRPC server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
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
