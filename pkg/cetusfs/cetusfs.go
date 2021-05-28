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
    "syscall"
    "encoding/json"
    "bufio"
    "crypto/md5"
    "encoding/hex"
    "net/http"
    "net/url"

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
    confinfo               map[string]string
)

const (
    // Directory where data for volumes and snapshots are persisted.
    // This can be ephemeral within the container or persisted if
    // backed by a Pod volume.
    dataRoot = "/csi-cetusfs-data-dir"

    // Extension with which snapshot files will be saved.
    snapshotExt = ".snap"
)

func init() {
    cetusFSVolumes = map[string]cetusFSVolume{}
    cetusFSVolumeSnapshots = map[string]cetusFSSnapshot{}
    confinfo = map[string]string{}
    conffile := "/etc/neucli/cetusfs_plugin.cfg"
    fconf, errconf := os.Open(conffile)
    if errconf != nil {
        glog.V(4).Infof("open plugin cfg file error : %v", errconf)
        return
    }
    confbuf := bufio.NewReader(fconf)
    for {
        line, readerr := confbuf.ReadString('\n')
        if readerr != nil {
            break
        }
        line = strings.TrimSpace(line)
        seqindex := strings.Index(line, "=")
        if seqindex > 0 {
            confinfo[line[:seqindex]] = line[seqindex+1:]
        }
    }
}

func getCetusVol(clusterid int, poolid int, volumename string) (int, error){
    storagemngip := confinfo["storagemngip"]
    storagemngport := confinfo["storagemngport"]
    endpoint := "http://"+storagemngip+":"+storagemngport
    mngtoken := confinfo["mngtoken"]

    client := &http.Client{}


    gettokenuri := endpoint+"/mng/dfsVolume"
    req, err := http.NewRequest("GET", gettokenuri, nil)
    if err != nil{
        glog.V(4).Infof("new request get volume  error")
        return -1, nil
    }
    req.Header.Add("X_auth_token", mngtoken)
    req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
    resp, resperr := client.Do(req)
    if resperr != nil{
        glog.V(4).Infof("request get volume error")
        return -1, nil
    }
    if resp.StatusCode != 200 {
        glog.V(4).Infof("response get volume info  status code not 200")
        return -1, nil
    }
    defer resp.Body.Close()
    respbody, _ := ioutil.ReadAll(resp.Body)

    type Volinfo struct {
        VolumeId int `json:"volumeId"`
        ClusterId int `json:"clusterId"`
        VolumeName string `json:"volumeName"`
        PoolId int `json:"poolId"`
        CreateType int `json:"createType"`
        Status string `json:"status"`
        OperatingStatus string `json:"operatingStatus"`
        DfsRunStatus string `json:"dfsRunStatus"`
        CreateTime string `json:"createTime"`
        Description string `json:"description"`
        CapacityTotal int `json:"capacityTotal"`
        CapacityUsed int `json:"capacityUsed"`
        ClusterName string `json:"clusterName"`
        PoolName string `json:"poolName"`
        VolGroupId int `json:"volGroupId"`
        IsOperating bool `json:"isOperating"`
        SnapEnable bool `json:"snapEnable"`
        SysUserId int `json:"sysUserId"`
        MarkDel int `json:"markDel"`
        VolumeNum int `json:"volumeNum"`
        AutoPackSelect bool `json:"autoPackSelect"`
    }

    type ALLVolinfo struct {
        Result string `json:"result"`
        Params []Volinfo `json:"params"`
    }
    var allvinfos ALLVolinfo

    json.Unmarshal([]byte(string(respbody)), &allvinfos)

    vid := 0
    for i := 0; i < len(allvinfos.Params); i++ {
        if clusterid == allvinfos.Params[i].ClusterId && poolid == allvinfos.Params[i].PoolId {
            if volumename == allvinfos.Params[i].VolumeName {
                vid = allvinfos.Params[i].VolumeId
                break
            }
        }
    }
    return vid, nil
}

func setCetusQuota(volumeid int, path string, cap int64) (error){
    storagemngip := confinfo["storagemngip"]
    storagemngport := confinfo["storagemngport"]
    endpoint := "http://"+storagemngip+":"+storagemngport
    mngtoken := confinfo["mngtoken"]

    client := &http.Client{}

    createjson := fmt.Sprintf("{\"path\":\"%s\",\"limitsize\":\"%d\"}", path, cap)
    createpayload := strings.NewReader(createjson)
    gettokenuri := fmt.Sprintf("%s/mng/dfsVolume/%d/quota", endpoint, volumeid)
    glog.V(4).Infof("new request create volume quota  %v : %v", gettokenuri, createjson)
    req, err := http.NewRequest("POST", gettokenuri, createpayload)
    if err != nil{
        glog.V(4).Infof("new request get volume  error")
        return  nil
    }
    req.Header.Add("X_auth_token", mngtoken)
    req.Header.Add("Content-Type", "application/json")
    resp, resperr := client.Do(req)
    if resperr != nil{
        glog.V(4).Infof("request get volume error")
        return nil
    }
    if resp.StatusCode != 200 {
        glog.V(4).Infof("response get volume info  status code not 200")
        return nil
    }
    return nil
}

func updateCetusQuota(volumeid int, path string, cap int64) (error){
    storagemngip := confinfo["storagemngip"]
    storagemngport := confinfo["storagemngport"]
    endpoint := "http://"+storagemngip+":"+storagemngport
    mngtoken := confinfo["mngtoken"]

    client := &http.Client{}

    createjson := fmt.Sprintf("{\"path\":\"%s\",\"limitsize\":\"%d\"}", path, cap)
    createpayload := strings.NewReader(createjson)
    gettokenuri := fmt.Sprintf("%s/mng/dfsVolume/%d/quota", endpoint, volumeid)
    glog.V(4).Infof("new request update volume quota %v : %v", gettokenuri, createjson)
    req, err := http.NewRequest("PUT", gettokenuri, createpayload)
    if err != nil{
        glog.V(4).Infof("new request get volume  error")
        return  nil
    }
    req.Header.Add("X_auth_token", mngtoken)
    req.Header.Add("Content-Type", "application/json")
    resp, resperr := client.Do(req)
    if resperr != nil{
        glog.V(4).Infof("request get volume error")
        return nil
    }
    if resp.StatusCode != 200 {
        glog.V(4).Infof("response get volume info  status code not 200")
        return nil
    }
    return nil
}

func delCetusQuota(volumeid int, path string) (error){
    storagemngip := confinfo["storagemngip"]
    storagemngport := confinfo["storagemngport"]
    endpoint := "http://"+storagemngip+":"+storagemngport
    mngtoken := confinfo["mngtoken"]

    client := &http.Client{}

    gettokenuri := fmt.Sprintf("%s/mng/dfsVolume/%d/quota/%s", endpoint, volumeid, path)
    glog.V(4).Infof("new request delete volume quota  %v ", gettokenuri)
    req, err := http.NewRequest("DELETE", gettokenuri, nil)
    if err != nil{
        glog.V(4).Infof("new request get volume  error")
        return  nil
    }
    req.Header.Add("X_auth_token", mngtoken)
    req.Header.Add("Content-Type", "application/json")
    resp, resperr := client.Do(req)
    if resperr != nil{
        glog.V(4).Infof("request get volume error")
        return nil
    }
    if resp.StatusCode != 200 {
        glog.V(4).Infof("response get volume info  status code not 200")
        return nil
    }
    return nil
}

func getClusterID() (int, int, int, error){
    storageclusterid := -1
    storagepoolid := -1
    serviceclusterid := -1

    if confinfo["csimod"] == "" || confinfo["csimod"] == "local" {
        glog.V(4).Infof("plugin cfg mod local")
        return 0, 0, 0, nil
    }
    storagemngip := confinfo["storagemngip"]
    storagemngport := confinfo["storagemngport"]
    username := confinfo["storagemngusername"]
    password := confinfo["storagemngpassword"]
    poolname := confinfo["storagemngpoolname"]
    serclustername := confinfo["storagemngsercluster"]

    endpoint := "http://"+storagemngip+":"+storagemngport

    bytepass := []byte(password)
    passmd5 := md5.Sum(bytepass)
    bytepass = []byte(hex.EncodeToString(passmd5[:]))
    passmd5 = md5.Sum(bytepass)
    passwordmd5 := hex.EncodeToString(passmd5[:])

    client := &http.Client{}

    gettokenuri := endpoint+"/mng/j_spring_security_check_kxapi"
    data := url.Values{"password":{passwordmd5},"username":{username}}
    body := strings.NewReader(data.Encode())
    req, err := http.NewRequest("POST", gettokenuri, body)
    if err != nil{
        glog.V(4).Infof("new request error")
        return -1, -1, -1, nil
    }
    req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
    resp, resperr := client.Do(req)
    if resperr != nil{
        glog.V(4).Infof("request token error")
        return -1, -1, -1, nil
    }
    mngtoken := resp.Header.Get("X_auth_token")
    confinfo["mngtoken"] = mngtoken

    getclusterinfouri := endpoint+"/mng/neuStorCluster?infoType=list&serClusterId=0"
    req, err = http.NewRequest("GET", getclusterinfouri, nil)
    if err != nil{
        glog.V(4).Infof("new request storage cluster info  error")
        return -1, -1, -1, nil
    }
    req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
    req.Header.Add("X_auth_token", mngtoken)
    resp, resperr = client.Do(req)
    if resperr != nil{
        glog.V(4).Infof("response storage cluster info  error")
        return -1, -1, -1, nil
    }
    if resp.StatusCode != 200 {
        glog.V(4).Infof("response storage cluster info  status code not 200")
        return -1, -1, -1, nil
    }
    defer resp.Body.Close()
    respbody, _ := ioutil.ReadAll(resp.Body)

    type Clusterinfo struct {
        ClusterId int `json:"clusterId"`
        ClusterName string `json:"clusterName"`
        Description string `json:"description"`
        DeviceNum int `json:"deviceNum"`
        BlockPoolNum int `json:"blockPoolNum"`
        DfsPoolNum int `json:"dfsPoolNum"`
        ObjPoolNum int `json:"objPoolNum"`
        OperatingStatus string `json:"operatingStatus"`
        HealthStatus string `json:"healthStatus"`
    }

    type ALLClusterinfo struct {
        Result string `json:"result"`
        Message string `json:"message"`
        Params []Clusterinfo `json:"params"`
    }
    var allcinfos ALLClusterinfo

    json.Unmarshal([]byte(string(respbody)), &allcinfos)
    for i := 0; i < len(allcinfos.Params); i++ {
        cid := fmt.Sprintf("%d", allcinfos.Params[i].ClusterId)
        getpoolinfouri := endpoint+"/mng/neuStorCluster/"+cid+"/dfsPool?infoType=list"
        req, err = http.NewRequest("GET", getpoolinfouri, nil)
        if err != nil{
            glog.V(4).Infof("new request storage pool info  error")
            return -1, -1, -1, nil
        }
        req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
        req.Header.Add("X_auth_token", mngtoken)
        resp, resperr = client.Do(req)
        if resperr != nil{
            glog.V(4).Infof("response storage pool info  error")
            return -1, -1, -1, nil
        }
        if resp.StatusCode != 200 {
            glog.V(4).Infof("response storage pool info  status code not 200")
            return -1, -1, -1, nil
        }
        defer resp.Body.Close()

        respbody, _ := ioutil.ReadAll(resp.Body)

        type Poolinfo struct {
            DfsPoolId int `json:"dfsPoolId"`
            PoolName string `json:"poolName"`
            CapacityTotal int `json:"capacityTotal"`
            Description string `json:"description"`
            OperatingStatus string `json:"operatingStatus"`
            HealthStatus string `json:"healthStatus"`
            HasDisk bool `json:"hasDisk"`
        }


        type ALLPoolinfo struct {
            Result string `json:"result"`
            Params []Poolinfo `json:"params"`
        }
        var allpinfos ALLPoolinfo

        json.Unmarshal([]byte(string(respbody)), &allpinfos)
        findpoolid := false
        for j := 0; j < len(allpinfos.Params); j++ {
            if allpinfos.Params[j].PoolName == poolname {
                findpoolid = true
                storageclusterid = allcinfos.Params[i].ClusterId
                storagepoolid = allpinfos.Params[j].DfsPoolId
                //serviceclusterid := -1
                break
            }
        }
        if findpoolid {
            break
        }
    }
    for k := 0; k < len(allcinfos.Params); k++ {
        cid := fmt.Sprintf("%d", allcinfos.Params[k].ClusterId)
        getserclusterinfouri := endpoint+"/mng/sercluster?infoType=list&clusterType=1&storClusterId="+cid
        req, err = http.NewRequest("GET", getserclusterinfouri, nil)
        if err != nil{
            glog.V(4).Infof("new request service cluster info  error")
            return -1, -1, -1, nil
        }
        req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
        req.Header.Add("X_auth_token", mngtoken)
        resp, resperr = client.Do(req)
        if resperr != nil{
            glog.V(4).Infof("response service cluster info  error")
            return -1, -1, -1, nil
        }
        if resp.StatusCode != 200 {
            glog.V(4).Infof("response service cluster info  status code not 200")
            return -1, -1, -1, nil
        }
        defer resp.Body.Close()

        respbody, _ := ioutil.ReadAll(resp.Body)

        type SerClusterinfo struct {
            SerClusterId int `json:"id"`
            ClusterName string `json:"clusterName"`
            ClusterType int `json:"clusterType"`
            StorClusterId int `json:"storClusterId"`
            Description string `json:"description"`
            StorClusterName string `json:"storClusterName"`
            OperatingStatus string `json:"operatingStatus"`
        }


        type ALLSerClusterinfo struct {
            Result string `json:"result"`
            Message string `json:"message"`
            Params []SerClusterinfo `json:"params"`
        }
        var allsinfos ALLSerClusterinfo

        json.Unmarshal([]byte(string(respbody)), &allsinfos)
        findserclusterid := false
        for j := 0; j < len(allsinfos.Params); j++ {
            if allsinfos.Params[j].ClusterName == serclustername {
                findserclusterid = true
                serviceclusterid = allsinfos.Params[j].SerClusterId
                break
            }
        }
        if findserclusterid {
            break
        }
    }

    return storageclusterid, storagepoolid, serviceclusterid, nil

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

func discoverExistingVol() {
    csijsonfile := "/etc/neucli/cetusfs_csi_info.json"
    glog.V(4).Infof("discovering existing vol in %s", csijsonfile)

    _, ferr := os.Lstat(csijsonfile)
    if os.IsNotExist(ferr) {
        glog.V(4).Infof("no found configure file %s", csijsonfile)
        return 
    }
    filePtr, err := os.Open(csijsonfile)
    if err != nil {
        fmt.Println("Open file failed [Err:%s]", err.Error())
        return
    }
    defer filePtr.Close()


    decoder := json.NewDecoder(filePtr)
    err = decoder.Decode(&cetusFSVolumes)
    if err != nil {
        fmt.Println("Decoder failed", err.Error())

    } else {
        fmt.Println("Decoder success")
        fmt.Println("load volume ", cetusFSVolumes)
    }
    
}

func saveExistingVol() {
    csijsonfile := "/etc/neucli/cetusfs_csi_info.json"
    if ferr := os.MkdirAll(filepath.Dir(csijsonfile), os.FileMode(0755)); ferr != nil {
        glog.V(4).Infof("can not open /etc/neucli/flexblock_csi_info.json file: %v", ferr)
        return
    }
    csiinfofile, fileerr := os.OpenFile(csijsonfile, syscall.O_RDWR|syscall.O_CREAT|syscall.O_TRUNC, 0666)
    if fileerr != nil {
        glog.V(4).Infof("can not open /etc/neucli/flexblock_csi_info.json file: %v", fileerr)
        return
    }
    defer csiinfofile.Close() // in case we fail before the explicit close

    encoder := json.NewEncoder(csiinfofile)
    jsonerr := encoder.Encode(cetusFSVolumes)
    if jsonerr != nil {
        glog.V(4).Infof("Encoder failed", jsonerr)
    }

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

    discoverExistingVol()
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
        volconffile := "/etc/neucli/cetusfs_plugin_vol.info"
        _, ferr := os.Stat(volconffile)
        if ferr == nil {
            f, eerr := os.Open(volconffile)
            if eerr == nil {
                reader := bufio.NewReader(f);
                line, _, rerr := reader.ReadLine();
                if rerr == nil {
                    volname := string(line)

                    var cmd []string
                    executor := utilexec.New()
                    size := fmt.Sprintf("%dM", cap/mib)

                    storclusterid, storpoolid, _, getclustererr := getClusterID()
                    if getclustererr != nil {
                        return nil, status.Errorf(codes.Internal, "failed get cluster info %v", getclustererr)
                    }
                    if storclusterid == 0 {
                        cmd = []string{"cetusadm", "quota", volname, "enable"}
                        glog.V(4).Infof("Command Start: %v", cmd)
                        outumount, errumount := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
                        glog.V(4).Infof("Command Finish: %v", string(outumount))
                        if errumount != nil {
                            glog.V(4).Infof("failed update cetus quota  update target %v: %v", errumount, string(outumount))
                        }

                        cmd = []string{"cetusadm", "quota", volname, "limit", "/"+volID, size+"B"}
                        glog.V(4).Infof("Command Start: %v", cmd)
                        outumount, errumount = executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
                        glog.V(4).Infof("Command Finish: %v", string(outumount))
                        if errumount != nil {
                            glog.V(4).Infof("failed update cetus quota  update target %v: %v", errumount, string(outumount))
                        }
                    } else {
                        vid, viderr := getCetusVol(storclusterid, storpoolid, volname)
                        if viderr != nil {
                            glog.V(4).Infof("failed get cetus volume %v: %v",viderr, volname)
                        }
                        glog.V(4).Infof("get cetus volume %v: %v",volname, vid)
                        setCetusQuota(vid, "/"+volID, cap)
                    }

                }
            }
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

    saveExistingVol()

    return &cetusfsVol, nil
}

// updateVolume updates the existing cetusfs volume.
func updateCetusfsVolume(volID string, volume cetusFSVolume) error {
    glog.V(4).Infof("updating cetusfs volume: %s", volID)

    vol, err := getVolumeByID(volID)
    if err != nil {
        // Return OK if the volume is not found.
        return nil
    }
    if vol.VolAccessType == mountAccess {
        volconffile := "/etc/neucli/cetusfs_plugin_vol.info"
        _, ferr := os.Stat(volconffile)
        if ferr == nil {
            f, eerr := os.Open(volconffile)
            if eerr == nil {
                reader := bufio.NewReader(f);
                line, _, rerr := reader.ReadLine();
                if rerr == nil {
                    volname := string(line)

                    var cmd []string
                    executor := utilexec.New()
                    size := fmt.Sprintf("%dM", volume.VolSize/mib)

                    storclusterid, storpoolid, _, getclustererr := getClusterID()
                    if getclustererr != nil {
                        return status.Errorf(codes.Internal, "failed get cluster info %v", getclustererr)
                    }
                    if storclusterid == 0 {
                        cmd = []string{"cetusadm", "quota", volname, "enable"}
                        glog.V(4).Infof("Command Start: %v", cmd)
                        outumount, errumount := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
                        glog.V(4).Infof("Command Finish: %v", string(outumount))
                        if errumount != nil {
                            glog.V(4).Infof("failed update cetus quota  update target %v: %v", errumount, string(outumount))
                        }

                        cmd = []string{"cetusadm", "quota", volname, "limit", "/"+volID, size+"B"}
                        glog.V(4).Infof("Command Start: %v", cmd)
                        outumount, errumount = executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
                        glog.V(4).Infof("Command Finish: %v", string(outumount))
                        if errumount != nil {
                            glog.V(4).Infof("failed update cetus quota  update target %v: %v", errumount, string(outumount))
                        }
                    } else {
                        vid, viderr := getCetusVol(storclusterid, storpoolid, volname)
                        if viderr != nil {
                            glog.V(4).Infof("failed get cetus volume %v: %v",viderr, volname)
                        }
                        updateCetusQuota(vid, "/"+volID, volume.VolSize)
                    }
                }
            }
        }

    }

    cetusFSVolumes[volID] = volume

    saveExistingVol()

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

    if vol.VolAccessType == mountAccess {
        volconffile := "/etc/neucli/cetusfs_plugin_vol.info"
        _, ferr := os.Stat(volconffile)
        if ferr == nil {
            f, eerr := os.Open(volconffile)
            if eerr == nil {
                reader := bufio.NewReader(f);
                line, _, rerr := reader.ReadLine();
                if rerr == nil {
                    volname := string(line)

                    var cmd []string
                    executor := utilexec.New()

                    storclusterid, storpoolid, _, getclustererr := getClusterID()
                    if getclustererr != nil {
                        return status.Errorf(codes.Internal, "failed get cluster info %v", getclustererr)
                    }
                    if storclusterid == 0 {
                        cmd = []string{"cetusadm", "quota", volname, "unlimit", "/"+volID}
                        glog.V(4).Infof("Command Start: %v", cmd)
                        outumount, errumount := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
                        glog.V(4).Infof("Command Finish: %v", string(outumount))
                        if errumount != nil {
                            glog.V(4).Infof("failed update cetus quota  update target %v: %v", errumount, string(outumount))
                        }
                    } else {
                        vid, viderr := getCetusVol(storclusterid, storpoolid, volname)
                        if viderr != nil {
                            glog.V(4).Infof("failed get cetus volume %v: %v",viderr, volname)
                        }
                        delCetusQuota(vid, "/"+volID)
                    }

                }
            }
        }

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

    saveExistingVol()

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
