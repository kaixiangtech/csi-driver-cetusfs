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

package cetusfs

import (
    "errors"
    "fmt"
    "io"
    "io/ioutil"
    "os"
    "path/filepath"
    "strings"

    "github.com/golang/glog"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
    "k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
    utilexec "k8s.io/utils/exec"

    timestamp "github.com/golang/protobuf/ptypes/timestamp"
)

const (
    kib    int64 = 1024
    mib    int64 = kib * 1024
    gib    int64 = mib * 1024
    gib100 int64 = gib * 100
    tib    int64 = gib * 1024
    tib100 int64 = tib * 100
)

type cetusFS struct {
    name              string
    nodeID            string
    version           string
    endpoint          string
    ephemeral         bool
    maxVolumesPerNode int64

    ids *identityServer
    ns  *nodeServer
    cs  *controllerServer
}

type cetusFSVolume struct {
    VolName       string     `json:"volName"`
    VolID         string     `json:"volID"`
    VolSize       int64      `json:"volSize"`
    VolPath       string     `json:"volPath"`
    VolAccessType accessType `json:"volAccessType"`
    ParentVolID   string     `json:"parentVolID,omitempty"`
    ParentSnapID  string     `json:"parentSnapID,omitempty"`
    Ephemeral     bool       `json:"ephemeral"`
}

type cetusFSSnapshot struct {
    Name         string               `json:"name"`
    Id           string               `json:"id"`
    VolID        string               `json:"volID"`
    Path         string               `json:"path"`
    CreationTime *timestamp.Timestamp `json:"creationTime"`
    SizeBytes    int64                `json:"sizeBytes"`
    ReadyToUse   bool                 `json:"readyToUse"`
}

var (
    vendorVersion = "dev"

    cetusFSVolumes         map[string]cetusFSVolume
    cetusFSVolumeSnapshots map[string]cetusFSSnapshot
)

const (
    // Directory where data for volumes and snapshots are persisted.
    // This can be ephemeral within the container or persisted if
    // backed by a Pod volume.
    dataRoot = "/csi-data-dir"

    // Extension with which snapshot files will be saved.
    snapshotExt = ".snap"
)

func init() {
    cetusFSVolumes = map[string]cetusFSVolume{}
    cetusFSVolumeSnapshots = map[string]cetusFSSnapshot{}
}

func NewCetusFSDriver(driverName, nodeID, endpoint string, ephemeral bool, maxVolumesPerNode int64, version string) (*cetusFS, error) {
    if driverName == "" {
        return nil, errors.New("no driver name provided")
    }

    if nodeID == "" {
        return nil, errors.New("no node id provided")
    }

    if endpoint == "" {
        return nil, errors.New("no driver endpoint provided")
    }
    if version != "" {
        vendorVersion = version
    }

    if err := os.MkdirAll(dataRoot, 0750); err != nil {
        return nil, fmt.Errorf("failed to create dataRoot: %v", err)
    }

    glog.Infof("Driver: %v ", driverName)
    glog.Infof("Version: %s", vendorVersion)

    return &cetusFS{
        name:              driverName,
        version:           vendorVersion,
        nodeID:            nodeID,
        endpoint:          endpoint,
        ephemeral:         ephemeral,
        maxVolumesPerNode: maxVolumesPerNode,
    }, nil
}

func getSnapshotID(file string) (bool, string) {
    glog.V(4).Infof("file: %s", file)
    // Files with .snap extension are volumesnapshot files.
    // e.g. foo.snap, foo.bar.snap
    if filepath.Ext(file) == snapshotExt {
        return true, strings.TrimSuffix(file, snapshotExt)
    }
    return false, ""
}

func discoverExistingSnapshots() {
    glog.V(4).Infof("discovering existing snapshots in %s", dataRoot)
    files, err := ioutil.ReadDir(dataRoot)
    if err != nil {
        glog.Errorf("failed to discover snapshots under %s: %v", dataRoot, err)
    }
    for _, file := range files {
        isSnapshot, snapshotID := getSnapshotID(file.Name())
        if isSnapshot {
            glog.V(4).Infof("adding snapshot %s from file %s", snapshotID, getSnapshotPath(snapshotID))
            cetusFSVolumeSnapshots[snapshotID] = cetusFSSnapshot{
                Id:         snapshotID,
                Path:       getSnapshotPath(snapshotID),
                ReadyToUse: true,
            }
        }
    }
}

func (hp *cetusFS) Run() {
    // Create GRPC servers
    hp.ids = NewIdentityServer(hp.name, hp.version)
    hp.ns = NewNodeServer(hp.nodeID, hp.ephemeral, hp.maxVolumesPerNode)
    hp.cs = NewControllerServer(hp.ephemeral, hp.nodeID)

    discoverExistingSnapshots()
    s := NewNonBlockingGRPCServer()
    s.Start(hp.endpoint, hp.ids, hp.cs, hp.ns)
    s.Wait()
}

func getVolumeByID(volumeID string) (cetusFSVolume, error) {
    if cetusFSVol, ok := cetusFSVolumes[volumeID]; ok {
        return cetusFSVol, nil
    }
    return cetusFSVolume{}, fmt.Errorf("volume id %s does not exist in the volumes list", volumeID)
}

func getVolumeByName(volName string) (cetusFSVolume, error) {
    for _, cetusFSVol := range cetusFSVolumes {
        if cetusFSVol.VolName == volName {
            return cetusFSVol, nil
        }
    }
    return cetusFSVolume{}, fmt.Errorf("volume name %s does not exist in the volumes list", volName)
}

func getSnapshotByName(name string) (cetusFSSnapshot, error) {
    for _, snapshot := range cetusFSVolumeSnapshots {
        if snapshot.Name == name {
            return snapshot, nil
        }
    }
    return cetusFSSnapshot{}, fmt.Errorf("snapshot name %s does not exist in the snapshots list", name)
}

// getVolumePath returns the canonical path for cetusfs volume
func getVolumePath(volID string) string {
    return filepath.Join(dataRoot, volID)
}

// createVolume create the directory for the cetusfs volume.
// It returns the volume path or err if one occurs.
func createCetusfsVolume(volID, name string, cap int64, volAccessType accessType, ephemeral bool) (*cetusFSVolume, error) {
    glog.V(4).Infof("create cetusfs volume: %s", volID)
    path := getVolumePath(volID)

    switch volAccessType {
    case mountAccess:
        err := os.MkdirAll(path, 0777)
        if err != nil {
            return nil, err
        }
    case blockAccess:
        executor := utilexec.New()
        size := fmt.Sprintf("%dM", cap/mib)
        // Create a block file.
        _, err := os.Stat(path)
        if err != nil {
            if os.IsNotExist(err) {
                out, err := executor.Command("fallocate", "-l", size, path).CombinedOutput()
                if err != nil {
                    return nil, fmt.Errorf("failed to create block device: %v, %v", err, string(out))
                }
            } else {
                return nil, fmt.Errorf("failed to stat block device: %v, %v", path, err)
            }
        }

        // Associate block file with the loop device.
        volPathHandler := volumepathhandler.VolumePathHandler{}
        _, err = volPathHandler.AttachFileDevice(path)
        if err != nil {
            // Remove the block file because it'll no longer be used again.
            if err2 := os.Remove(path); err2 != nil {
                glog.Errorf("failed to cleanup block file %s: %v", path, err2)
            }
            return nil, fmt.Errorf("failed to attach device %v: %v", path, err)
        }
    default:
        return nil, fmt.Errorf("unsupported access type %v", volAccessType)
    }

    cetusfsVol := cetusFSVolume{
        VolID:         volID,
        VolName:       name,
        VolSize:       cap,
        VolPath:       path,
        VolAccessType: volAccessType,
        Ephemeral:     ephemeral,
    }
    cetusFSVolumes[volID] = cetusfsVol
    return &cetusfsVol, nil
}

// updateVolume updates the existing cetusfs volume.
func updateCetusfsVolume(volID string, volume cetusFSVolume) error {
    glog.V(4).Infof("updating cetusfs volume: %s", volID)

    if _, err := getVolumeByID(volID); err != nil {
        // Return OK if the volume is not found.
        return nil
    }

    cetusFSVolumes[volID] = volume
    return nil
}

// deleteVolume deletes the directory for the cetusfs volume.
func deleteCetusfsVolume(volID string) error {
    glog.V(4).Infof("deleting cetusfs volume: %s", volID)

    vol, err := getVolumeByID(volID)
    if err != nil {
        // Return OK if the volume is not found.
        return nil
    }

    if vol.VolAccessType == blockAccess {
        volPathHandler := volumepathhandler.VolumePathHandler{}
        path := getVolumePath(volID)
        glog.V(4).Infof("deleting loop device for file %s if it exists", path)
        if err := volPathHandler.DetachFileDevice(path); err != nil {
            return fmt.Errorf("failed to remove loop device for file %s: %v", path, err)
        }
    }

    path := getVolumePath(volID)
    if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
        return err
    }
    delete(cetusFSVolumes, volID)
    return nil
}

// cetusFSIsEmpty is a simple check to determine if the specified cetusfs directory
// is empty or not.
func cetusFSIsEmpty(p string) (bool, error) {
    f, err := os.Open(p)
    if err != nil {
        return true, fmt.Errorf("unable to open cetusfs volume, error: %v", err)
    }
    defer f.Close()

    _, err = f.Readdir(1)
    if err == io.EOF {
        return true, nil
    }
    return false, err
}

// loadFromSnapshot populates the given destPath with data from the snapshotID
func loadFromSnapshot(size int64, snapshotId, destPath string, mode accessType) error {
    snapshot, ok := cetusFSVolumeSnapshots[snapshotId]
    if !ok {
        return status.Errorf(codes.NotFound, "cannot find snapshot %v", snapshotId)
    }
    if snapshot.ReadyToUse != true {
        return status.Errorf(codes.Internal, "snapshot %v is not yet ready to use.", snapshotId)
    }
    if snapshot.SizeBytes > size {
        return status.Errorf(codes.InvalidArgument, "snapshot %v size %v is greater than requested volume size %v", snapshotId, snapshot.SizeBytes, size)
    }
    snapshotPath := snapshot.Path

    var cmd []string
    switch mode {
    case mountAccess:
        cmd = []string{"tar", "zxvf", snapshotPath, "-C", destPath}
    case blockAccess:
        cmd = []string{"dd", "if=" + snapshotPath, "of=" + destPath}
    default:
        return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
    }

    executor := utilexec.New()
    glog.V(4).Infof("Command Start: %v", cmd)
    out, err := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
    glog.V(4).Infof("Command Finish: %v", string(out))
    if err != nil {
        return status.Errorf(codes.Internal, "failed pre-populate data from snapshot %v: %v: %s", snapshotId, err, out)
    }
    return nil
}

// loadFromVolume populates the given destPath with data from the srcVolumeID
func loadFromVolume(size int64, srcVolumeId, destPath string, mode accessType) error {
    cetusFSVolume, ok := cetusFSVolumes[srcVolumeId]
    if !ok {
        return status.Error(codes.NotFound, "source volumeId does not exist, are source/destination in the same storage class?")
    }
    if cetusFSVolume.VolSize > size {
        return status.Errorf(codes.InvalidArgument, "volume %v size %v is greater than requested volume size %v", srcVolumeId, cetusFSVolume.VolSize, size)
    }
    if mode != cetusFSVolume.VolAccessType {
        return status.Errorf(codes.InvalidArgument, "volume %v mode is not compatible with requested mode", srcVolumeId)
    }

    switch mode {
    case mountAccess:
        return loadFromFilesystemVolume(cetusFSVolume, destPath)
    case blockAccess:
        return loadFromBlockVolume(cetusFSVolume, destPath)
    default:
        return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
    }
}

func loadFromFilesystemVolume(cetusFSVolume cetusFSVolume, destPath string) error {
    srcPath := cetusFSVolume.VolPath
    isEmpty, err := cetusFSIsEmpty(srcPath)
    if err != nil {
        return status.Errorf(codes.Internal, "failed verification check of source cetusfs volume %v: %v", cetusFSVolume.VolID, err)
    }

    // If the source cetusfs volume is empty it's a noop and we just move along, otherwise the cp call will fail with a a file stat error DNE
    if !isEmpty {
        args := []string{"-a", srcPath + "/.", destPath + "/"}
        executor := utilexec.New()
        out, err := executor.Command("cp", args...).CombinedOutput()
        if err != nil {
            return status.Errorf(codes.Internal, "failed pre-populate data from volume %v: %v: %s", cetusFSVolume.VolID, err, out)
        }
    }
    return nil
}

func loadFromBlockVolume(cetusFSVolume cetusFSVolume, destPath string) error {
    srcPath := cetusFSVolume.VolPath
    args := []string{"if=" + srcPath, "of=" + destPath}
    executor := utilexec.New()
    out, err := executor.Command("dd", args...).CombinedOutput()
    if err != nil {
        return status.Errorf(codes.Internal, "failed pre-populate data from volume %v: %v: %s", cetusFSVolume.VolID, err, out)
    }
    return nil
}