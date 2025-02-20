package daemonset

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kmmv1beta1 "github.com/kubernetes-sigs/kernel-module-management/api/v1beta1"
	"github.com/kubernetes-sigs/kernel-module-management/internal/constants"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	kubeletDevicePluginsVolumeName = "kubelet-device-plugins"
	kubeletDevicePluginsPath       = "/var/lib/kubelet/device-plugins"
	nodeLibModulesPath             = "/lib/modules"
	nodeLibModulesVolumeName       = "node-lib-modules"
	nodeUsrLibModulesPath          = "/usr/lib/modules"
	nodeUsrLibModulesVolumeName    = "node-usr-lib-modules"
	nodeVarLibFirmwarePath         = "/var/lib/firmware"
	nodeVarLibFirmwareVolumeName   = "node-var-lib-firmware"
	devicePluginKernelVersion      = ""
)

//go:generate mockgen -source=daemonset.go -package=daemonset -destination=mock_daemonset.go

type DaemonSetCreator interface {
	GarbageCollect(ctx context.Context, existingDS map[string]*appsv1.DaemonSet, validKernels sets.String) ([]string, error)
	ModuleDaemonSetsByKernelVersion(ctx context.Context, name, namespace string) (map[string]*appsv1.DaemonSet, error)
	SetDriverContainerAsDesired(ctx context.Context, ds *appsv1.DaemonSet, image string, mod kmmv1beta1.Module, kernelVersion string) error
	SetDevicePluginAsDesired(ctx context.Context, ds *appsv1.DaemonSet, mod *kmmv1beta1.Module) error
	GetNodeLabelFromPod(pod *v1.Pod, moduleName string) string
}

type daemonSetGenerator struct {
	client      client.Client
	kernelLabel string
	scheme      *runtime.Scheme
}

func NewCreator(client client.Client, kernelLabel string, scheme *runtime.Scheme) DaemonSetCreator {
	return &daemonSetGenerator{
		client:      client,
		kernelLabel: kernelLabel,
		scheme:      scheme,
	}
}

func (dc *daemonSetGenerator) GarbageCollect(ctx context.Context, existingDS map[string]*appsv1.DaemonSet, validKernels sets.String) ([]string, error) {
	deleted := make([]string, 0)

	for kernelVersion, ds := range existingDS {
		if !dc.isDevicePluginDaemonSet(ds) && !validKernels.Has(kernelVersion) {
			if err := dc.client.Delete(ctx, ds); err != nil {
				return nil, fmt.Errorf("could not delete DaemonSet %s: %v", ds.Name, err)
			}

			deleted = append(deleted, ds.Name)
		}
	}

	return deleted, nil
}

func (dc *daemonSetGenerator) ModuleDaemonSetsByKernelVersion(ctx context.Context, name, namespace string) (map[string]*appsv1.DaemonSet, error) {
	dsList, err := dc.moduleDaemonSets(ctx, name, namespace)
	if err != nil {
		return nil, fmt.Errorf("could not get all DaemonSets: %w", err)
	}

	dsByKernelVersion := make(map[string]*appsv1.DaemonSet, len(dsList))

	for i := 0; i < len(dsList); i++ {
		ds := dsList[i]

		kernelVersion := ds.Labels[dc.kernelLabel]
		if dsByKernelVersion[kernelVersion] != nil {
			return nil, fmt.Errorf("multiple DaemonSets found for kernel %q", kernelVersion)
		}

		dsByKernelVersion[kernelVersion] = &ds
	}

	return dsByKernelVersion, nil
}

func (dc *daemonSetGenerator) SetDriverContainerAsDesired(ctx context.Context, ds *appsv1.DaemonSet, image string, mod kmmv1beta1.Module, kernelVersion string) error {
	if ds == nil {
		return errors.New("ds cannot be nil")
	}

	if image == "" {
		return errors.New("image cannot be empty")
	}

	if kernelVersion == "" {
		return errors.New("kernelVersion cannot be empty")
	}

	standardLabels := map[string]string{
		constants.ModuleNameLabel: mod.Name,
		dc.kernelLabel:            kernelVersion,
		constants.DaemonSetRole:   "module-loader",
	}

	ds.SetLabels(
		OverrideLabels(ds.GetLabels(), standardLabels),
	)

	nodeSelector := CopyMapStringString(mod.Spec.Selector)
	nodeSelector[dc.kernelLabel] = kernelVersion

	hostPathDirectory := v1.HostPathDirectory
	hostPathDirectoryOrCreate := v1.HostPathDirectoryOrCreate

	container := v1.Container{
		Command:         []string{"sleep", "infinity"},
		Name:            "module-loader",
		Image:           image,
		ImagePullPolicy: mod.Spec.ModuleLoader.Container.ImagePullPolicy,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: MakeLoadCommand(mod.Spec.ModuleLoader.Container.Modprobe, mod.Name),
				},
			},
			PreStop: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: MakeUnloadCommand(mod.Spec.ModuleLoader.Container.Modprobe, mod.Name),
				},
			},
		},
		SecurityContext: &v1.SecurityContext{
			AllowPrivilegeEscalation: pointer.Bool(false),
			Capabilities: &v1.Capabilities{
				Add: []v1.Capability{"SYS_MODULE"},
			},
			RunAsUser: pointer.Int64(0),
			SELinuxOptions: &v1.SELinuxOptions{
				Type: "spc_t",
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      nodeLibModulesVolumeName,
				ReadOnly:  true,
				MountPath: nodeLibModulesPath,
			},
			{
				Name:      nodeUsrLibModulesVolumeName,
				ReadOnly:  true,
				MountPath: nodeUsrLibModulesPath,
			},
		},
	}

	volumes := []v1.Volume{
		{
			Name: nodeLibModulesVolumeName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: nodeLibModulesPath,
					Type: &hostPathDirectory,
				},
			},
		},
		{
			Name: nodeUsrLibModulesVolumeName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: nodeUsrLibModulesPath,
					Type: &hostPathDirectory,
				},
			},
		},
	}

	if fw := mod.Spec.ModuleLoader.Container.Modprobe.FirmwarePath; fw != "" {
		moduleFirmwarePath := fmt.Sprintf("%s/%s", nodeVarLibFirmwarePath, mod.Name)

		firmwareVolume := v1.Volume{
			Name: nodeVarLibFirmwareVolumeName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: moduleFirmwarePath,
					Type: &hostPathDirectoryOrCreate,
				},
			},
		}
		volumes = append(volumes, firmwareVolume)

		firmwareVolumeMount := v1.VolumeMount{
			Name:      nodeVarLibFirmwareVolumeName,
			MountPath: moduleFirmwarePath,
		}

		container.VolumeMounts = append(container.VolumeMounts, firmwareVolumeMount)
	}

	ds.Spec = appsv1.DaemonSetSpec{
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:     standardLabels,
				Finalizers: []string{constants.NodeLabelerFinalizer},
			},
			Spec: v1.PodSpec{
				Containers:         []v1.Container{container},
				ImagePullSecrets:   GetPodPullSecrets(mod.Spec.ImageRepoSecret),
				NodeSelector:       nodeSelector,
				PriorityClassName:  "system-node-critical",
				ServiceAccountName: mod.Spec.ModuleLoader.ServiceAccountName,
				Volumes:            volumes,
			},
		},
		Selector: &metav1.LabelSelector{MatchLabels: standardLabels},
	}

	return controllerutil.SetControllerReference(&mod, ds, dc.scheme)
}

func (dc *daemonSetGenerator) SetDevicePluginAsDesired(ctx context.Context, ds *appsv1.DaemonSet, mod *kmmv1beta1.Module) error {
	if ds == nil {
		return errors.New("ds cannot be nil")
	}

	if mod.Spec.DevicePlugin == nil {
		return errors.New("device plugin in module should not be nil")
	}

	containerVolumeMounts := []v1.VolumeMount{
		{
			Name:      kubeletDevicePluginsVolumeName,
			MountPath: kubeletDevicePluginsPath,
		},
	}

	hostPathDirectory := v1.HostPathDirectory

	devicePluginVolume := v1.Volume{
		Name: kubeletDevicePluginsVolumeName,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: kubeletDevicePluginsPath,
				Type: &hostPathDirectory,
			},
		},
	}

	standardLabels := map[string]string{
		constants.ModuleNameLabel: mod.Name,
		constants.DaemonSetRole:   "device-plugin",
	}

	ds.SetLabels(
		OverrideLabels(ds.GetLabels(), standardLabels),
	)

	ds.Spec = appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: standardLabels},
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:     standardLabels,
				Finalizers: []string{constants.NodeLabelerFinalizer},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Args:            mod.Spec.DevicePlugin.Container.Args,
						Command:         mod.Spec.DevicePlugin.Container.Command,
						Env:             mod.Spec.DevicePlugin.Container.Env,
						Name:            "device-plugin",
						Image:           mod.Spec.DevicePlugin.Container.Image,
						ImagePullPolicy: mod.Spec.DevicePlugin.Container.ImagePullPolicy,
						Resources:       mod.Spec.DevicePlugin.Container.Resources,
						SecurityContext: &v1.SecurityContext{Privileged: pointer.Bool(true)},
						VolumeMounts:    append(mod.Spec.DevicePlugin.Container.VolumeMounts, containerVolumeMounts...),
					},
				},
				PriorityClassName:  "system-node-critical",
				ImagePullSecrets:   GetPodPullSecrets(mod.Spec.ImageRepoSecret),
				NodeSelector:       map[string]string{getDriverContainerNodeLabel(mod.Name): ""},
				ServiceAccountName: mod.Spec.DevicePlugin.ServiceAccountName,
				Volumes:            append([]v1.Volume{devicePluginVolume}, mod.Spec.DevicePlugin.Volumes...),
			},
		},
	}

	return controllerutil.SetControllerReference(mod, ds, dc.scheme)
}

func (dc *daemonSetGenerator) GetNodeLabelFromPod(pod *v1.Pod, moduleName string) string {
	kernelVersion := pod.Labels[dc.kernelLabel]
	if kernelVersion == devicePluginKernelVersion {
		return getDevicePluginNodeLabel(moduleName)
	}
	return getDriverContainerNodeLabel(moduleName)
}

func (dc *daemonSetGenerator) moduleDaemonSets(ctx context.Context, name, namespace string) ([]appsv1.DaemonSet, error) {
	dsList := appsv1.DaemonSetList{}
	opts := []client.ListOption{
		client.MatchingLabels(map[string]string{constants.ModuleNameLabel: name}),
		client.InNamespace(namespace),
	}
	if err := dc.client.List(ctx, &dsList, opts...); err != nil {
		return nil, fmt.Errorf("could not list DaemonSets: %v", err)
	}
	return dsList.Items, nil
}

func (dc *daemonSetGenerator) isDevicePluginDaemonSet(ds *appsv1.DaemonSet) bool {
	return ds.Labels[dc.kernelLabel] == ""
}

// CopyMapStringString returns a deep copy of m.
func CopyMapStringString(m map[string]string) map[string]string {
	n := make(map[string]string, len(m))

	for k, v := range m {
		n[k] = v
	}

	return n
}

func getDriverContainerNodeLabel(moduleName string) string {
	return fmt.Sprintf("kmm.node.kubernetes.io/%s.ready", moduleName)
}

func getDevicePluginNodeLabel(moduleName string) string {
	return fmt.Sprintf("kmm.node.kubernetes.io/%s.device-plugin-ready", moduleName)
}

func IsDevicePluginKernelVersion(kernelVersion string) bool {
	return kernelVersion == devicePluginKernelVersion
}

func GetDevicePluginKernelVersion() string {
	return devicePluginKernelVersion
}

func GetPodPullSecrets(secret *v1.LocalObjectReference) []v1.LocalObjectReference {
	if secret == nil {
		return nil
	}

	return []v1.LocalObjectReference{*secret}
}

func OverrideLabels(labels, overrides map[string]string) map[string]string {
	if labels == nil {
		labels = make(map[string]string, len(overrides))
	}

	for k, v := range overrides {
		labels[k] = v
	}

	return labels
}

func MakeLoadCommand(spec kmmv1beta1.ModprobeSpec, modName string) []string {
	loadCommandShell := []string{
		"/bin/sh",
		"-c",
	}

	loadCommand := "modprobe"

	if ra := spec.RawArgs; ra != nil && len(ra.Load) > 0 {
		loadCommand = fmt.Sprintf("%s %s", loadCommand, strings.Join(ra.Load, " "))
		return append(loadCommandShell, loadCommand)
	}

	if fw := spec.FirmwarePath; fw != "" {
		loadCommand = fmt.Sprintf("cp -r %s %s/%s && %s", fw, nodeVarLibFirmwarePath, modName, loadCommand)
	}

	if a := spec.Args; a != nil && len(a.Load) > 0 {
		loadCommand = fmt.Sprintf("%s %s", loadCommand, strings.Join(a.Load, " "))
	} else {
		loadCommand = fmt.Sprintf("%s -v", loadCommand)
	}

	if dirName := spec.DirName; dirName != "" {
		loadCommand = fmt.Sprintf("%s -d %s", loadCommand, dirName)
	}

	loadCommand = fmt.Sprintf("%s %s", loadCommand, spec.ModuleName)

	if p := spec.Parameters; len(p) > 0 {
		loadCommand = fmt.Sprintf("%s %s", loadCommand, strings.Join(spec.Parameters, " "))
	}

	return append(loadCommandShell, loadCommand)
}

func MakeUnloadCommand(spec kmmv1beta1.ModprobeSpec, modName string) []string {
	unloadCommandShell := []string{
		"/bin/sh",
		"-c",
	}

	unloadCommand := "modprobe"

	if ra := spec.RawArgs; ra != nil && len(ra.Unload) > 0 {
		unloadCommand = fmt.Sprintf("%s %s", unloadCommand, strings.Join(ra.Unload, " "))
		return append(unloadCommandShell, unloadCommand)
	}

	if a := spec.Args; a != nil && len(a.Unload) > 0 {
		unloadCommand = fmt.Sprintf("%s %s", unloadCommand, strings.Join(a.Unload, " "))
	} else {
		unloadCommand = fmt.Sprintf("%s -rv", unloadCommand)
	}

	if dirName := spec.DirName; dirName != "" {
		unloadCommand = fmt.Sprintf("%s -d %s", unloadCommand, dirName)
	}

	unloadCommand = fmt.Sprintf("%s %s", unloadCommand, spec.ModuleName)

	if fw := spec.FirmwarePath; fw != "" {
		unloadCommand = fmt.Sprintf("%s && rm -rf %s/%s", unloadCommand, nodeVarLibFirmwarePath, modName)
	}

	return append(unloadCommandShell, unloadCommand)
}
