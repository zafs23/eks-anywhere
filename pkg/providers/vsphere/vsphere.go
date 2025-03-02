package vsphere

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"reflect"
	"text/template"
	"time"

	"github.com/Masterminds/sprig"
	etcdv1 "github.com/aws/etcdadm-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"

	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/bootstrapper"
	"github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/config"
	"github.com/aws/eks-anywhere/pkg/constants"
	"github.com/aws/eks-anywhere/pkg/executables"
	"github.com/aws/eks-anywhere/pkg/filewriter"
	"github.com/aws/eks-anywhere/pkg/govmomi"
	"github.com/aws/eks-anywhere/pkg/logger"
	"github.com/aws/eks-anywhere/pkg/providers"
	"github.com/aws/eks-anywhere/pkg/providers/common"
	"github.com/aws/eks-anywhere/pkg/retrier"
	"github.com/aws/eks-anywhere/pkg/types"
	releasev1alpha1 "github.com/aws/eks-anywhere/release/api/v1alpha1"
)

const (
	CredentialsObjectName    = "vsphere-credentials"
	eksaLicense              = "EKSA_LICENSE"
	vSphereUsernameKey       = "VSPHERE_USERNAME"
	vSpherePasswordKey       = "VSPHERE_PASSWORD"
	vSphereServerKey         = "VSPHERE_SERVER"
	govcDatacenterKey        = "GOVC_DATACENTER"
	govcInsecure             = "GOVC_INSECURE"
	expClusterResourceSetKey = "EXP_CLUSTER_RESOURCE_SET"
	defaultTemplateLibrary   = "eks-a-templates"
	defaultTemplatesFolder   = "vm/Templates"
	maxRetries               = 30
	backOffPeriod            = 5 * time.Second
)

//go:embed config/template-cp.yaml
var defaultCAPIConfigCP string

//go:embed config/template-md.yaml
var defaultClusterConfigMD string

//go:embed config/secret.yaml
var defaultSecretObject string

//go:embed config/defaultStorageClass.yaml
var defaultStorageClass []byte

var (
	eksaVSphereDatacenterResourceType = fmt.Sprintf("vspheredatacenterconfigs.%s", v1alpha1.GroupVersion.Group)
	eksaVSphereMachineResourceType    = fmt.Sprintf("vspheremachineconfigs.%s", v1alpha1.GroupVersion.Group)
)

var requiredEnvs = []string{vSphereUsernameKey, vSpherePasswordKey, expClusterResourceSetKey}

type vsphereProvider struct {
	clusterConfig         *v1alpha1.Cluster
	providerGovcClient    ProviderGovcClient
	providerKubectlClient ProviderKubectlClient
	writer                filewriter.FileWriter
	templateBuilder       *VsphereTemplateBuilder
	skipIPCheck           bool
	csiEnabled            bool
	Retrier               *retrier.Retrier
	validator             *Validator
	defaulter             *Defaulter
	ipValidator           IPValidator
}

type ProviderGovcClient interface {
	SearchTemplate(ctx context.Context, datacenter, template string) (string, error)
	LibraryElementExists(ctx context.Context, library string) (bool, error)
	GetLibraryElementContentVersion(ctx context.Context, element string) (string, error)
	DeleteLibraryElement(ctx context.Context, element string) error
	TemplateHasSnapshot(ctx context.Context, template string) (bool, error)
	GetWorkloadAvailableSpace(ctx context.Context, datastore string) (float64, error)
	ValidateVCenterSetupMachineConfig(ctx context.Context, datacenterConfig *v1alpha1.VSphereDatacenterConfig, machineConfig *v1alpha1.VSphereMachineConfig, selfSigned *bool) error
	ValidateVCenterConnection(ctx context.Context, server string) error
	ValidateVCenterAuthentication(ctx context.Context) error
	IsCertSelfSigned(ctx context.Context) bool
	GetCertThumbprint(ctx context.Context) (string, error)
	ConfigureCertThumbprint(ctx context.Context, server, thumbprint string) error
	DatacenterExists(ctx context.Context, datacenter string) (bool, error)
	NetworkExists(ctx context.Context, network string) (bool, error)
	CreateLibrary(ctx context.Context, datastore, library string) error
	DeployTemplateFromLibrary(ctx context.Context, templateDir, templateName, library, datacenter, datastore, network, resourcePool string, resizeDisk2 bool) error
	ImportTemplate(ctx context.Context, library, ovaURL, name string) error
	GetVMDiskSizeInGB(ctx context.Context, vm, datacenter string) (int, error)
	GetTags(ctx context.Context, path string) (tags []string, err error)
	ListTags(ctx context.Context) ([]executables.Tag, error)
	CreateTag(ctx context.Context, tag, category string) error
	AddTag(ctx context.Context, path, tag string) error
	ListCategories(ctx context.Context) ([]string, error)
	CreateCategoryForVM(ctx context.Context, name string) error
	CreateUser(ctx context.Context, username string, password string) error
	UserExists(ctx context.Context, username string) (bool, error)
	CreateGroup(ctx context.Context, name string) error
	GroupExists(ctx context.Context, name string) (bool, error)
	AddUserToGroup(ctx context.Context, name string, username string) error
	RoleExists(ctx context.Context, name string) (bool, error)
	CreateRole(ctx context.Context, name string, privileges []string) error
	SetGroupRoleOnObject(ctx context.Context, principal string, role string, object string, domain string) error
}

type ProviderKubectlClient interface {
	ApplyKubeSpecFromBytes(ctx context.Context, cluster *types.Cluster, data []byte) error
	CreateNamespaceIfNotPresent(ctx context.Context, kubeconfig string, namespace string) error
	LoadSecret(ctx context.Context, secretObject string, secretObjType string, secretObjectName string, kubeConfFile string) error
	GetEksaCluster(ctx context.Context, cluster *types.Cluster, clusterName string) (*v1alpha1.Cluster, error)
	GetEksaVSphereDatacenterConfig(ctx context.Context, vsphereDatacenterConfigName string, kubeconfigFile string, namespace string) (*v1alpha1.VSphereDatacenterConfig, error)
	GetEksaVSphereMachineConfig(ctx context.Context, vsphereMachineConfigName string, kubeconfigFile string, namespace string) (*v1alpha1.VSphereMachineConfig, error)
	GetMachineDeployment(ctx context.Context, machineDeploymentName string, opts ...executables.KubectlOpt) (*clusterv1.MachineDeployment, error)
	GetKubeadmControlPlane(ctx context.Context, cluster *types.Cluster, clusterName string, opts ...executables.KubectlOpt) (*controlplanev1.KubeadmControlPlane, error)
	GetEtcdadmCluster(ctx context.Context, cluster *types.Cluster, clusterName string, opts ...executables.KubectlOpt) (*etcdv1.EtcdadmCluster, error)
	GetSecretFromNamespace(ctx context.Context, kubeconfigFile, name, namespace string) (*corev1.Secret, error)
	UpdateAnnotation(ctx context.Context, resourceType, objectName string, annotations map[string]string, opts ...executables.KubectlOpt) error
	RemoveAnnotationInNamespace(ctx context.Context, resourceType, objectName, key string, cluster *types.Cluster, namespace string) error
	SearchVsphereMachineConfig(ctx context.Context, name string, kubeconfigFile string, namespace string) ([]*v1alpha1.VSphereMachineConfig, error)
	SearchVsphereDatacenterConfig(ctx context.Context, name string, kubeconfigFile string, namespace string) ([]*v1alpha1.VSphereDatacenterConfig, error)
	SetDaemonSetImage(ctx context.Context, kubeconfigFile, name, namespace, container, image string) error
	DeleteEksaDatacenterConfig(ctx context.Context, vsphereDatacenterResourceType string, vsphereDatacenterConfigName string, kubeconfigFile string, namespace string) error
	DeleteEksaMachineConfig(ctx context.Context, vsphereMachineResourceType string, vsphereMachineConfigName string, kubeconfigFile string, namespace string) error
	ApplyTolerationsFromTaintsToDaemonSet(ctx context.Context, oldTaints []corev1.Taint, newTaints []corev1.Taint, dsName string, kubeconfigFile string) error
}

// IPValidator is an interface that defines methods to validate the control plane IP.
type IPValidator interface {
	ValidateControlPlaneIPUniqueness(cluster *v1alpha1.Cluster) error
}

func NewProvider(datacenterConfig *v1alpha1.VSphereDatacenterConfig, clusterConfig *v1alpha1.Cluster, providerGovcClient ProviderGovcClient, providerKubectlClient ProviderKubectlClient, writer filewriter.FileWriter, ipValidator IPValidator, now types.NowFunc, skipIpCheck bool) *vsphereProvider { //nolint:revive
	// TODO(g-gaston): ignoring linter error for exported function returning unexported member
	// We should make it exported, but that would involve a bunch of changes, so will do it separately
	vcb := govmomi.NewVMOMIClientBuilder()
	v := NewValidator(
		providerGovcClient,
		vcb,
	)

	return NewProviderCustomNet(
		datacenterConfig,
		clusterConfig,
		providerGovcClient,
		providerKubectlClient,
		writer,
		ipValidator,
		now,
		skipIpCheck,
		v,
	)
}

func NewProviderCustomNet(datacenterConfig *v1alpha1.VSphereDatacenterConfig, clusterConfig *v1alpha1.Cluster, providerGovcClient ProviderGovcClient, providerKubectlClient ProviderKubectlClient, writer filewriter.FileWriter, ipValidator IPValidator, now types.NowFunc, skipIpCheck bool, v *Validator) *vsphereProvider { //nolint:revive
	// TODO(g-gaston): ignoring linter error for exported function returning unexported member
	// We should make it exported, but that would involve a bunch of changes, so will do it separately
	retrier := retrier.NewWithMaxRetries(maxRetries, backOffPeriod)
	return &vsphereProvider{
		clusterConfig:         clusterConfig,
		providerGovcClient:    providerGovcClient,
		providerKubectlClient: providerKubectlClient,
		writer:                writer,
		templateBuilder: NewVsphereTemplateBuilder(
			now,
		),
		skipIPCheck: skipIpCheck,
		csiEnabled:  !datacenterConfig.Spec.DisableCSI,
		Retrier:     retrier,
		validator:   v,
		defaulter:   NewDefaulter(providerGovcClient),
		ipValidator: ipValidator,
	}
}

func (p *vsphereProvider) UpdateKubeConfig(_ *[]byte, _ string) error {
	// customize generated kube config
	return nil
}

func (p *vsphereProvider) machineConfigsSpecChanged(ctx context.Context, cc *v1alpha1.Cluster, cluster *types.Cluster, newClusterSpec *cluster.Spec) (bool, error) {
	for _, oldMcRef := range cc.MachineConfigRefs() {
		existingVmc, err := p.providerKubectlClient.GetEksaVSphereMachineConfig(ctx, oldMcRef.Name, cluster.KubeconfigFile, newClusterSpec.Cluster.Namespace)
		if err != nil {
			return false, err
		}
		csmc, ok := newClusterSpec.VSphereMachineConfigs[oldMcRef.Name]
		if !ok {
			logger.V(3).Info(fmt.Sprintf("Old machine config spec %s not found in the existing spec", oldMcRef.Name))
			return true, nil
		}
		if !reflect.DeepEqual(existingVmc.Spec, csmc.Spec) {
			logger.V(3).Info(fmt.Sprintf("New machine config spec %s is different from the existing spec", oldMcRef.Name))
			return true, nil
		}
	}

	return false, nil
}

func (p *vsphereProvider) BootstrapClusterOpts(spec *cluster.Spec) ([]bootstrapper.BootstrapClusterOption, error) {
	return common.BootstrapClusterOpts(p.clusterConfig, spec.VSphereDatacenter.Spec.Server)
}

func (p *vsphereProvider) Name() string {
	return constants.VSphereProviderName
}

func (p *vsphereProvider) DatacenterResourceType() string {
	return eksaVSphereDatacenterResourceType
}

func (p *vsphereProvider) MachineResourceType() string {
	return eksaVSphereMachineResourceType
}

func (p *vsphereProvider) generateSSHKeysIfNotSet(machineConfigs map[string]*v1alpha1.VSphereMachineConfig) error {
	var generatedKey string
	for _, machineConfig := range machineConfigs {
		user := machineConfig.Spec.Users[0]
		if user.SshAuthorizedKeys[0] == "" {
			if generatedKey != "" { // use the same key
				user.SshAuthorizedKeys[0] = generatedKey
			} else {
				logger.Info("Provided sshAuthorizedKey is not set or is empty, auto-generating new key pair...", "vSphereMachineConfig", machineConfig.Name)
				var err error
				generatedKey, err = common.GenerateSSHAuthKey(p.writer)
				if err != nil {
					return err
				}
				user.SshAuthorizedKeys[0] = generatedKey
			}
		}
	}

	return nil
}

func (p *vsphereProvider) DeleteResources(ctx context.Context, clusterSpec *cluster.Spec) error {
	for _, mc := range clusterSpec.VSphereMachineConfigs {
		if err := p.providerKubectlClient.DeleteEksaMachineConfig(ctx, eksaVSphereMachineResourceType, mc.Name, clusterSpec.ManagementCluster.KubeconfigFile, mc.Namespace); err != nil {
			return err
		}
	}
	return p.providerKubectlClient.DeleteEksaDatacenterConfig(ctx,
		eksaVSphereDatacenterResourceType,
		clusterSpec.VSphereDatacenter.Name,
		clusterSpec.ManagementCluster.KubeconfigFile,
		clusterSpec.VSphereDatacenter.Namespace,
	)
}

func (p *vsphereProvider) PostClusterDeleteValidate(_ context.Context, _ *types.Cluster) error {
	// No validations
	return nil
}

func (p *vsphereProvider) PostMoveManagementToBootstrap(_ context.Context, _ *types.Cluster) error {
	// NOOP
	return nil
}

func (p *vsphereProvider) SetupAndValidateCreateCluster(ctx context.Context, clusterSpec *cluster.Spec) error {
	if err := p.validator.validateUpgradeRolloutStrategy(clusterSpec); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}
	if err := SetupEnvVars(clusterSpec.VSphereDatacenter); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}

	vSphereClusterSpec := NewSpec(clusterSpec)

	if err := p.defaulter.SetDefaultsForDatacenterConfig(ctx, vSphereClusterSpec.VSphereDatacenter); err != nil {
		return fmt.Errorf("failed setting default values for vsphere datacenter config: %v", err)
	}

	if err := vSphereClusterSpec.VSphereDatacenter.Validate(); err != nil {
		return err
	}

	if err := p.validator.ValidateVCenterConfig(ctx, vSphereClusterSpec.VSphereDatacenter); err != nil {
		return err
	}

	if err := p.defaulter.setDefaultsForMachineConfig(ctx, vSphereClusterSpec); err != nil {
		return fmt.Errorf("failed setting default values for vsphere machine configs: %v", err)
	}

	if err := p.validator.ValidateClusterMachineConfigs(ctx, vSphereClusterSpec); err != nil {
		return err
	}

	if err := p.generateSSHKeysIfNotSet(clusterSpec.VSphereMachineConfigs); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}

	// TODO: move this to validator
	if clusterSpec.Cluster.IsManaged() {
		for _, mc := range clusterSpec.VSphereMachineConfigs {
			em, err := p.providerKubectlClient.SearchVsphereMachineConfig(ctx, mc.GetName(), clusterSpec.ManagementCluster.KubeconfigFile, mc.GetNamespace())
			if err != nil {
				return err
			}
			if len(em) > 0 {
				return fmt.Errorf("VSphereMachineConfig %s already exists", mc.GetName())
			}
		}
		existingDatacenter, err := p.providerKubectlClient.SearchVsphereDatacenterConfig(ctx, clusterSpec.VSphereDatacenter.Name, clusterSpec.ManagementCluster.KubeconfigFile, clusterSpec.Cluster.Namespace)
		if err != nil {
			return err
		}
		if len(existingDatacenter) > 0 {
			return fmt.Errorf("VSphereDatacenter %s already exists", clusterSpec.VSphereDatacenter.Name)
		}
		for _, identityProviderRef := range clusterSpec.Cluster.Spec.IdentityProviderRefs {
			if identityProviderRef.Kind == v1alpha1.OIDCConfigKind {
				clusterSpec.OIDCConfig.SetManagedBy(p.clusterConfig.ManagedBy())
			}
		}
	}

	if !p.skipIPCheck {
		if err := p.ipValidator.ValidateControlPlaneIPUniqueness(clusterSpec.Cluster); err != nil {
			return err
		}
	} else {
		logger.Info("Skipping check for whether control plane ip is in use")
	}

	var passed bool
	var err error
	vuc := config.NewVsphereUserConfig()

	if passed, err = p.validator.validateUserPrivs(ctx, vSphereClusterSpec, vuc); err != nil {
		return err
	} else if passed {
		s := fmt.Sprintf("%s user vSphere privileges validated", vuc.EksaVsphereUsername)
		logger.MarkPass(s)
	}

	if len(vuc.EksaVsphereCPUsername) > 0 && vuc.EksaVsphereCPUsername != vuc.EksaVsphereUsername {
		if passed, err = p.validator.validateCPUserPrivs(ctx, vSphereClusterSpec, vuc); err != nil {
			return err
		} else if passed {
			s := fmt.Sprintf("%s user vSphere privileges validated", vuc.EksaVsphereCPUsername)
			logger.MarkPass(s)
		}
	}

	if len(vuc.EksaVsphereCSIUsername) > 0 && vuc.EksaVsphereCSIUsername != vuc.EksaVsphereUsername {
		if passed, err = p.validator.validateCSIUserPrivs(ctx, vSphereClusterSpec, vuc); err != nil {
			return err
		} else if passed {
			s := fmt.Sprintf("%s user vSphere privileges validated", vuc.EksaVsphereCSIUsername)
			logger.MarkPass(s)
		}
	}

	return nil
}

func (p *vsphereProvider) SetupAndValidateUpgradeCluster(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec, _ *cluster.Spec) error {
	if err := p.validator.validateUpgradeRolloutStrategy(clusterSpec); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}
	if err := SetupEnvVars(clusterSpec.VSphereDatacenter); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}

	vSphereClusterSpec := NewSpec(clusterSpec)

	if err := p.defaulter.SetDefaultsForDatacenterConfig(ctx, vSphereClusterSpec.VSphereDatacenter); err != nil {
		return fmt.Errorf("failed setting default values for vsphere datacenter config: %v", err)
	}

	if err := vSphereClusterSpec.VSphereDatacenter.Validate(); err != nil {
		return err
	}

	if err := p.validator.ValidateVCenterConfig(ctx, vSphereClusterSpec.VSphereDatacenter); err != nil {
		return err
	}

	if err := p.defaulter.setDefaultsForMachineConfig(ctx, vSphereClusterSpec); err != nil {
		return fmt.Errorf("failed setting default values for vsphere machine configs: %v", err)
	}

	if err := p.validator.ValidateClusterMachineConfigs(ctx, vSphereClusterSpec); err != nil {
		return err
	}

	err := p.validateMachineConfigsNameUniqueness(ctx, cluster, clusterSpec)
	if err != nil {
		return fmt.Errorf("failed validate machineconfig uniqueness: %v", err)
	}
	return nil
}

func (p *vsphereProvider) validateMachineConfigsNameUniqueness(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	prevSpec, err := p.providerKubectlClient.GetEksaCluster(ctx, cluster, clusterSpec.Cluster.GetName())
	if err != nil {
		return err
	}

	cpMachineConfigName := clusterSpec.Cluster.Spec.ControlPlaneConfiguration.MachineGroupRef.Name
	if prevSpec.Spec.ControlPlaneConfiguration.MachineGroupRef.Name != cpMachineConfigName {
		em, err := p.providerKubectlClient.SearchVsphereMachineConfig(ctx, cpMachineConfigName, cluster.KubeconfigFile, clusterSpec.Cluster.GetNamespace())
		if err != nil {
			return err
		}
		if len(em) > 0 {
			return fmt.Errorf("control plane VSphereMachineConfig %s already exists", cpMachineConfigName)
		}
	}

	if clusterSpec.Cluster.Spec.ExternalEtcdConfiguration != nil && prevSpec.Spec.ExternalEtcdConfiguration != nil {
		etcdMachineConfigName := clusterSpec.Cluster.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name
		if prevSpec.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name != etcdMachineConfigName {
			em, err := p.providerKubectlClient.SearchVsphereMachineConfig(ctx, etcdMachineConfigName, clusterSpec.ManagementCluster.KubeconfigFile, clusterSpec.Cluster.GetNamespace())
			if err != nil {
				return err
			}
			if len(em) > 0 {
				return fmt.Errorf("external etcd machineconfig %s already exists", etcdMachineConfigName)
			}
		}
	}

	return nil
}

func (p *vsphereProvider) UpdateSecrets(ctx context.Context, cluster *types.Cluster, _ *cluster.Spec) error {
	var contents bytes.Buffer
	err := p.createSecret(ctx, cluster, &contents)
	if err != nil {
		return err
	}

	err = p.providerKubectlClient.ApplyKubeSpecFromBytes(ctx, cluster, contents.Bytes())
	if err != nil {
		return fmt.Errorf("loading secrets object: %v", err)
	}
	return nil
}

func (p *vsphereProvider) SetupAndValidateDeleteCluster(ctx context.Context, _ *types.Cluster, spec *cluster.Spec) error {
	if err := p.validator.validateUpgradeRolloutStrategy(spec); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}
	if err := SetupEnvVars(spec.VSphereDatacenter); err != nil {
		return fmt.Errorf("failed setup and validations: %v", err)
	}
	return nil
}

func NeedsNewControlPlaneTemplate(oldSpec, newSpec *cluster.Spec, oldVdc, newVdc *v1alpha1.VSphereDatacenterConfig, oldVmc, newVmc *v1alpha1.VSphereMachineConfig) bool {
	// Another option is to generate MachineTemplates based on the old and new eksa spec,
	// remove the name field and compare them with DeepEqual
	// We plan to approach this way since it's more flexible to add/remove fields and test out for validation
	if oldSpec.Cluster.Spec.KubernetesVersion != newSpec.Cluster.Spec.KubernetesVersion {
		return true
	}
	if oldSpec.Cluster.Spec.ControlPlaneConfiguration.Endpoint.Host != newSpec.Cluster.Spec.ControlPlaneConfiguration.Endpoint.Host {
		return true
	}
	if oldSpec.Bundles.Spec.Number != newSpec.Bundles.Spec.Number {
		return true
	}
	return AnyImmutableFieldChanged(oldVdc, newVdc, oldVmc, newVmc)
}

func NeedsNewWorkloadTemplate(oldSpec, newSpec *cluster.Spec, oldVdc, newVdc *v1alpha1.VSphereDatacenterConfig, oldVmc, newVmc *v1alpha1.VSphereMachineConfig) bool {
	if oldSpec.Cluster.Spec.KubernetesVersion != newSpec.Cluster.Spec.KubernetesVersion {
		return true
	}
	if oldSpec.Bundles.Spec.Number != newSpec.Bundles.Spec.Number {
		return true
	}
	if !v1alpha1.WorkerNodeGroupConfigurationSliceTaintsEqual(oldSpec.Cluster.Spec.WorkerNodeGroupConfigurations, newSpec.Cluster.Spec.WorkerNodeGroupConfigurations) ||
		!v1alpha1.WorkerNodeGroupConfigurationsLabelsMapEqual(oldSpec.Cluster.Spec.WorkerNodeGroupConfigurations, newSpec.Cluster.Spec.WorkerNodeGroupConfigurations) {
		return true
	}
	return AnyImmutableFieldChanged(oldVdc, newVdc, oldVmc, newVmc)
}

func NeedsNewKubeadmConfigTemplate(newWorkerNodeGroup *v1alpha1.WorkerNodeGroupConfiguration, oldWorkerNodeGroup *v1alpha1.WorkerNodeGroupConfiguration, oldWorkerNodeVmc *v1alpha1.VSphereMachineConfig, newWorkerNodeVmc *v1alpha1.VSphereMachineConfig) bool {
	return !v1alpha1.TaintsSliceEqual(newWorkerNodeGroup.Taints, oldWorkerNodeGroup.Taints) || !v1alpha1.MapEqual(newWorkerNodeGroup.Labels, oldWorkerNodeGroup.Labels) ||
		!v1alpha1.UsersSliceEqual(oldWorkerNodeVmc.Spec.Users, newWorkerNodeVmc.Spec.Users)
}

func NeedsNewEtcdTemplate(oldSpec, newSpec *cluster.Spec, oldVdc, newVdc *v1alpha1.VSphereDatacenterConfig, oldVmc, newVmc *v1alpha1.VSphereMachineConfig) bool {
	if oldSpec.Cluster.Spec.KubernetesVersion != newSpec.Cluster.Spec.KubernetesVersion {
		return true
	}
	if oldSpec.Bundles.Spec.Number != newSpec.Bundles.Spec.Number {
		return true
	}
	return AnyImmutableFieldChanged(oldVdc, newVdc, oldVmc, newVmc)
}

func AnyImmutableFieldChanged(oldVdc, newVdc *v1alpha1.VSphereDatacenterConfig, oldVmc, newVmc *v1alpha1.VSphereMachineConfig) bool {
	if oldVmc.Spec.NumCPUs != newVmc.Spec.NumCPUs {
		return true
	}
	if oldVmc.Spec.MemoryMiB != newVmc.Spec.MemoryMiB {
		return true
	}
	if oldVmc.Spec.DiskGiB != newVmc.Spec.DiskGiB {
		return true
	}
	if oldVmc.Spec.Datastore != newVmc.Spec.Datastore {
		return true
	}
	if oldVmc.Spec.Folder != newVmc.Spec.Folder {
		return true
	}
	if oldVdc.Spec.Network != newVdc.Spec.Network {
		return true
	}
	if oldVmc.Spec.ResourcePool != newVmc.Spec.ResourcePool {
		return true
	}
	if oldVdc.Spec.Thumbprint != newVdc.Spec.Thumbprint {
		return true
	}
	if oldVmc.Spec.Template != newVmc.Spec.Template {
		return true
	}
	return false
}

func (p *vsphereProvider) generateCAPISpecForUpgrade(ctx context.Context, bootstrapCluster, workloadCluster *types.Cluster, currentSpec, newClusterSpec *cluster.Spec) (controlPlaneSpec, workersSpec []byte, err error) {
	clusterName := newClusterSpec.Cluster.Name
	var controlPlaneTemplateName, workloadTemplateName, kubeadmconfigTemplateName, etcdTemplateName string
	var needsNewEtcdTemplate bool

	c, err := p.providerKubectlClient.GetEksaCluster(ctx, workloadCluster, newClusterSpec.Cluster.Name)
	if err != nil {
		return nil, nil, err
	}
	vdc, err := p.providerKubectlClient.GetEksaVSphereDatacenterConfig(ctx, newClusterSpec.VSphereDatacenter.Name, workloadCluster.KubeconfigFile, newClusterSpec.Cluster.Namespace)
	if err != nil {
		return nil, nil, err
	}
	controlPlaneMachineConfig := newClusterSpec.VSphereMachineConfigs[newClusterSpec.Cluster.Spec.ControlPlaneConfiguration.MachineGroupRef.Name]
	controlPlaneVmc, err := p.providerKubectlClient.GetEksaVSphereMachineConfig(ctx, c.Spec.ControlPlaneConfiguration.MachineGroupRef.Name, workloadCluster.KubeconfigFile, newClusterSpec.Cluster.Namespace)
	if err != nil {
		return nil, nil, err
	}
	needsNewControlPlaneTemplate := NeedsNewControlPlaneTemplate(currentSpec, newClusterSpec, vdc, newClusterSpec.VSphereDatacenter, controlPlaneVmc, controlPlaneMachineConfig)
	if !needsNewControlPlaneTemplate {
		cp, err := p.providerKubectlClient.GetKubeadmControlPlane(ctx, workloadCluster, c.Name, executables.WithCluster(bootstrapCluster), executables.WithNamespace(constants.EksaSystemNamespace))
		if err != nil {
			return nil, nil, err
		}
		controlPlaneTemplateName = cp.Spec.MachineTemplate.InfrastructureRef.Name
	} else {
		controlPlaneTemplateName = common.CPMachineTemplateName(clusterName, p.templateBuilder.now)
	}

	previousWorkerNodeGroupConfigs := cluster.BuildMapForWorkerNodeGroupsByName(currentSpec.Cluster.Spec.WorkerNodeGroupConfigurations)

	workloadTemplateNames := make(map[string]string, len(newClusterSpec.Cluster.Spec.WorkerNodeGroupConfigurations))
	kubeadmconfigTemplateNames := make(map[string]string, len(newClusterSpec.Cluster.Spec.WorkerNodeGroupConfigurations))
	for _, workerNodeGroupConfiguration := range newClusterSpec.Cluster.Spec.WorkerNodeGroupConfigurations {

		oldWorkerNodeVmc, newWorkerNodeVmc, err := p.getWorkerNodeMachineConfigs(ctx, workloadCluster, newClusterSpec, workerNodeGroupConfiguration, previousWorkerNodeGroupConfigs)
		if err != nil {
			return nil, nil, err
		}
		needsNewWorkloadTemplate, err := p.needsNewMachineTemplate(currentSpec, newClusterSpec, workerNodeGroupConfiguration, vdc, previousWorkerNodeGroupConfigs, oldWorkerNodeVmc, newWorkerNodeVmc)
		if err != nil {
			return nil, nil, err
		}
		needsNewKubeadmConfigTemplate, err := p.needsNewKubeadmConfigTemplate(workerNodeGroupConfiguration, previousWorkerNodeGroupConfigs, oldWorkerNodeVmc, newWorkerNodeVmc)
		if err != nil {
			return nil, nil, err
		}
		if !needsNewKubeadmConfigTemplate {
			mdName := machineDeploymentName(newClusterSpec.Cluster.Name, workerNodeGroupConfiguration.Name)
			md, err := p.providerKubectlClient.GetMachineDeployment(ctx, mdName, executables.WithCluster(bootstrapCluster), executables.WithNamespace(constants.EksaSystemNamespace))
			if err != nil {
				return nil, nil, err
			}
			kubeadmconfigTemplateName = md.Spec.Template.Spec.Bootstrap.ConfigRef.Name
			kubeadmconfigTemplateNames[workerNodeGroupConfiguration.Name] = kubeadmconfigTemplateName
		} else {
			kubeadmconfigTemplateName = common.KubeadmConfigTemplateName(clusterName, workerNodeGroupConfiguration.Name, p.templateBuilder.now)
			kubeadmconfigTemplateNames[workerNodeGroupConfiguration.Name] = kubeadmconfigTemplateName
		}

		if !needsNewWorkloadTemplate {
			mdName := machineDeploymentName(newClusterSpec.Cluster.Name, workerNodeGroupConfiguration.Name)
			md, err := p.providerKubectlClient.GetMachineDeployment(ctx, mdName, executables.WithCluster(bootstrapCluster), executables.WithNamespace(constants.EksaSystemNamespace))
			if err != nil {
				return nil, nil, err
			}
			workloadTemplateName = md.Spec.Template.Spec.InfrastructureRef.Name
			workloadTemplateNames[workerNodeGroupConfiguration.Name] = workloadTemplateName
		} else {
			workloadTemplateName = common.WorkerMachineTemplateName(clusterName, workerNodeGroupConfiguration.Name, p.templateBuilder.now)
			workloadTemplateNames[workerNodeGroupConfiguration.Name] = workloadTemplateName
		}
	}

	if newClusterSpec.Cluster.Spec.ExternalEtcdConfiguration != nil {
		etcdMachineConfig := newClusterSpec.VSphereMachineConfigs[newClusterSpec.Cluster.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name]
		etcdMachineVmc, err := p.providerKubectlClient.GetEksaVSphereMachineConfig(ctx, c.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name, workloadCluster.KubeconfigFile, newClusterSpec.Cluster.Namespace)
		if err != nil {
			return nil, nil, err
		}
		needsNewEtcdTemplate = NeedsNewEtcdTemplate(currentSpec, newClusterSpec, vdc, newClusterSpec.VSphereDatacenter, etcdMachineVmc, etcdMachineConfig)
		if !needsNewEtcdTemplate {
			etcdadmCluster, err := p.providerKubectlClient.GetEtcdadmCluster(ctx, workloadCluster, clusterName, executables.WithCluster(bootstrapCluster), executables.WithNamespace(constants.EksaSystemNamespace))
			if err != nil {
				return nil, nil, err
			}
			etcdTemplateName = etcdadmCluster.Spec.InfrastructureTemplate.Name
		} else {
			/* During a cluster upgrade, etcd machines need to be upgraded first, so that the etcd machines with new spec get created and can be used by controlplane machines
			as etcd endpoints. KCP rollout should not start until then. As a temporary solution in the absence of static etcd endpoints, we annotate the etcd cluster as "upgrading",
			so that KCP checks this annotation and does not proceed if etcd cluster is upgrading. The etcdadm controller removes this annotation once the etcd upgrade is complete.
			*/
			err = p.providerKubectlClient.UpdateAnnotation(ctx, "etcdadmcluster", fmt.Sprintf("%s-etcd", clusterName),
				map[string]string{etcdv1.UpgradeInProgressAnnotation: "true"},
				executables.WithCluster(bootstrapCluster),
				executables.WithNamespace(constants.EksaSystemNamespace))
			if err != nil {
				return nil, nil, err
			}
			etcdTemplateName = common.EtcdMachineTemplateName(clusterName, p.templateBuilder.now)
		}
	}

	cpOpt := func(values map[string]interface{}) {
		values["controlPlaneTemplateName"] = controlPlaneTemplateName
		values["etcdTemplateName"] = etcdTemplateName
	}
	controlPlaneSpec, err = p.templateBuilder.GenerateCAPISpecControlPlane(newClusterSpec, cpOpt)
	if err != nil {
		return nil, nil, err
	}

	workersSpec, err = p.templateBuilder.GenerateCAPISpecWorkers(newClusterSpec, workloadTemplateNames, kubeadmconfigTemplateNames)
	if err != nil {
		return nil, nil, err
	}
	return controlPlaneSpec, workersSpec, nil
}

func (p *vsphereProvider) generateCAPISpecForCreate(ctx context.Context, clusterSpec *cluster.Spec) (controlPlaneSpec, workersSpec []byte, err error) {
	clusterName := clusterSpec.Cluster.Name

	cpOpt := func(values map[string]interface{}) {
		values["controlPlaneTemplateName"] = common.CPMachineTemplateName(clusterName, p.templateBuilder.now)
		values["etcdTemplateName"] = common.EtcdMachineTemplateName(clusterName, p.templateBuilder.now)
	}
	controlPlaneSpec, err = p.templateBuilder.GenerateCAPISpecControlPlane(clusterSpec, cpOpt)
	if err != nil {
		return nil, nil, err
	}

	// TODO(g-gaston): update this to use the new method CAPIWorkersSpecWithInitialNames.
	// That implies moving to monotonically increasing names instead of based on timestamp.
	// Upgrades should also be moved to that naming scheme for consistency. That requires bigger changes.
	workloadTemplateNames := make(map[string]string, len(clusterSpec.Cluster.Spec.WorkerNodeGroupConfigurations))
	kubeadmconfigTemplateNames := make(map[string]string, len(clusterSpec.Cluster.Spec.WorkerNodeGroupConfigurations))
	for _, workerNodeGroupConfiguration := range clusterSpec.Cluster.Spec.WorkerNodeGroupConfigurations {
		workloadTemplateNames[workerNodeGroupConfiguration.Name] = common.WorkerMachineTemplateName(clusterSpec.Cluster.Name, workerNodeGroupConfiguration.Name, p.templateBuilder.now)
		kubeadmconfigTemplateNames[workerNodeGroupConfiguration.Name] = common.KubeadmConfigTemplateName(clusterSpec.Cluster.Name, workerNodeGroupConfiguration.Name, p.templateBuilder.now)
	}
	workersSpec, err = p.templateBuilder.GenerateCAPISpecWorkers(clusterSpec, workloadTemplateNames, kubeadmconfigTemplateNames)
	if err != nil {
		return nil, nil, err
	}
	return controlPlaneSpec, workersSpec, nil
}

func (p *vsphereProvider) GenerateCAPISpecForUpgrade(ctx context.Context, bootstrapCluster, workloadCluster *types.Cluster, currentSpec, clusterSpec *cluster.Spec) (controlPlaneSpec, workersSpec []byte, err error) {
	controlPlaneSpec, workersSpec, err = p.generateCAPISpecForUpgrade(ctx, bootstrapCluster, workloadCluster, currentSpec, clusterSpec)
	if err != nil {
		return nil, nil, fmt.Errorf("generating cluster api spec contents: %v", err)
	}
	return controlPlaneSpec, workersSpec, nil
}

func (p *vsphereProvider) GenerateCAPISpecForCreate(ctx context.Context, _ *types.Cluster, clusterSpec *cluster.Spec) (controlPlaneSpec, workersSpec []byte, err error) {
	controlPlaneSpec, workersSpec, err = p.generateCAPISpecForCreate(ctx, clusterSpec)
	if err != nil {
		return nil, nil, fmt.Errorf("generating cluster api spec contents: %v", err)
	}
	return controlPlaneSpec, workersSpec, nil
}

func (p *vsphereProvider) InstallStorageClass(ctx context.Context, cluster *types.Cluster) error {
	if !p.csiEnabled {
		return nil
	}

	return p.providerKubectlClient.ApplyKubeSpecFromBytes(ctx, cluster, defaultStorageClass)
}

func (p *vsphereProvider) createSecret(ctx context.Context, cluster *types.Cluster, contents *bytes.Buffer) error {
	t, err := template.New("tmpl").Funcs(sprig.TxtFuncMap()).Parse(defaultSecretObject)
	if err != nil {
		return fmt.Errorf("creating secret object template: %v", err)
	}
	vuc := config.NewVsphereUserConfig()

	values := map[string]string{
		"vspherePassword":           os.Getenv(vSpherePasswordKey),
		"vsphereUsername":           os.Getenv(vSphereUsernameKey),
		"eksaCloudProviderUsername": vuc.EksaVsphereCPUsername,
		"eksaCloudProviderPassword": vuc.EksaVsphereCPPassword,
		"eksaCSIUsername":           vuc.EksaVsphereCSIUsername,
		"eksaCSIPassword":           vuc.EksaVsphereCSIPassword,
		"eksaLicense":               os.Getenv(eksaLicense),
		"eksaSystemNamespace":       constants.EksaSystemNamespace,
		"vsphereCredentialsName":    constants.VSphereCredentialsName,
		"eksaLicenseName":           constants.EksaLicenseName,
	}
	err = t.Execute(contents, values)
	if err != nil {
		return fmt.Errorf("substituting values for secret object template: %v", err)
	}
	return nil
}

func (p *vsphereProvider) PreCAPIInstallOnBootstrap(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	return nil
}

func (p *vsphereProvider) PostBootstrapSetup(ctx context.Context, clusterConfig *v1alpha1.Cluster, cluster *types.Cluster) error {
	return nil
}

func (p *vsphereProvider) PostBootstrapSetupUpgrade(ctx context.Context, clusterConfig *v1alpha1.Cluster, cluster *types.Cluster) error {
	return nil
}

func (p *vsphereProvider) PostWorkloadInit(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	return nil
}

func (p *vsphereProvider) Version(clusterSpec *cluster.Spec) string {
	return clusterSpec.VersionsBundle.VSphere.Version
}

func (p *vsphereProvider) EnvMap(_ *cluster.Spec) (map[string]string, error) {
	envMap := make(map[string]string)
	for _, key := range requiredEnvs {
		if env, ok := os.LookupEnv(key); ok && len(env) > 0 {
			envMap[key] = env
		} else {
			return envMap, fmt.Errorf("warning required env not set %s", key)
		}
	}
	return envMap, nil
}

func (p *vsphereProvider) GetDeployments() map[string][]string {
	return map[string][]string{
		"capv-system": {"capv-controller-manager"},
	}
}

func (p *vsphereProvider) GetInfrastructureBundle(clusterSpec *cluster.Spec) *types.InfrastructureBundle {
	bundle := clusterSpec.VersionsBundle
	folderName := fmt.Sprintf("infrastructure-vsphere/%s/", bundle.VSphere.Version)

	infraBundle := types.InfrastructureBundle{
		FolderName: folderName,
		Manifests: []releasev1alpha1.Manifest{
			bundle.VSphere.Components,
			bundle.VSphere.Metadata,
			bundle.VSphere.ClusterTemplate,
		},
	}
	return &infraBundle
}

func (p *vsphereProvider) DatacenterConfig(spec *cluster.Spec) providers.DatacenterConfig {
	return spec.VSphereDatacenter
}

func (p *vsphereProvider) MachineConfigs(spec *cluster.Spec) []providers.MachineConfig {
	annotateMachineConfig(
		spec,
		spec.Cluster.Spec.ControlPlaneConfiguration.MachineGroupRef.Name,
		spec.Cluster.ControlPlaneAnnotation(),
		"true",
	)
	if spec.Cluster.Spec.ExternalEtcdConfiguration != nil {
		annotateMachineConfig(
			spec,
			spec.Cluster.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name,
			spec.Cluster.EtcdAnnotation(),
			"true",
		)
	}

	for _, workerNodeGroupConfiguration := range p.clusterConfig.Spec.WorkerNodeGroupConfigurations {
		setMachineConfigManagedBy(
			spec,
			workerNodeGroupConfiguration.MachineGroupRef.Name,
		)
	}

	machineConfigs := make([]providers.MachineConfig, 0, len(spec.VSphereMachineConfigs))
	for _, m := range spec.VSphereMachineConfigs {
		machineConfigs = append(machineConfigs, m)
	}

	return machineConfigs
}

func annotateMachineConfig(spec *cluster.Spec, machineConfigName, annotationKey, annotationValue string) {
	machineConfig := spec.VSphereMachineConfigs[machineConfigName]
	if machineConfig.Annotations == nil {
		machineConfig.Annotations = make(map[string]string, 1)
	}
	machineConfig.Annotations[annotationKey] = annotationValue
	setMachineConfigManagedBy(spec, machineConfigName)
}

func setMachineConfigManagedBy(spec *cluster.Spec, machineConfigName string) {
	machineConfig := spec.VSphereMachineConfigs[machineConfigName]
	if machineConfig.Annotations == nil {
		machineConfig.Annotations = make(map[string]string, 1)
	}
	if spec.Cluster.IsManaged() {
		machineConfig.SetManagedBy(spec.Cluster.ManagedBy())
	}
}

func (p *vsphereProvider) ValidateNewSpec(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) error {
	prevSpec, err := p.providerKubectlClient.GetEksaCluster(ctx, cluster, clusterSpec.Cluster.Name)
	if err != nil {
		return err
	}

	prevDatacenter, err := p.providerKubectlClient.GetEksaVSphereDatacenterConfig(ctx, prevSpec.Spec.DatacenterRef.Name, cluster.KubeconfigFile, prevSpec.Namespace)
	if err != nil {
		return err
	}

	datacenter := clusterSpec.VSphereDatacenter

	oSpec := prevDatacenter.Spec
	nSpec := datacenter.Spec

	prevMachineConfigRefs := machineRefSliceToMap(prevSpec.MachineConfigRefs())

	for _, machineConfigRef := range clusterSpec.Cluster.MachineConfigRefs() {
		machineConfig, ok := clusterSpec.VSphereMachineConfigs[machineConfigRef.Name]
		if !ok {
			return fmt.Errorf("cannot find machine config %s in vsphere provider machine configs", machineConfigRef.Name)
		}

		if _, ok = prevMachineConfigRefs[machineConfig.Name]; ok {
			err = p.validateMachineConfigImmutability(ctx, cluster, machineConfig, clusterSpec)
			if err != nil {
				return err
			}
		}
	}

	if nSpec.Server != oSpec.Server {
		return fmt.Errorf("spec.server is immutable. Previous value %s, new value %s", oSpec.Server, nSpec.Server)
	}
	if nSpec.Datacenter != oSpec.Datacenter {
		return fmt.Errorf("spec.datacenter is immutable. Previous value %s, new value %s", oSpec.Datacenter, nSpec.Datacenter)
	}

	if nSpec.Network != oSpec.Network {
		return fmt.Errorf("spec.network is immutable. Previous value %s, new value %s", oSpec.Network, nSpec.Network)
	}

	secretChanged, err := p.secretContentsChanged(ctx, cluster)
	if err != nil {
		return err
	}

	if secretChanged {
		return fmt.Errorf("the VSphere credentials derived from %s and %s are immutable; please use the same credentials for the upgraded cluster", vSpherePasswordKey, vSphereUsernameKey)
	}
	return nil
}

func (p *vsphereProvider) getWorkerNodeMachineConfigs(ctx context.Context, workloadCluster *types.Cluster, newClusterSpec *cluster.Spec, workerNodeGroupConfiguration v1alpha1.WorkerNodeGroupConfiguration, prevWorkerNodeGroupConfigs map[string]v1alpha1.WorkerNodeGroupConfiguration) (*v1alpha1.VSphereMachineConfig, *v1alpha1.VSphereMachineConfig, error) {
	if _, ok := prevWorkerNodeGroupConfigs[workerNodeGroupConfiguration.Name]; ok {
		oldWorkerMachineConfig := newClusterSpec.VSphereMachineConfigs[workerNodeGroupConfiguration.MachineGroupRef.Name]
		newWorkerMachineConfig, err := p.providerKubectlClient.GetEksaVSphereMachineConfig(ctx, workerNodeGroupConfiguration.MachineGroupRef.Name, workloadCluster.KubeconfigFile, newClusterSpec.Cluster.Namespace)
		if err != nil {
			return oldWorkerMachineConfig, nil, err
		}
		return oldWorkerMachineConfig, newWorkerMachineConfig, nil
	}
	return nil, nil, nil
}

func (p *vsphereProvider) needsNewMachineTemplate(currentSpec, newClusterSpec *cluster.Spec, workerNodeGroupConfiguration v1alpha1.WorkerNodeGroupConfiguration, vdc *v1alpha1.VSphereDatacenterConfig, prevWorkerNodeGroupConfigs map[string]v1alpha1.WorkerNodeGroupConfiguration, oldWorkerMachineConfig *v1alpha1.VSphereMachineConfig, newWorkerMachineConfig *v1alpha1.VSphereMachineConfig) (bool, error) {
	if _, ok := prevWorkerNodeGroupConfigs[workerNodeGroupConfiguration.Name]; ok {
		needsNewWorkloadTemplate := NeedsNewWorkloadTemplate(currentSpec, newClusterSpec, vdc, newClusterSpec.VSphereDatacenter, oldWorkerMachineConfig, newWorkerMachineConfig)
		return needsNewWorkloadTemplate, nil
	}
	return true, nil
}

func (p *vsphereProvider) needsNewKubeadmConfigTemplate(workerNodeGroupConfiguration v1alpha1.WorkerNodeGroupConfiguration, prevWorkerNodeGroupConfigs map[string]v1alpha1.WorkerNodeGroupConfiguration, oldWorkerNodeVmc *v1alpha1.VSphereMachineConfig, newWorkerNodeVmc *v1alpha1.VSphereMachineConfig) (bool, error) {
	if _, ok := prevWorkerNodeGroupConfigs[workerNodeGroupConfiguration.Name]; ok {
		existingWorkerNodeGroupConfig := prevWorkerNodeGroupConfigs[workerNodeGroupConfiguration.Name]
		return NeedsNewKubeadmConfigTemplate(&workerNodeGroupConfiguration, &existingWorkerNodeGroupConfig, oldWorkerNodeVmc, newWorkerNodeVmc), nil
	}
	return true, nil
}

func (p *vsphereProvider) validateMachineConfigImmutability(ctx context.Context, cluster *types.Cluster, newConfig *v1alpha1.VSphereMachineConfig, clusterSpec *cluster.Spec) error {
	prevMachineConfig, err := p.providerKubectlClient.GetEksaVSphereMachineConfig(ctx, newConfig.Name, cluster.KubeconfigFile, clusterSpec.Cluster.Namespace)
	if err != nil {
		return err
	}

	if newConfig.Spec.StoragePolicyName != prevMachineConfig.Spec.StoragePolicyName {
		return fmt.Errorf("spec.storagePolicyName is immutable. Previous value %s, new value %s", prevMachineConfig.Spec.StoragePolicyName, newConfig.Spec.StoragePolicyName)
	}

	if newConfig.Spec.OSFamily != prevMachineConfig.Spec.OSFamily {
		return fmt.Errorf("spec.osFamily is immutable. Previous value %v, new value %v", prevMachineConfig.Spec.OSFamily, newConfig.Spec.OSFamily)
	}

	return nil
}

func (p *vsphereProvider) secretContentsChanged(ctx context.Context, workloadCluster *types.Cluster) (bool, error) {
	nPassword := os.Getenv(vSpherePasswordKey)
	oSecret, err := p.providerKubectlClient.GetSecretFromNamespace(ctx, workloadCluster.KubeconfigFile, CredentialsObjectName, constants.EksaSystemNamespace)
	if err != nil {
		return false, fmt.Errorf("obtaining VSphere secret %s from workload cluster: %v", CredentialsObjectName, err)
	}

	if string(oSecret.Data["password"]) != nPassword {
		return true, nil
	}

	nUser := os.Getenv(vSphereUsernameKey)
	if string(oSecret.Data["username"]) != nUser {
		return true, nil
	}
	return false, nil
}

func (p *vsphereProvider) ChangeDiff(currentSpec, newSpec *cluster.Spec) *types.ComponentChangeDiff {
	if currentSpec.VersionsBundle.VSphere.Version == newSpec.VersionsBundle.VSphere.Version {
		return nil
	}

	return &types.ComponentChangeDiff{
		ComponentName: constants.VSphereProviderName,
		NewVersion:    newSpec.VersionsBundle.VSphere.Version,
		OldVersion:    currentSpec.VersionsBundle.VSphere.Version,
	}
}

func (p *vsphereProvider) RunPostControlPlaneUpgrade(_ context.Context, _ *cluster.Spec, _ *cluster.Spec, _ *types.Cluster, _ *types.Cluster) error {
	return nil
}

func cpiResourceSetName(clusterSpec *cluster.Spec) string {
	return fmt.Sprintf("%s-cpi", clusterSpec.Cluster.Name)
}

func csiResourceSetName(clusterSpec *cluster.Spec) string {
	return fmt.Sprintf("%s-csi", clusterSpec.Cluster.Name)
}

func (p *vsphereProvider) UpgradeNeeded(ctx context.Context, newSpec, currentSpec *cluster.Spec, cluster *types.Cluster) (bool, error) {
	newV, oldV := newSpec.VersionsBundle.VSphere, currentSpec.VersionsBundle.VSphere

	if newV.Driver.ImageDigest != oldV.Driver.ImageDigest ||
		newV.Syncer.ImageDigest != oldV.Syncer.ImageDigest ||
		newV.Manager.ImageDigest != oldV.Manager.ImageDigest ||
		newV.KubeVip.ImageDigest != oldV.KubeVip.ImageDigest {
		return true, nil
	}
	cc := currentSpec.Cluster
	existingVdc, err := p.providerKubectlClient.GetEksaVSphereDatacenterConfig(ctx, cc.Spec.DatacenterRef.Name, cluster.KubeconfigFile, newSpec.Cluster.Namespace)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(existingVdc.Spec, newSpec.VSphereDatacenter.Spec) {
		logger.V(3).Info("New provider spec is different from the new spec")
		return true, nil
	}

	machineConfigsSpecChanged, err := p.machineConfigsSpecChanged(ctx, cc, cluster, newSpec)
	if err != nil {
		return false, err
	}
	return machineConfigsSpecChanged, nil
}

func machineRefSliceToMap(machineRefs []v1alpha1.Ref) map[string]v1alpha1.Ref {
	refMap := make(map[string]v1alpha1.Ref, len(machineRefs))
	for _, ref := range machineRefs {
		refMap[ref.Name] = ref
	}
	return refMap
}

func machineDeploymentName(clusterName, nodeGroupName string) string {
	return fmt.Sprintf("%s-%s", clusterName, nodeGroupName)
}

func (p *vsphereProvider) InstallCustomProviderComponents(ctx context.Context, kubeconfigFile string) error {
	return nil
}

func (p *vsphereProvider) PostBootstrapDeleteForUpgrade(ctx context.Context) error {
	return nil
}

// GetDefaultStorageClass returns a copy of default storage class.
func GetDefaultStorageClass() []byte {
	exportedStorageClass := make([]byte, len(defaultStorageClass))
	copy(exportedStorageClass, defaultStorageClass)
	return exportedStorageClass
}
