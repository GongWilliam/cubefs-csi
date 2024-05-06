package cubefs

import (
	"errors"
	"fmt"
	"os"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	CSI_NODE_DS_NAME        = "cfs-csi-node"
	CUBEFS_CLIENT_IMAGE_KEY = "cubefs-client-image"
	DS_DRIVER_NAME          = "cfs-driver"

	CLIENT_CPU_REQ_KEY   = "cfs-client-cpu-req"
	CLIENT_CPU_LIMIT_KEY = "cfs-client-cpu-limit"
	CLIENT_MEM_REQ_KEY   = "cfs-client-mem-req"
	CLIENT_MEM_LIMIT_KEY = "cfs-client-mem-limit"

	// default 1c1g
	DEFAULT_CPU_REQ   = "512m"
	DEFAULT_CPU_LIMIT = "512m"
	DEFAULT_MEM_REQ   = "1000Mi"
	DEFAULT_MEM_LIMIT = "1000Mi"
)

func (ns *nodeServer) ProduceClientPod(ctx context.Context, pvName string) error {
	newPod, err := ns.genClientPod(ctx, pvName)
	if err != nil {
		glog.Errorf("gen client pod failed: %v", err.Error())
		return err
	}
	_, err = ns.createPod(ctx, newPod)
	if err != nil {
		glog.Errorf("create client pod failed: %v", err.Error())
		return err
	}
	return nil
}

func (ns *nodeServer) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if pod == nil {
		glog.Error("Create pod: pod is nil")
		return nil, nil
	}
	glog.Infof("Create pod %s", pod.Name)
	mntPod, err := ns.Driver.K8sClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		glog.Errorf("Can't create pod %s: %v", pod.Name, err.Error())
		return nil, err
	}
	return mntPod, nil
}

func (ns *nodeServer) getPVCByPvName(ctx context.Context, pvName string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	pv, err := ns.Driver.K8sClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	pvcName := pv.Spec.ClaimRef.Name
	pvcNS := pv.Spec.ClaimRef.Namespace
	glog.Infof("pv %v's pvc is %v/%v", pvName, pvcNS, pvcName)
	pvc, err := ns.Driver.K8sClient.CoreV1().PersistentVolumeClaims(pvcNS).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	return pvc, pv, nil
}

func (ns *nodeServer) GetPodResourceInfo(ctx context.Context, pvName string) (map[string]string, error) {
	// get pvc by pv name
	pvc, pv, err := ns.getPVCByPvName(ctx, pvName)
	if err != nil {
		return nil, fmt.Errorf("get pvc by pv's name failed: %v", err.Error())
	}
	ret := pv.Spec.CSI.VolumeAttributes
	// get pod cpu/mem setting from pvc
	cpuReq := DEFAULT_CPU_REQ
	if v, ok := pvc.Labels[CLIENT_CPU_REQ_KEY]; ok && v != "" {
		cpuReq = v
	}
	ret[CLIENT_CPU_REQ_KEY] = cpuReq

	cpuLimit := DEFAULT_CPU_LIMIT
	if v, ok := pvc.Labels[CLIENT_CPU_LIMIT_KEY]; ok && v != "" {
		cpuLimit = v
	}
	ret[CLIENT_CPU_LIMIT_KEY] = cpuLimit

	memReq := DEFAULT_MEM_REQ
	if v, ok := pvc.Labels[CLIENT_MEM_REQ_KEY]; ok && v != "" {
		memReq = v
	}
	ret[CLIENT_MEM_REQ_KEY] = memReq

	memLimit := DEFAULT_MEM_LIMIT
	if v, ok := pvc.Labels[CLIENT_MEM_LIMIT_KEY]; ok && v != "" {
		memLimit = v
	}
	ret[CLIENT_MEM_LIMIT_KEY] = memLimit

	return ret, nil
}

func (ns *nodeServer) getImageName(ctx context.Context, ds *v1.DaemonSet) (string, error) {
	// if can get info from labels, then return
	if _, ok := ds.Labels[CUBEFS_CLIENT_IMAGE_KEY]; ok {
		return ds.Labels[CUBEFS_CLIENT_IMAGE_KEY], nil
	}
	// get csi-driver image
	for i := range ds.Spec.Template.Spec.Containers {
		if ds.Spec.Template.Spec.Containers[i].Name == DS_DRIVER_NAME {
			return ds.Spec.Template.Spec.Containers[i].Image, nil
		}
	}
	return "", errors.New("can't get image name for client pod")
}

func (ns *nodeServer) getNS() string {
	return os.Getenv("NAMESPACE")
}

func (ns *nodeServer) genClientPod(ctx context.Context, pvName string) (*corev1.Pod, error) {
	namespace := ns.getNS()
	ds, err := ns.Driver.K8sClient.AppsV1().DaemonSets(namespace).Get(ctx, CSI_NODE_DS_NAME, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	imageName, err := ns.getImageName(ctx, ds)
	if err != nil {
		return nil, err
	}
	resourceMap, err := ns.GetPodResourceInfo(ctx, pvName)
	if err != nil {
		return nil, err
	}

	podName := ns.NodeID + "-" + pvName
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns.getNS(),
			Labels:    make(map[string]string),
			Name:      podName,
		},
		Spec: corev1.PodSpec{
			Containers:         []corev1.Container{ns.genClientContainer(imageName, resourceMap)},
			NodeName:           ns.NodeID,
			HostNetwork:        ds.Spec.Template.Spec.HostNetwork,
			HostPID:            ds.Spec.Template.Spec.HostIPC,
			HostIPC:            ds.Spec.Template.Spec.HostIPC,
			ServiceAccountName: ds.Spec.Template.Spec.ServiceAccountName,
			Tolerations:        ds.Spec.Template.Spec.Tolerations,
		},
	}, nil
}

func (ns *nodeServer) genClientContainer(imageName string, resourceInfo map[string]string) corev1.Container {
	isPrivileged := true
	rootUser := int64(0)
	return corev1.Container{
		Name:  "cfs-client",
		Image: imageName,
		SecurityContext: &corev1.SecurityContext{
			Privileged: &isPrivileged,
			RunAsUser:  &rootUser,
		},
		Env: []corev1.EnvVar{},
		Resources: corev1.ResourceRequirements{
			Limits: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse(resourceInfo[CLIENT_CPU_LIMIT_KEY]),
				corev1.ResourceMemory: resource.MustParse(resourceInfo[CLIENT_MEM_LIMIT_KEY]),
			},
			Requests: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse(resourceInfo[CLIENT_CPU_REQ_KEY]),
				corev1.ResourceMemory: resource.MustParse(resourceInfo[CLIENT_MEM_REQ_KEY]),
			},
		},
	}
}
