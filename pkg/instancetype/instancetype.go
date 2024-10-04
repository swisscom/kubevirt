//nolint:dupl,lll,gocyclo
package instancetype

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfield "k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/cache"

	virtv1 "kubevirt.io/api/core/v1"
	apiinstancetype "kubevirt.io/api/instancetype"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	"kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	"kubevirt.io/kubevirt/pkg/defaults"
	"kubevirt.io/kubevirt/pkg/instancetype/apply"
	"kubevirt.io/kubevirt/pkg/instancetype/find"
	preferenceApply "kubevirt.io/kubevirt/pkg/instancetype/preference/apply"
	preferenceFind "kubevirt.io/kubevirt/pkg/instancetype/preference/find"
	"kubevirt.io/kubevirt/pkg/instancetype/revision"
	"kubevirt.io/kubevirt/pkg/instancetype/upgrade"
	"kubevirt.io/kubevirt/pkg/network/vmispec"
	utils "kubevirt.io/kubevirt/pkg/util"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

const (
	VMFieldConflictErrorFmt                         = "VM field %s conflicts with selected instance type"
	VMFieldsConflictsErrorFmt                       = "VM fields %s conflict with selected instance type"
	InsufficientInstanceTypeCPUResourcesErrorFmt    = "insufficient CPU resources of %d vCPU provided by instance type, preference requires %d vCPU"
	InsufficientVMCPUResourcesErrorFmt              = "insufficient CPU resources of %d vCPU provided by VirtualMachine, preference requires %d vCPU provided as %s"
	InsufficientInstanceTypeMemoryResourcesErrorFmt = "insufficient Memory resources of %s provided by instance type, preference requires %s"
	InsufficientVMMemoryResourcesErrorFmt           = "insufficient Memory resources of %s provided by VirtualMachine, preference requires %s"
	NoVMCPUResourcesDefinedErrorFmt                 = "no CPU resources provided by VirtualMachine, preference requires %d vCPU"
	logVerbosityLevel                               = 3
)

type Methods interface {
	Upgrade(vm *virtv1.VirtualMachine) error
	FindInstancetypeSpec(vm *virtv1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error)
	ApplyToVmi(field *k8sfield.Path, instancetypespec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec, vmiMetadata *metav1.ObjectMeta) Conflicts
	FindPreferenceSpec(vm *virtv1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error)
	StoreControllerRevisions(vm *virtv1.VirtualMachine) error
	InferDefaultInstancetype(vm *virtv1.VirtualMachine) error
	InferDefaultPreference(vm *virtv1.VirtualMachine) error
	CheckPreferenceRequirements(instancetypeSpec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec) (Conflicts, error)
	ApplyToVM(vm *virtv1.VirtualMachine) error
	Expand(vm *virtv1.VirtualMachine, clusterConfig *virtconfig.ClusterConfig) (*virtv1.VirtualMachine, error)
}

type Conflicts apply.Conflicts

func (c Conflicts) String() string {
	pathStrings := make([]string, 0, len(c))
	for _, path := range c {
		pathStrings = append(pathStrings, path.String())
	}
	return strings.Join(pathStrings, ", ")
}

type InstancetypeMethods struct {
	InstancetypeStore        cache.Store
	ClusterInstancetypeStore cache.Store
	PreferenceStore          cache.Store
	ClusterPreferenceStore   cache.Store
	ControllerRevisionStore  cache.Store
	Clientset                kubecli.KubevirtClient
}

var _ Methods = &InstancetypeMethods{}

func (m *InstancetypeMethods) Expand(vm *virtv1.VirtualMachine, clusterConfig *virtconfig.ClusterConfig) (*virtv1.VirtualMachine, error) {
	if vm.Spec.Instancetype == nil && vm.Spec.Preference == nil {
		return vm, nil
	}

	instancetypeSpec, err := m.FindInstancetypeSpec(vm)
	if err != nil {
		return nil, err
	}
	preferenceSpec, err := m.FindPreferenceSpec(vm)
	if err != nil {
		return nil, err
	}
	expandedVM := vm.DeepCopy()

	utils.SetDefaultVolumeDisk(&expandedVM.Spec.Template.Spec)

	if err := vmispec.SetDefaultNetworkInterface(clusterConfig, &expandedVM.Spec.Template.Spec); err != nil {
		return nil, err
	}

	conflicts := m.ApplyToVmi(
		k8sfield.NewPath("spec", "template", "spec"),
		instancetypeSpec, preferenceSpec,
		&expandedVM.Spec.Template.Spec,
		&expandedVM.Spec.Template.ObjectMeta,
	)
	if len(conflicts) > 0 {
		return nil, fmt.Errorf(VMFieldsConflictsErrorFmt, conflicts.String())
	}

	// Apply defaults to VM.Spec.Template.Spec after applying instance types to ensure we don't conflict
	if err := defaults.SetDefaultVirtualMachineInstanceSpec(clusterConfig, &expandedVM.Spec.Template.Spec); err != nil {
		return nil, err
	}

	// Remove InstancetypeMatcher and PreferenceMatcher, so the returned VM object can be used and not cause a conflict
	expandedVM.Spec.Instancetype = nil
	expandedVM.Spec.Preference = nil

	return expandedVM, nil
}

func (m *InstancetypeMethods) ApplyToVM(vm *virtv1.VirtualMachine) error {
	instancetypeFinder := find.NewSpecFinder(m.InstancetypeStore, m.ClusterInstancetypeStore, m.ControllerRevisionStore, m.Clientset)
	preferenceFinder := preferenceFind.NewSpecFinder(m.PreferenceStore, m.ClusterPreferenceStore, m.ControllerRevisionStore, m.Clientset)
	return apply.NewVMApplier(instancetypeFinder, preferenceFinder).ApplyToVM(vm)
}

func GetPreferredTopology(preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec) instancetypev1beta1.PreferredCPUTopology {
	return preferenceApply.GetPreferredTopology(preferenceSpec)
}

func IsPreferredTopologySupported(topology instancetypev1beta1.PreferredCPUTopology) bool {
	supportedTopologies := []instancetypev1beta1.PreferredCPUTopology{
		instancetypev1beta1.DeprecatedPreferSockets,
		instancetypev1beta1.DeprecatedPreferCores,
		instancetypev1beta1.DeprecatedPreferThreads,
		instancetypev1beta1.DeprecatedPreferSpread,
		instancetypev1beta1.DeprecatedPreferAny,
		instancetypev1beta1.Sockets,
		instancetypev1beta1.Cores,
		instancetypev1beta1.Threads,
		instancetypev1beta1.Spread,
		instancetypev1beta1.Any,
	}
	return slices.Contains(supportedTopologies, topology)
}

func GetSpreadOptions(preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec) (uint32, instancetypev1beta1.SpreadAcross) {
	return preferenceApply.GetSpreadOptions(preferenceSpec)
}

func GetRevisionName(vmName, resourceName, resourceVersion string, resourceUID types.UID, resourceGeneration int64) string {
	return revision.GenerateName(vmName, resourceName, resourceVersion, resourceUID, resourceGeneration)
}

func CreateControllerRevision(vm *virtv1.VirtualMachine, object runtime.Object) (*appsv1.ControllerRevision, error) {
	return revision.CreateControllerRevision(vm, object)
}

func (m *InstancetypeMethods) StoreControllerRevisions(vm *virtv1.VirtualMachine) error {
	return revision.New(
		m.InstancetypeStore,
		m.ClusterInstancetypeStore,
		m.PreferenceStore,
		m.ClusterInstancetypeStore,
		m.Clientset).Store(vm)
}

func CompareRevisions(revisionA, revisionB *appsv1.ControllerRevision) (bool, error) {
	return revision.Compare(revisionA, revisionB)
}

func checkCPUPreferenceRequirements(instancetypeSpec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec) (Conflicts, error) {
	if instancetypeSpec != nil {
		if instancetypeSpec.CPU.Guest < preferenceSpec.Requirements.CPU.Guest {
			return Conflicts{k8sfield.NewPath("spec", "instancetype")}, fmt.Errorf(InsufficientInstanceTypeCPUResourcesErrorFmt, instancetypeSpec.CPU.Guest, preferenceSpec.Requirements.CPU.Guest)
		}
		return nil, nil
	}

	cpuField := k8sfield.NewPath("spec", "template", "spec", "domain", "cpu")
	if vmiSpec.Domain.CPU == nil {
		return Conflicts{cpuField}, fmt.Errorf(NoVMCPUResourcesDefinedErrorFmt, preferenceSpec.Requirements.CPU.Guest)
	}

	switch GetPreferredTopology(preferenceSpec) {
	case instancetypev1beta1.DeprecatedPreferThreads, instancetypev1beta1.Threads:
		if vmiSpec.Domain.CPU.Threads < preferenceSpec.Requirements.CPU.Guest {
			return Conflicts{cpuField.Child("threads")}, fmt.Errorf(InsufficientVMCPUResourcesErrorFmt, vmiSpec.Domain.CPU.Threads, preferenceSpec.Requirements.CPU.Guest, "threads")
		}
	case instancetypev1beta1.DeprecatedPreferCores, instancetypev1beta1.Cores:
		if vmiSpec.Domain.CPU.Cores < preferenceSpec.Requirements.CPU.Guest {
			return Conflicts{cpuField.Child("cores")}, fmt.Errorf(InsufficientVMCPUResourcesErrorFmt, vmiSpec.Domain.CPU.Cores, preferenceSpec.Requirements.CPU.Guest, "cores")
		}
	case instancetypev1beta1.DeprecatedPreferSockets, instancetypev1beta1.Sockets:
		if vmiSpec.Domain.CPU.Sockets < preferenceSpec.Requirements.CPU.Guest {
			return Conflicts{cpuField.Child("sockets")}, fmt.Errorf(InsufficientVMCPUResourcesErrorFmt, vmiSpec.Domain.CPU.Sockets, preferenceSpec.Requirements.CPU.Guest, "sockets")
		}
	case instancetypev1beta1.DeprecatedPreferSpread, instancetypev1beta1.Spread:
		var (
			vCPUs     uint32
			conflicts Conflicts
		)
		_, across := GetSpreadOptions(preferenceSpec)
		switch across {
		case instancetypev1beta1.SpreadAcrossSocketsCores:
			vCPUs = vmiSpec.Domain.CPU.Sockets * vmiSpec.Domain.CPU.Cores
			conflicts = Conflicts{cpuField.Child("sockets"), cpuField.Child("cores")}
		case instancetypev1beta1.SpreadAcrossCoresThreads:
			vCPUs = vmiSpec.Domain.CPU.Cores * vmiSpec.Domain.CPU.Threads
			conflicts = Conflicts{cpuField.Child("cores"), cpuField.Child("threads")}
		case instancetypev1beta1.SpreadAcrossSocketsCoresThreads:
			vCPUs = vmiSpec.Domain.CPU.Sockets * vmiSpec.Domain.CPU.Cores * vmiSpec.Domain.CPU.Threads
			conflicts = Conflicts{cpuField.Child("sockets"), cpuField.Child("cores"), cpuField.Child("threads")}
		}
		if vCPUs < preferenceSpec.Requirements.CPU.Guest {
			return conflicts, fmt.Errorf(InsufficientVMCPUResourcesErrorFmt, vCPUs, preferenceSpec.Requirements.CPU.Guest, across)
		}
	case instancetypev1beta1.DeprecatedPreferAny, instancetypev1beta1.Any:
		cpuResources := vmiSpec.Domain.CPU.Cores * vmiSpec.Domain.CPU.Sockets * vmiSpec.Domain.CPU.Threads
		if cpuResources < preferenceSpec.Requirements.CPU.Guest {
			return Conflicts{cpuField.Child("cores"), cpuField.Child("sockets"), cpuField.Child("threads")}, fmt.Errorf(InsufficientVMCPUResourcesErrorFmt, cpuResources, preferenceSpec.Requirements.CPU.Guest, "cores, sockets and threads")
		}
	}

	return nil, nil
}

func checkMemoryPreferenceRequirements(instancetypeSpec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec) (Conflicts, error) {
	if instancetypeSpec != nil && instancetypeSpec.Memory.Guest.Cmp(preferenceSpec.Requirements.Memory.Guest) < 0 {
		return Conflicts{k8sfield.NewPath("spec", "instancetype")}, fmt.Errorf(InsufficientInstanceTypeMemoryResourcesErrorFmt, instancetypeSpec.Memory.Guest.String(), preferenceSpec.Requirements.Memory.Guest.String())
	}

	if instancetypeSpec == nil && vmiSpec.Domain.Memory != nil && vmiSpec.Domain.Memory.Guest.Cmp(preferenceSpec.Requirements.Memory.Guest) < 0 {
		return Conflicts{k8sfield.NewPath("spec", "template", "spec", "domain", "memory")}, fmt.Errorf(InsufficientVMMemoryResourcesErrorFmt, vmiSpec.Domain.Memory.Guest.String(), preferenceSpec.Requirements.Memory.Guest.String())
	}
	return nil, nil
}

func (m *InstancetypeMethods) CheckPreferenceRequirements(instancetypeSpec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec) (Conflicts, error) {
	if preferenceSpec == nil || preferenceSpec.Requirements == nil {
		return nil, nil
	}

	if preferenceSpec.Requirements.CPU != nil {
		if conflicts, err := checkCPUPreferenceRequirements(instancetypeSpec, preferenceSpec, vmiSpec); err != nil {
			return conflicts, err
		}
	}

	if preferenceSpec.Requirements.Memory != nil {
		if conflicts, err := checkMemoryPreferenceRequirements(instancetypeSpec, preferenceSpec, vmiSpec); err != nil {
			return conflicts, err
		}
	}

	return nil, nil
}

func (m *InstancetypeMethods) ApplyToVmi(field *k8sfield.Path, instancetypeSpec *instancetypev1beta1.VirtualMachineInstancetypeSpec, preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec, vmiMetadata *metav1.ObjectMeta) Conflicts {
	conflicts := apply.NewVMIApplier().ApplyToVMI(field, instancetypeSpec, preferenceSpec, vmiSpec, vmiMetadata)
	return Conflicts(conflicts)
}

func (m *InstancetypeMethods) FindPreferenceSpec(vm *virtv1.VirtualMachine) (*instancetypev1beta1.VirtualMachinePreferenceSpec, error) {
	return preferenceFind.NewSpecFinder(m.PreferenceStore, m.ClusterPreferenceStore, m.ControllerRevisionStore, m.Clientset).Find(vm)
}

func (m *InstancetypeMethods) FindInstancetypeSpec(vm *virtv1.VirtualMachine) (*instancetypev1beta1.VirtualMachineInstancetypeSpec, error) {
	return find.NewSpecFinder(m.InstancetypeStore, m.ClusterInstancetypeStore, m.ControllerRevisionStore, m.Clientset).Find(vm)
}

func (m *InstancetypeMethods) InferDefaultInstancetype(vm *virtv1.VirtualMachine) error {
	if vm.Spec.Instancetype == nil {
		return nil
	}
	// Leave matcher unchanged when inference is disabled
	if vm.Spec.Instancetype.InferFromVolume == "" {
		return nil
	}

	ignoreFailure := vm.Spec.Instancetype.InferFromVolumeFailurePolicy != nil &&
		*vm.Spec.Instancetype.InferFromVolumeFailurePolicy == virtv1.IgnoreInferFromVolumeFailure

	defaultName, defaultKind, err := m.inferDefaultsFromVolumes(vm, vm.Spec.Instancetype.InferFromVolume, apiinstancetype.DefaultInstancetypeLabel, apiinstancetype.DefaultInstancetypeKindLabel)
	if err != nil {
		var ignoreableInferenceErr *IgnoreableInferenceError
		if errors.As(err, &ignoreableInferenceErr) && ignoreFailure {
			log.Log.Object(vm).V(logVerbosityLevel).Info("Ignored error during inference of instancetype, clearing matcher.")
			vm.Spec.Instancetype = nil
			return nil
		}

		return err
	}

	if ignoreFailure {
		vm.Spec.Template.Spec.Domain.Memory = nil
	}

	vm.Spec.Instancetype = &virtv1.InstancetypeMatcher{
		Name: defaultName,
		Kind: defaultKind,
	}
	return nil
}

func (m *InstancetypeMethods) InferDefaultPreference(vm *virtv1.VirtualMachine) error {
	if vm.Spec.Preference == nil {
		return nil
	}
	// Leave matcher unchanged when inference is disabled
	if vm.Spec.Preference.InferFromVolume == "" {
		return nil
	}

	defaultName, defaultKind, err := m.inferDefaultsFromVolumes(vm, vm.Spec.Preference.InferFromVolume, apiinstancetype.DefaultPreferenceLabel, apiinstancetype.DefaultPreferenceKindLabel)
	if err != nil {
		var ignoreableInferenceErr *IgnoreableInferenceError
		ignoreFailure := vm.Spec.Preference.InferFromVolumeFailurePolicy != nil &&
			*vm.Spec.Preference.InferFromVolumeFailurePolicy == virtv1.IgnoreInferFromVolumeFailure

		if errors.As(err, &ignoreableInferenceErr) && ignoreFailure {
			log.Log.Object(vm).V(logVerbosityLevel).Info("Ignored error during inference of preference, clearing matcher.")
			vm.Spec.Preference = nil
			return nil
		}

		return err
	}

	vm.Spec.Preference = &virtv1.PreferenceMatcher{
		Name: defaultName,
		Kind: defaultKind,
	}
	return nil
}

/*
Defaults will be inferred from the following combinations of DataVolumeSources, DataVolumeTemplates, DataSources and PVCs:

Volume -> PersistentVolumeClaimVolumeSource -> PersistentVolumeClaim
Volume -> DataVolumeSource -> DataVolume
Volume -> DataVolumeSource -> DataVolumeSourcePVC -> PersistentVolumeClaim
Volume -> DataVolumeSource -> DataVolumeSourceRef -> DataSource
Volume -> DataVolumeSource -> DataVolumeSourceRef -> DataSource -> PersistentVolumeClaim
Volume -> DataVolumeSource -> DataVolumeTemplate -> DataVolumeSourcePVC -> PersistentVolumeClaim
Volume -> DataVolumeSource -> DataVolumeTemplate -> DataVolumeSourceRef -> DataSource
Volume -> DataVolumeSource -> DataVolumeTemplate -> DataVolumeSourceRef -> DataSource -> PersistentVolumeClaim
*/
func (m *InstancetypeMethods) inferDefaultsFromVolumes(vm *virtv1.VirtualMachine, inferFromVolumeName, defaultNameLabel, defaultKindLabel string) (defaultName, defaultKind string, err error) {
	for _, volume := range vm.Spec.Template.Spec.Volumes {
		if volume.Name != inferFromVolumeName {
			continue
		}
		if volume.PersistentVolumeClaim != nil {
			return m.inferDefaultsFromPVC(volume.PersistentVolumeClaim.ClaimName, vm.Namespace, defaultNameLabel, defaultKindLabel)
		}
		if volume.DataVolume != nil {
			return m.inferDefaultsFromDataVolume(vm, volume.DataVolume.Name, defaultNameLabel, defaultKindLabel)
		}
		return "", "", NewIgnoreableInferenceError(fmt.Errorf("unable to infer defaults from volume %s as type is not supported", inferFromVolumeName))
	}
	return "", "", fmt.Errorf("unable to find volume %s to infer defaults", inferFromVolumeName)
}

func inferDefaultsFromLabels(labels map[string]string, defaultNameLabel, defaultKindLabel string) (defaultName, defaultKind string, err error) {
	defaultName, hasLabel := labels[defaultNameLabel]
	if !hasLabel {
		return "", "", NewIgnoreableInferenceError(fmt.Errorf("unable to find required %s label on the volume", defaultNameLabel))
	}
	return defaultName, labels[defaultKindLabel], nil
}

func (m *InstancetypeMethods) inferDefaultsFromPVC(pvcName, pvcNamespace, defaultNameLabel, defaultKindLabel string) (defaultName, defaultKind string, err error) {
	pvc, err := m.Clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(context.Background(), pvcName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	return inferDefaultsFromLabels(pvc.Labels, defaultNameLabel, defaultKindLabel)
}

func (m *InstancetypeMethods) inferDefaultsFromDataVolume(vm *virtv1.VirtualMachine, dvName, defaultNameLabel, defaultKindLabel string) (defaultName, defaultKind string, err error) {
	if len(vm.Spec.DataVolumeTemplates) > 0 {
		for _, dvt := range vm.Spec.DataVolumeTemplates {
			if dvt.Name != dvName {
				continue
			}
			dvtSpec := dvt.Spec
			return m.inferDefaultsFromDataVolumeSpec(&dvtSpec, defaultNameLabel, defaultKindLabel, vm.Namespace)
		}
	}
	dv, err := m.Clientset.CdiClient().CdiV1beta1().DataVolumes(vm.Namespace).Get(context.Background(), dvName, metav1.GetOptions{})
	if err != nil {
		// Handle garbage collected DataVolumes by attempting to lookup the PVC using the name of the DataVolume in the VM namespace
		if k8serrors.IsNotFound(err) {
			return m.inferDefaultsFromPVC(dvName, vm.Namespace, defaultNameLabel, defaultKindLabel)
		}
		return "", "", err
	}
	// Check the DataVolume for any labels before checking the underlying PVC
	defaultName, defaultKind, err = inferDefaultsFromLabels(dv.Labels, defaultNameLabel, defaultKindLabel)
	if err == nil {
		return defaultName, defaultKind, nil
	}
	return m.inferDefaultsFromDataVolumeSpec(&dv.Spec, defaultNameLabel, defaultKindLabel, vm.Namespace)
}

func (m *InstancetypeMethods) inferDefaultsFromDataVolumeSpec(dataVolumeSpec *v1beta1.DataVolumeSpec, defaultNameLabel, defaultKindLabel, vmNameSpace string) (defaultName, defaultKind string, err error) {
	if dataVolumeSpec != nil && dataVolumeSpec.Source != nil && dataVolumeSpec.Source.PVC != nil {
		return m.inferDefaultsFromPVC(dataVolumeSpec.Source.PVC.Name, dataVolumeSpec.Source.PVC.Namespace, defaultNameLabel, defaultKindLabel)
	}
	if dataVolumeSpec != nil && dataVolumeSpec.SourceRef != nil {
		return m.inferDefaultsFromDataVolumeSourceRef(dataVolumeSpec.SourceRef, defaultNameLabel, defaultKindLabel, vmNameSpace)
	}
	return "", "", NewIgnoreableInferenceError(fmt.Errorf("unable to infer defaults from DataVolumeSpec as DataVolumeSource is not supported"))
}

func (m *InstancetypeMethods) inferDefaultsFromDataSource(dataSourceName, dataSourceNamespace, defaultNameLabel, defaultKindLabel string) (defaultName, defaultKind string, err error) {
	ds, err := m.Clientset.CdiClient().CdiV1beta1().DataSources(dataSourceNamespace).Get(context.Background(), dataSourceName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	// Check the DataSource for any labels before checking the underlying PVC
	defaultName, defaultKind, err = inferDefaultsFromLabels(ds.Labels, defaultNameLabel, defaultKindLabel)
	if err == nil {
		return defaultName, defaultKind, nil
	}
	if ds.Spec.Source.PVC != nil {
		return m.inferDefaultsFromPVC(ds.Spec.Source.PVC.Name, ds.Spec.Source.PVC.Namespace, defaultNameLabel, defaultKindLabel)
	}
	return "", "", NewIgnoreableInferenceError(fmt.Errorf("unable to infer defaults from DataSource that doesn't provide DataVolumeSourcePVC"))
}

func (m *InstancetypeMethods) inferDefaultsFromDataVolumeSourceRef(sourceRef *v1beta1.DataVolumeSourceRef, defaultNameLabel, defaultKindLabel, vmNameSpace string) (defaultName, defaultKind string, err error) {
	if sourceRef.Kind == "DataSource" {
		// The namespace can be left blank here with the assumption that the DataSource is in the same namespace as the VM
		namespace := vmNameSpace
		if sourceRef.Namespace != nil {
			namespace = *sourceRef.Namespace
		}
		return m.inferDefaultsFromDataSource(sourceRef.Name, namespace, defaultNameLabel, defaultKindLabel)
	}
	return "", "", NewIgnoreableInferenceError(fmt.Errorf("unable to infer defaults from DataVolumeSourceRef as Kind %s is not supported", sourceRef.Kind))
}

func AddInstancetypeNameAnnotations(vm *virtv1.VirtualMachine, target metav1.Object) {
	if vm.Spec.Instancetype == nil {
		return
	}

	if target.GetAnnotations() == nil {
		target.SetAnnotations(make(map[string]string))
	}
	switch strings.ToLower(vm.Spec.Instancetype.Kind) {
	case apiinstancetype.PluralResourceName, apiinstancetype.SingularResourceName:
		target.GetAnnotations()[virtv1.InstancetypeAnnotation] = vm.Spec.Instancetype.Name
	case "", apiinstancetype.ClusterPluralResourceName, apiinstancetype.ClusterSingularResourceName:
		target.GetAnnotations()[virtv1.ClusterInstancetypeAnnotation] = vm.Spec.Instancetype.Name
	}
}

func AddPreferenceNameAnnotations(vm *virtv1.VirtualMachine, target metav1.Object) {
	if vm.Spec.Preference == nil {
		return
	}

	if target.GetAnnotations() == nil {
		target.SetAnnotations(make(map[string]string))
	}
	switch strings.ToLower(vm.Spec.Preference.Kind) {
	case apiinstancetype.PluralPreferenceResourceName, apiinstancetype.SingularPreferenceResourceName:
		target.GetAnnotations()[virtv1.PreferenceAnnotation] = vm.Spec.Preference.Name
	case "", apiinstancetype.ClusterPluralPreferenceResourceName, apiinstancetype.ClusterSingularPreferenceResourceName:
		target.GetAnnotations()[virtv1.ClusterPreferenceAnnotation] = vm.Spec.Preference.Name
	}
}

func ApplyDevicePreferences(preferenceSpec *instancetypev1beta1.VirtualMachinePreferenceSpec, vmiSpec *virtv1.VirtualMachineInstanceSpec) {
	preferenceApply.ApplyDevicePreferences(preferenceSpec, vmiSpec)
}

func (m *InstancetypeMethods) Upgrade(vm *virtv1.VirtualMachine) error {
	return upgrade.New(m.ControllerRevisionStore, m.Clientset).Upgrade(vm)
}

func IsObjectLatestVersion(cr *appsv1.ControllerRevision) bool {
	return upgrade.IsObjectLatestVersion(cr)
}
