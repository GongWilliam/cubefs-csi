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

package cubefs

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	csicommon "github.com/cubefs/cubefs-csi/pkg/csi-common"
	"github.com/golang/glog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/mount"
)

const DriverName = "csi.cubefs.com"

type driver struct {
	*csicommon.CSIDriver
	ids *csicommon.DefaultIdentityServer
	cs  *controllerServer
	ns  *nodeServer
	Config
}

type Config struct {
	NodeID         string
	DriverName     string
	KubeConfig     string
	Version        string
	RemountDamaged bool
	KubeletRootDir string
}

func NewDriver(conf Config) (*driver, error) {
	glog.Infof("driverName:%v, version:%v, nodeID:%v", conf.DriverName, conf.Version, conf.NodeID)
	clientSet, err := initClientSet(conf.KubeConfig)
	if err != nil {
		glog.Errorf("init client-go Clientset fail. kubeconfig:%v, err:%v", conf.KubeConfig, err)
		return nil, err
	}

	csiDriver := csicommon.NewCSIDriver(conf.DriverName, conf.Version, conf.NodeID, clientSet)
	if csiDriver == nil {
		return nil, status.Error(codes.InvalidArgument, "csiDriver init fail")
	}

	csiDriver.AddControllerServiceCapabilities(
		[]csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		})
	csiDriver.AddVolumeCapabilityAccessModes(
		[]csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		})

	return &driver{
		CSIDriver: csiDriver,
		Config:    conf,
	}, nil
}

func initClientSet(kubeconfig string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		// creates the out-of-cluster config
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		// creates the in-cluster config
		config, err = rest.InClusterConfig()
	}

	if err != nil {
		return nil, err
	}

	config.Timeout = 10 * time.Second

	if os.Getenv("KUBE_QPS") != "" {
		kubeQpsInt, err := strconv.Atoi(os.Getenv("KUBE_QPS"))
		if err != nil {
			return nil, err
		}
		config.QPS = float32(kubeQpsInt)
	}
	if os.Getenv("KUBE_BURST") != "" {
		kubeBurstInt, err := strconv.Atoi(os.Getenv("KUBE_BURST"))
		if err != nil {
			return nil, err
		}
		config.Burst = kubeBurstInt
	}

	return kubernetes.NewForConfig(config)
}

func NewNodeServer(d *driver) *nodeServer {
	return &nodeServer{
		DefaultNodeServer: csicommon.NewDefaultNodeServer(d.CSIDriver),
		mounter:           mount.New(""),
		Config:            d.Config,
	}
}

func NewIdentityServer(d *driver) *identityServer {
	return &identityServer{
		DefaultIdentityServer: csicommon.NewDefaultIdentityServer(d.CSIDriver),
	}
}

func NewControllerServer(d *driver) *controllerServer {
	return &controllerServer{
		DefaultControllerServer: csicommon.NewDefaultControllerServer(d.CSIDriver),
		driver:                  d,
	}
}

func (d *driver) Run(endpoint string) {
	nodeServer := NewNodeServer(d)
	if nodeName := os.Getenv("KUBE_NODE_NAME"); d.RemountDamaged && nodeName != "" {
		nodeServer.remountDamagedVolumes(nodeName)
	}
	// // test, need to remove this
	// err := nodeServer.ProduceClientPod(context.Background(), "pvc-62b19a73-8f6f-4545-bf6e-c6ad929c8a0c")
	// if err != nil {
	// 	log.Printf("create client pod failed: %v", err.Error())
	// }
	csicommon.RunControllerandNodePublishServer(endpoint, NewIdentityServer(d), NewControllerServer(d), NewNodeServer(d))
}

func (d *driver) RunClient(mountpoint, pvName string) error {
	nodeServer := NewNodeServer(d)
	volInfos, err := nodeServer.GetPodResourceInfo(context.Background(), pvName)
	if err != nil {
		log.Printf("get volume info failed: %v", err.Error())
		return fmt.Errorf("get volume info failed: %v", err.Error())
	}

	cfsServer, err := newCfsServer(pvName, volInfos)
	if err != nil {
		log.Printf("new cfs server failed: %v", err.Error())
		return fmt.Errorf("new cfs server failed: %v", err.Error())
	}

	if err := cfsServer.persistClientConf(mountpoint); err != nil {
		log.Printf("persist client config file failed: %v", err.Error())
		return fmt.Errorf("persist client config file failed: %v", err.Error())
	}

	if err := cfsServer.runClient(); err != nil {
		log.Printf("mount failed: %v", err.Error())
		return fmt.Errorf("mount failed: %v", err.Error())
	}
	return nil
}

func (d *driver) queryPersistentVolumes(ctx context.Context, pvName string) (*v1.PersistentVolume, error) {
	persistentVolume, err := d.CSIDriver.K8sClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	if persistentVolume == nil {
		return nil, status.Error(codes.Unknown, fmt.Sprintf("not found PersistentVolume[%v]", pvName))
	}

	return persistentVolume, nil
}
