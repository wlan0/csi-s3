/*
Copyright 2017 The Kubernetes Authors.

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
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/ctrox/csi-s3/pkg/mounter"
	"github.com/ctrox/csi-s3/pkg/s3"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type controllerServer struct {
	*csicommon.DefaultControllerServer
}

const (
	defaultFsPath = "csi-fs"
)

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	params := req.GetParameters()

	volumeID := sanitizeVolumeID(req.GetName())
	bucketName := volumeID
	prefix := ""

	// check if bucket name is overridden
	if nameOverride, ok := params[mounter.BucketKey]; ok {
		bucketName = nameOverride
		prefix = volumeID
		volumeID = path.Join(bucketName, prefix)
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	capacityBytes := int64(req.GetCapacityRange().GetRequiredBytes())

	mounter := params[mounter.TypeKey]

	glog.V(4).Infof("Got a request to create volume %s", volumeID)
	client, err := s3.NewClientFromSecret(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := client.BucketExists(bucketName)
	if err != nil {
		return nil, fmt.Errorf("failed to check if bucket %s exists: %v", volumeID, err)
	}
	var meta *s3.FSMeta
	if exists {
		meta, err = client.GetFSMeta(bucketName, prefix)

		if err != nil {
			glog.Warningf("Bucket %s exists, but failed to get its metadata: %v", volumeID, err)
			meta = &s3.FSMeta{
				BucketName:    bucketName,
				Prefix:        prefix,
				Mounter:       mounter,
				CapacityBytes: capacityBytes,
				FSPath:        defaultFsPath,
				CreatedByCsi:  false,
			}
		} else {
			// Check if volume capacity requested is bigger than the already existing capacity
			if capacityBytes > meta.CapacityBytes {
				return nil, status.Error(
					codes.AlreadyExists, fmt.Sprintf("Volume with the same name: %s but with smaller size already exist", volumeID),
				)
			}
			meta.Mounter = mounter
		}
	} else {
		if err = client.CreateBucket(bucketName); err != nil {
			return nil, fmt.Errorf("failed to create bucket %s: %v", bucketName, err)
		}
		if err = client.CreatePrefix(bucketName, path.Join(prefix, defaultFsPath)); err != nil {
			return nil, fmt.Errorf("failed to create prefix %s: %v", path.Join(prefix, defaultFsPath), err)
		}
		meta = &s3.FSMeta{
			BucketName:    bucketName,
			Prefix:        prefix,
			Mounter:       mounter,
			CapacityBytes: capacityBytes,
			FSPath:        defaultFsPath,
			CreatedByCsi:  !exists,
		}
	}
	if err := client.SetFSMeta(meta); err != nil {
		return nil, fmt.Errorf("error setting bucket metadata: %w", err)
	}

	glog.V(4).Infof("create volume %s", volumeID)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: capacityBytes,
			VolumeContext: req.GetParameters(),
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	bucketName, prefix := volumeIDToBucketPrefix(volumeID)

	// Check arguments
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("Invalid delete volume req: %v", req)
		return nil, err
	}
	glog.V(4).Infof("Deleting volume %s", volumeID)

	client, err := s3.NewClientFromSecret(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := client.BucketExists(bucketName)
	if err != nil {
		return nil, err
	}
	if exists {
		meta, err := client.GetFSMeta(bucketName, prefix)
		if err != nil {
			return nil, fmt.Errorf("failed to get metadata of buckect %s", volumeID)
		}
		if prefix != "" {
			if err := client.RemovePrefix(bucketName, prefix); err != nil {
				return nil, fmt.Errorf("unable to remove prefix: %w", err)
			}
		}
		if meta.CreatedByCsi {
			if err := client.RemoveBucket(bucketName); err != nil {
				glog.V(3).Infof("Failed to remove volume %s: %v", volumeID, err)
				return nil, err
			}
			glog.V(4).Infof("Bucket %s removed", volumeID)
		} else {
			glog.V(4).Infof("Bucket %s is not created by csi-s3, will not be deleted by csi-s3 automatically.", volumeID)
		}
	} else {
		glog.V(5).Infof("Bucket %s does not exist, ignoring request", volumeID)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}
	bucketName, prefix := volumeIDToBucketPrefix(req.GetVolumeId())

	s3, err := s3.NewClientFromSecret(req.GetSecrets())
	if err != nil {
		return nil, fmt.Errorf("failed to initialize S3 client: %s", err)
	}
	exists, err := s3.BucketExists(bucketName)
	if err != nil {
		return nil, err
	}

	if !exists {
		// return an error if the bucket of the requested volume does not exist
		return nil, status.Error(codes.NotFound, fmt.Sprintf("bucket of volume with id %s does not exist", req.GetVolumeId()))
	}

	if _, err := s3.GetFSMeta(bucketName, prefix); err != nil {
		// return an error if the fsmeta of the requested volume does not exist
		return nil, status.Error(codes.NotFound, fmt.Sprintf("fsmeta of volume with id %s does not exist", req.GetVolumeId()))
	}

	// We currently only support RWO
	supportedAccessMode := &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "Only single node writer is supported"}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessMode: supportedAccessMode,
				},
			},
		},
	}, nil
}

func (cs *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return &csi.ControllerExpandVolumeResponse{}, status.Error(codes.Unimplemented, "ControllerExpandVolume is not implemented")
}

func sanitizeVolumeID(volumeID string) string {
	volumeID = strings.ToLower(volumeID)
	if len(volumeID) > 63 {
		h := sha1.New()
		io.WriteString(h, volumeID)
		volumeID = hex.EncodeToString(h.Sum(nil))
	}
	return volumeID
}

// volumeIDToBucketPrefix returns the bucket name and prefix based on the volumeID.
// Prefix is empty if volumeID does not have a slash in the name.
func volumeIDToBucketPrefix(volumeID string) (string, string) {
	// if the volumeID has a slash in it, this volume is
	// stored under a certain prefix within the bucket.
	splitVolumeID := strings.Split(volumeID, "/")
	if len(splitVolumeID) > 1 {
		return splitVolumeID[0], splitVolumeID[1]
	}

	return volumeID, ""
}
