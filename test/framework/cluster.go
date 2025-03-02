package framework

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	rapi "github.com/tinkerbell/rufio/api/v1alpha1"
	rctrl "github.com/tinkerbell/rufio/controllers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	packagesv1 "github.com/aws/eks-anywhere-packages/api/v1alpha1"
	"github.com/aws/eks-anywhere/internal/pkg/api"
	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/clients/kubernetes"
	"github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/constants"
	"github.com/aws/eks-anywhere/pkg/controller/clientutil"
	"github.com/aws/eks-anywhere/pkg/executables"
	"github.com/aws/eks-anywhere/pkg/filewriter"
	"github.com/aws/eks-anywhere/pkg/git"
	"github.com/aws/eks-anywhere/pkg/retrier"
	"github.com/aws/eks-anywhere/pkg/semver"
	"github.com/aws/eks-anywhere/pkg/templater"
	"github.com/aws/eks-anywhere/pkg/types"
)

const (
	defaultClusterConfigFile         = "cluster.yaml"
	defaultBundleReleaseManifestFile = "bin/local-bundle-release.yaml"
	defaultEksaBinaryLocation        = "eksctl anywhere"
	defaultClusterName               = "eksa-test"
	eksctlVersionEnvVar              = "EKSCTL_VERSION"
	eksctlVersionEnvVarDummyVal      = "ham sandwich"
	ClusterPrefixVar                 = "T_CLUSTER_PREFIX"
	JobIdVar                         = "T_JOB_ID"
	BundlesOverrideVar               = "T_BUNDLES_OVERRIDE"
	ClusterIPPoolEnvVar              = "T_CLUSTER_IP_POOL"
	CleanupVmsVar                    = "T_CLEANUP_VMS"
	hardwareYamlPath                 = "hardware.yaml"
	hardwareCsvPath                  = "hardware.csv"
	EksaPackagesInstallation         = "eks-anywhere-packages"
)

//go:embed testdata/oidc-roles.yaml
var oidcRoles []byte

//go:embed testdata/hpa_busybox.yaml
var hpaBusybox []byte

type ClusterE2ETest struct {
	T                      T
	ClusterConfigLocation  string
	ClusterConfigFolder    string
	HardwareConfigLocation string
	HardwareCsvLocation    string
	TestHardware           map[string]*api.Hardware
	HardwarePool           map[string]*api.Hardware
	WithNoPowerActions     bool
	ClusterName            string
	ClusterConfig          *cluster.Config
	clusterValidator       *ClusterValidator
	Provider               Provider
	clusterFillers         []api.ClusterFiller
	KubectlClient          *executables.Kubectl
	GitProvider            git.ProviderClient
	GitClient              git.Client
	HelmInstallConfig      *HelmInstallConfig
	PackageConfig          *PackageConfig
	GitWriter              filewriter.FileWriter
	eksaBinaryLocation     string
	ExpectFailure          bool
}

type ClusterE2ETestOpt func(e *ClusterE2ETest)

// NewClusterE2ETest is a support structure for defining an end-to-end test.
func NewClusterE2ETest(t T, provider Provider, opts ...ClusterE2ETestOpt) *ClusterE2ETest {
	e := &ClusterE2ETest{
		T:                     t,
		Provider:              provider,
		ClusterConfig:         &cluster.Config{},
		ClusterConfigLocation: defaultClusterConfigFile,
		ClusterName:           getClusterName(t),
		clusterFillers:        make([]api.ClusterFiller, 0),
		KubectlClient:         buildKubectl(t),
		eksaBinaryLocation:    defaultEksaBinaryLocation,
	}

	for _, opt := range opts {
		opt(e)
	}

	if e.ClusterConfigFolder == "" {
		e.ClusterConfigFolder = e.ClusterName
	}
	if e.HardwareConfigLocation == "" {
		e.HardwareConfigLocation = filepath.Join(e.ClusterConfigFolder, hardwareYamlPath)
	}
	if e.HardwareCsvLocation == "" {
		e.HardwareCsvLocation = filepath.Join(e.ClusterConfigFolder, hardwareCsvPath)
	}

	e.ClusterConfigLocation = filepath.Join(e.ClusterConfigFolder, e.ClusterName+"-eks-a.yaml")

	if err := os.MkdirAll(e.ClusterConfigFolder, os.ModePerm); err != nil {
		t.Fatalf("Failed creating cluster config folder for test: %s", err)
	}

	provider.Setup()

	e.T.Cleanup(func() {
		e.CleanupVms()

		tinkerbellCIEnvironment := os.Getenv(TinkerbellCIEnvironment)
		if e.Provider.Name() == TinkerbellProviderName && tinkerbellCIEnvironment == "true" {
			e.CleanupDockerEnvironment()
		}
	})

	return e
}

func withHardware(requiredCount int, hardareType string, labels map[string]string) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		hardwarePool := e.GetHardwarePool()

		if e.TestHardware == nil {
			e.TestHardware = make(map[string]*api.Hardware)
		}

		var count int
		for id, h := range hardwarePool {
			if _, exists := e.TestHardware[id]; !exists {
				count++
				h.Labels = labels
				e.TestHardware[id] = h
			}

			if count == requiredCount {
				break
			}
		}

		if count < requiredCount {
			e.T.Errorf("this test requires at least %d piece(s) of %s hardware", requiredCount, hardareType)
		}
	}
}

func WithNoPowerActions() ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		e.WithNoPowerActions = true
	}
}

func ExpectFailure(expected bool) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		e.ExpectFailure = expected
	}
}

func WithControlPlaneHardware(requiredCount int) ClusterE2ETestOpt {
	return withHardware(
		requiredCount,
		api.ControlPlane,
		map[string]string{api.HardwareLabelTypeKeyName: api.ControlPlane},
	)
}

func WithWorkerHardware(requiredCount int) ClusterE2ETestOpt {
	return withHardware(requiredCount, api.Worker, map[string]string{api.HardwareLabelTypeKeyName: api.Worker})
}

func WithCustomLabelHardware(requiredCount int, label string) ClusterE2ETestOpt {
	return withHardware(requiredCount, api.Worker, map[string]string{api.HardwareLabelTypeKeyName: label})
}

func WithExternalEtcdHardware(requiredCount int) ClusterE2ETestOpt {
	return withHardware(
		requiredCount,
		api.ExternalEtcd,
		map[string]string{api.HardwareLabelTypeKeyName: api.ExternalEtcd},
	)
}

// WithClusterName sets the name that will be used for the cluster. This will drive both the name of the eks-a
// cluster config objects as well as the cluster config file name.
func WithClusterName(name string) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		e.ClusterName = name
	}
}

func (e *ClusterE2ETest) GetHardwarePool() map[string]*api.Hardware {
	if e.HardwarePool == nil {
		csvFilePath := os.Getenv(tinkerbellInventoryCsvFilePathEnvVar)
		var err error
		e.HardwarePool, err = api.NewHardwareMapFromFile(csvFilePath)
		if err != nil {
			e.T.Fatalf("failed to create hardware map from test hardware pool: %v", err)
		}
	}
	return e.HardwarePool
}

func (e *ClusterE2ETest) RunClusterFlowWithGitOps(clusterOpts ...ClusterE2ETestOpt) {
	e.GenerateClusterConfig()
	e.createCluster()
	e.UpgradeWithGitOps(clusterOpts...)
	time.Sleep(5 * time.Minute)
	e.deleteCluster()
}

func WithClusterFiller(f ...api.ClusterFiller) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		e.clusterFillers = append(e.clusterFillers, f...)
	}
}

// WithClusterSingleNode helps to create an e2e test option for a single node cluster.
func WithClusterSingleNode(v v1alpha1.KubernetesVersion) ClusterE2ETestOpt {
	return WithClusterFiller(
		api.WithKubernetesVersion(v),
		api.WithControlPlaneCount(1),
		api.WithEtcdCountIfExternal(0),
		api.RemoveAllWorkerNodeGroups(),
	)
}

func WithClusterConfigLocationOverride(path string) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		e.ClusterConfigLocation = path
	}
}

func WithEksaVersion(version *semver.Version) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		eksaBinaryLocation, err := GetReleaseBinaryFromVersion(version)
		if err != nil {
			e.T.Fatal(err)
		}
		e.eksaBinaryLocation = eksaBinaryLocation
		err = setEksctlVersionEnvVar()
		if err != nil {
			e.T.Fatal(err)
		}
	}
}

func WithLatestMinorReleaseFromMain() ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		eksaBinaryLocation, err := GetLatestMinorReleaseBinaryFromMain()
		if err != nil {
			e.T.Fatal(err)
		}
		e.eksaBinaryLocation = eksaBinaryLocation
		err = setEksctlVersionEnvVar()
		if err != nil {
			e.T.Fatal(err)
		}
	}
}

func WithLatestMinorReleaseFromVersion(version *semver.Version) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		eksaBinaryLocation, err := GetLatestMinorReleaseBinaryFromVersion(version)
		if err != nil {
			e.T.Fatal(err)
		}
		e.eksaBinaryLocation = eksaBinaryLocation
		err = setEksctlVersionEnvVar()
		if err != nil {
			e.T.Fatal(err)
		}
	}
}

func WithEnvVar(key, val string) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		err := os.Setenv(key, val)
		if err != nil {
			e.T.Fatalf("couldn't set env var %s to value %s due to: %v", key, val, err)
		}
	}
}

type Provider interface {
	Name() string
	// ClusterConfigUpdates allows a provider to modify the default cluster config
	// after this one is generated for the first time. This is not reapplied on every CLI operation.
	// Prefer to call UpdateClusterConfig directly from the tests to make it more explicit.
	ClusterConfigUpdates() []api.ClusterConfigFiller
	Setup()
	CleanupVMs(clusterName string) error
	UpdateKubeConfig(content *[]byte, clusterName string) error
	ClusterValidations() []ClusterValidation
}

func (e *ClusterE2ETest) GenerateClusterConfig(opts ...CommandOpt) {
	e.GenerateClusterConfigForVersion("", opts...)
}

func (e *ClusterE2ETest) PowerOffHardware() {
	// Initializing BMC Client
	ctx := context.Background()
	bmcClientFactory := rctrl.NewBMCClientFactoryFunc(ctx)

	for _, h := range e.TestHardware {
		bmcClient, err := bmcClientFactory(ctx, h.BMCIPAddress, "623", h.BMCUsername, h.BMCPassword)
		if err != nil {
			e.T.Fatalf("failed to create bmc client: %v", err)
		}

		defer func() {
			// Close BMC connection after reconcilation
			err = bmcClient.Close(ctx)
			if err != nil {
				e.T.Fatalf("BMC close connection failed: %v", err)
			}
		}()

		_, err = bmcClient.SetPowerState(ctx, string(rapi.Off))
		if err != nil {
			e.T.Fatalf("failed to power off hardware: %v", err)
		}
	}
}

func (e *ClusterE2ETest) PXEBootHardware() {
	// Initializing BMC Client
	ctx := context.Background()
	bmcClientFactory := rctrl.NewBMCClientFactoryFunc(ctx)

	for _, h := range e.TestHardware {
		bmcClient, err := bmcClientFactory(ctx, h.BMCIPAddress, "623", h.BMCUsername, h.BMCPassword)
		if err != nil {
			e.T.Fatalf("failed to create bmc client: %v", err)
		}

		defer func() {
			// Close BMC connection after reconcilation
			err = bmcClient.Close(ctx)
			if err != nil {
				e.T.Fatalf("BMC close connection failed: %v", err)
			}
		}()

		_, err = bmcClient.SetBootDevice(ctx, string(rapi.PXE), false, true)
		if err != nil {
			e.T.Fatalf("failed to pxe boot hardware: %v", err)
		}
	}
}

func (e *ClusterE2ETest) PowerOnHardware() {
	// Initializing BMC Client
	ctx := context.Background()
	bmcClientFactory := rctrl.NewBMCClientFactoryFunc(ctx)

	for _, h := range e.TestHardware {
		bmcClient, err := bmcClientFactory(ctx, h.BMCIPAddress, "623", h.BMCUsername, h.BMCPassword)
		if err != nil {
			e.T.Fatalf("failed to create bmc client: %v", err)
		}

		defer func() {
			// Close BMC connection after reconcilation
			err = bmcClient.Close(ctx)
			if err != nil {
				e.T.Fatalf("BMC close connection failed: %v", err)
			}
		}()

		_, err = bmcClient.SetPowerState(ctx, string(rapi.On))
		if err != nil {
			e.T.Fatalf("failed to power on hardware: %v", err)
		}
	}
}

func (e *ClusterE2ETest) ValidateHardwareDecommissioned() {
	// Initializing BMC Client
	ctx := context.Background()
	bmcClientFactory := rctrl.NewBMCClientFactoryFunc(ctx)

	var failedToDecomm []*api.Hardware
	for _, h := range e.TestHardware {
		bmcClient, err := bmcClientFactory(ctx, h.BMCIPAddress, "443", h.BMCUsername, h.BMCPassword)
		if err != nil {
			e.T.Fatalf("failed to create bmc client: %v", err)
		}

		defer func() {
			// Close BMC connection after reconcilation
			err = bmcClient.Close(ctx)
			if err != nil {
				e.T.Fatalf("BMC close connection failed: %v", err)
			}
		}()

		powerState, err := bmcClient.GetPowerState(ctx)
		// add sleep retries to give the machine time to power off
		timeout := 15
		for !strings.EqualFold(powerState, string(rapi.Off)) && timeout > 0 {
			if err != nil {
				e.T.Logf("failed to get power state for hardware (%v): %v", h, err)
			}
			time.Sleep(5 * time.Second)
			timeout = timeout - 5
			powerState, err = bmcClient.GetPowerState(ctx)
			e.T.Logf(
				"hardware power state (id=%s, hostname=%s, bmc_ip=%s): power_state=%s",
				h.MACAddress,
				h.Hostname,
				h.BMCIPAddress,
				powerState,
			)
		}

		if !strings.EqualFold(powerState, string(rapi.Off)) {
			e.T.Logf(
				"failed to decommission hardware: id=%s, hostname=%s, bmc_ip=%s",
				h.MACAddress,
				h.Hostname,
				h.BMCIPAddress,
			)
			failedToDecomm = append(failedToDecomm, h)
		} else {
			e.T.Logf("successfully decommissioned hardware: id=%s, hostname=%s, bmc_ip=%s", h.MACAddress, h.Hostname, h.BMCIPAddress)
		}
	}

	if len(failedToDecomm) > 0 {
		e.T.Fatalf("failed to decommision hardware during cluster deletion")
	}
}

func (e *ClusterE2ETest) GenerateHardwareConfig(opts ...CommandOpt) {
	e.generateHardwareConfig(opts...)
}

func (e *ClusterE2ETest) generateHardwareConfig(opts ...CommandOpt) {
	if len(e.TestHardware) == 0 {
		e.T.Fatal("you must provide the ClusterE2ETest the hardware to use for the test run")
	}

	if _, err := os.Stat(e.HardwareCsvLocation); err == nil {
		os.Remove(e.HardwareCsvLocation)
	}

	testHardware := e.TestHardware
	if e.WithNoPowerActions {
		hardwareWithNoBMC := make(map[string]*api.Hardware)
		for k, h := range testHardware {
			lessBmc := *h
			lessBmc.BMCIPAddress = ""
			lessBmc.BMCUsername = ""
			lessBmc.BMCPassword = ""
			hardwareWithNoBMC[k] = &lessBmc
		}
		testHardware = hardwareWithNoBMC
	}

	err := api.WriteHardwareMapToCSV(testHardware, e.HardwareCsvLocation)
	if err != nil {
		e.T.Fatalf("failed to create hardware csv for the test run: %v", err)
	}

	generateHardwareConfigArgs := []string{
		"generate", "hardware",
		"-z", e.HardwareCsvLocation,
		"-o", e.HardwareConfigLocation,
	}

	e.RunEKSA(generateHardwareConfigArgs, opts...)
}

func (e *ClusterE2ETest) GenerateClusterConfigForVersion(eksaVersion string, opts ...CommandOpt) {
	e.generateClusterConfigObjects(opts...)
	if eksaVersion != "" {
		err := cleanUpClusterForVersion(e.ClusterConfig, eksaVersion)
		if err != nil {
			e.T.Fatal(err)
		}
	}

	e.buildClusterConfigFile()
}

func (e *ClusterE2ETest) generateClusterConfigObjects(opts ...CommandOpt) {
	e.generateClusterConfigWithCLI()
	config, err := cluster.ParseConfigFromFile(e.ClusterConfigLocation)
	if err != nil {
		e.T.Fatalf("Failed parsing generated cluster config: %s", err)
	}

	// Copy all objects that might be generated by the CLI.
	// Don't replace the whole ClusterConfig since some ClusterE2ETestOpt might
	// have already set some data in it.
	e.ClusterConfig.Cluster = config.Cluster
	e.ClusterConfig.CloudStackDatacenter = config.CloudStackDatacenter
	e.ClusterConfig.VSphereDatacenter = config.VSphereDatacenter
	e.ClusterConfig.DockerDatacenter = config.DockerDatacenter
	e.ClusterConfig.SnowDatacenter = config.SnowDatacenter
	e.ClusterConfig.NutanixDatacenter = config.NutanixDatacenter
	e.ClusterConfig.TinkerbellDatacenter = config.TinkerbellDatacenter
	e.ClusterConfig.VSphereMachineConfigs = config.VSphereMachineConfigs
	e.ClusterConfig.CloudStackMachineConfigs = config.CloudStackMachineConfigs
	e.ClusterConfig.SnowMachineConfigs = config.SnowMachineConfigs
	e.ClusterConfig.NutanixMachineConfigs = config.NutanixMachineConfigs
	e.ClusterConfig.TinkerbellMachineConfigs = config.TinkerbellMachineConfigs
	e.ClusterConfig.TinkerbellTemplateConfigs = config.TinkerbellTemplateConfigs

	e.UpdateClusterConfig(e.baseClusterConfigUpdates()...)
}

// UpdateClusterConfig applies the cluster Config provided updates to e.ClusterConfig, marshalls its content
// to yaml and writes it to a file on disk configured by e.ClusterConfigLocation. Call this method when you want
// make changes to the eks-a cluster definition before running a CLI command or API operation.
func (e *ClusterE2ETest) UpdateClusterConfig(fillers ...api.ClusterConfigFiller) {
	e.T.Log("Updating cluster config")
	api.UpdateClusterConfig(e.ClusterConfig, fillers...)
	e.T.Logf("Writing cluster config to file: %s", e.ClusterConfigLocation)
	e.buildClusterConfigFile()
}

func (e *ClusterE2ETest) baseClusterConfigUpdates(opts ...CommandOpt) []api.ClusterConfigFiller {
	clusterFillers := make([]api.ClusterFiller, 0, len(e.clusterFillers)+3)
	// This defaults all tests to a 1:1:1 configuration. Since all the fillers defined on each test are run
	// after these 3, if the tests is explicit about any of these, the defaults will be overwritten
	clusterFillers = append(clusterFillers,
		api.WithControlPlaneCount(1), api.WithWorkerNodeCount(1), api.WithEtcdCountIfExternal(1),
	)
	clusterFillers = append(clusterFillers, e.clusterFillers...)
	configFillers := []api.ClusterConfigFiller{api.ClusterToConfigFiller(clusterFillers...)}
	configFillers = append(configFillers, e.Provider.ClusterConfigUpdates()...)

	return configFillers
}

func (e *ClusterE2ETest) generateClusterConfigWithCLI(opts ...CommandOpt) {
	generateClusterConfigArgs := []string{"generate", "clusterconfig", e.ClusterName, "-p", e.Provider.Name(), ">", e.ClusterConfigLocation}
	e.RunEKSA(generateClusterConfigArgs, opts...)
	e.T.Log("Cluster config generated with CLI")
}

func (e *ClusterE2ETest) parseClusterConfigFromDisk(file string) {
	e.T.Logf("Parsing cluster config from disk: %s", file)
	config, err := cluster.ParseConfigFromFile(file)
	if err != nil {
		e.T.Fatalf("Failed parsing generated cluster config: %s", err)
	}
	e.ClusterConfig = config
}

func (e *ClusterE2ETest) parseClusterConfigWithDefaultsFromDisk() (*v1alpha1.Cluster, error) {
	fullClusterConfigLocation := filepath.Join(e.ClusterConfigFolder, e.ClusterName+"-eks-a-cluster.yaml")
	content, err := os.ReadFile(fullClusterConfigLocation)
	if err != nil {
		return nil, fmt.Errorf("reading cluster config file: %v", err)
	}

	parsedCluster := &v1alpha1.Cluster{}
	if err := yaml.Unmarshal(content, parsedCluster); err != nil {
		return nil, fmt.Errorf("unable to marshal cluster config file contents %v", err)
	}

	return parsedCluster, err
}

// WithClusterConfig generates a base cluster config using the CLI `generate clusterconfig` command
// and updates them with the provided fillers. Helpful for defining the initial Cluster config
// before running a create operation.
func (e *ClusterE2ETest) WithClusterConfig(fillers ...api.ClusterConfigFiller) *ClusterE2ETest {
	e.T.Logf("Generating base config for cluster %s", e.ClusterName)
	e.generateClusterConfigWithCLI()
	e.parseClusterConfigFromDisk(e.ClusterConfigLocation)
	base := e.baseClusterConfigUpdates()
	allUpdates := make([]api.ClusterConfigFiller, 0, len(base)+len(fillers))
	allUpdates = append(allUpdates, base...)
	allUpdates = append(allUpdates, fillers...)
	e.UpdateClusterConfig(allUpdates...)
	return e
}

func (e *ClusterE2ETest) ImportImages(opts ...CommandOpt) {
	importImagesArgs := []string{"import-images", "-f", e.ClusterConfigLocation}
	e.RunEKSA(importImagesArgs, opts...)
}

func (e *ClusterE2ETest) DownloadArtifacts(opts ...CommandOpt) {
	downloadArtifactsArgs := []string{"download", "artifacts", "-f", e.ClusterConfigLocation}
	e.RunEKSA(downloadArtifactsArgs, opts...)
	if _, err := os.Stat("eks-anywhere-downloads.tar.gz"); err != nil {
		e.T.Fatal(err)
	} else {
		e.T.Log("Downloaded artifacts saved at eks-anywhere-downloads.tar.gz")
	}
}

func (e *ClusterE2ETest) CreateCluster(opts ...CommandOpt) {
	e.createCluster(opts...)
}

func (e *ClusterE2ETest) createCluster(opts ...CommandOpt) {
	e.T.Logf("Creating cluster %s", e.ClusterName)
	createClusterArgs := []string{"create", "cluster", "-f", e.ClusterConfigLocation, "-v", "12"}
	if getBundlesOverride() == "true" {
		createClusterArgs = append(createClusterArgs, "--bundles-override", defaultBundleReleaseManifestFile)
	}

	if e.Provider.Name() == TinkerbellProviderName {
		createClusterArgs = append(createClusterArgs, "-z", e.HardwareCsvLocation)
		tinkBootstrapIP := os.Getenv(tinkerbellBootstrapIPEnvVar)
		e.T.Logf("tinkBootstrapIP: %s", tinkBootstrapIP)
		if tinkBootstrapIP != "" {
			createClusterArgs = append(createClusterArgs, "--tinkerbell-bootstrap-ip", tinkBootstrapIP)
		}
	}

	e.RunEKSA(createClusterArgs, opts...)
}

func (e *ClusterE2ETest) ValidateCluster(kubeVersion v1alpha1.KubernetesVersion) {
	ctx := context.Background()
	e.T.Log("Validating cluster node status")
	r := retrier.New(10 * time.Minute)
	err := r.Retry(func() error {
		err := e.KubectlClient.ValidateNodes(ctx, e.Cluster().KubeconfigFile)
		if err != nil {
			return fmt.Errorf("validating nodes status: %v", err)
		}
		return nil
	})
	if err != nil {
		e.T.Fatal(err)
	}
	e.T.Log("Validating cluster node version")
	err = retrier.Retry(180, 1*time.Second, func() error {
		if err = e.KubectlClient.ValidateNodesVersion(ctx, e.Cluster().KubeconfigFile, kubeVersion); err != nil {
			return fmt.Errorf("validating nodes version: %v", err)
		}
		return nil
	})
	if err != nil {
		e.T.Fatal(err)
	}
}

func (e *ClusterE2ETest) WaitForMachineDeploymentReady(machineDeploymentName string) {
	ctx := context.Background()
	e.T.Logf("Waiting for machine deployment %s to be ready for cluster %s", machineDeploymentName, e.ClusterName)
	err := e.KubectlClient.WaitForMachineDeploymentReady(ctx, e.Cluster(), "5m", machineDeploymentName)
	if err != nil {
		e.T.Fatal(err)
	}
}

// GetEKSACluster retrieves the EKSA cluster from the runtime environment using kubectl.
func (e *ClusterE2ETest) GetEKSACluster() *v1alpha1.Cluster {
	ctx := context.Background()
	clus, err := e.KubectlClient.GetEksaCluster(ctx, e.Cluster(), e.ClusterName)
	if err != nil {
		e.T.Fatal(err)
	}
	return clus
}

func (e *ClusterE2ETest) GetCapiMachinesForCluster(clusterName string) map[string]types.Machine {
	ctx := context.Background()
	capiMachines, err := e.KubectlClient.GetMachines(ctx, e.Cluster(), clusterName)
	if err != nil {
		e.T.Fatal(err)
	}
	machinesMap := make(map[string]types.Machine, 0)
	for _, machine := range capiMachines {
		machinesMap[machine.Metadata.Name] = machine
	}
	return machinesMap
}

// ApplyClusterManifest uses client-side logic to create/update objects defined in a cluster yaml manifest.
func (e *ClusterE2ETest) ApplyClusterManifest() {
	ctx := context.Background()
	e.T.Logf("Applying cluster %s spec located at %s", e.ClusterName, e.ClusterConfigLocation)
	e.applyClusterManifest(ctx)
}

func (e *ClusterE2ETest) applyClusterManifest(ctx context.Context) {
	if err := e.KubectlClient.ApplyManifest(ctx, e.kubeconfigFilePath(), e.ClusterConfigLocation); err != nil {
		e.T.Fatalf("Failed to apply cluster config: %s", err)
	}
}

func WithClusterUpgrade(fillers ...api.ClusterFiller) ClusterE2ETestOpt {
	return func(e *ClusterE2ETest) {
		e.UpdateClusterConfig(api.ClusterToConfigFiller(fillers...))
	}
}

// UpgradeClusterWithKubectl uses client-side logic to upgrade a cluster.
func (e *ClusterE2ETest) UpgradeClusterWithKubectl(fillers ...api.ClusterConfigFiller) {
	fullClusterConfigLocation := filepath.Join(e.ClusterConfigFolder, e.ClusterName+"-eks-a-cluster.yaml")
	e.parseClusterConfigFromDisk(fullClusterConfigLocation)
	e.UpdateClusterConfig(fillers...)
	e.ApplyClusterManifest()
}

// UpgradeClusterWithNewConfig applies the test options, re-generates the cluster config file and runs the CLI upgrade command.
func (e *ClusterE2ETest) UpgradeClusterWithNewConfig(clusterOpts []ClusterE2ETestOpt, commandOpts ...CommandOpt) {
	e.upgradeCluster(clusterOpts, commandOpts...)
}

func (e *ClusterE2ETest) upgradeCluster(clusterOpts []ClusterE2ETestOpt, commandOpts ...CommandOpt) {
	for _, opt := range clusterOpts {
		opt(e)
	}
	e.buildClusterConfigFile()

	e.UpgradeCluster(commandOpts...)
}

// UpgradeCluster runs the CLI upgrade command.
func (e *ClusterE2ETest) UpgradeCluster(commandOpts ...CommandOpt) {
	upgradeClusterArgs := []string{"upgrade", "cluster", "-f", e.ClusterConfigLocation, "-v", "4"}
	if getBundlesOverride() == "true" {
		upgradeClusterArgs = append(upgradeClusterArgs, "--bundles-override", defaultBundleReleaseManifestFile)
	}

	e.RunEKSA(upgradeClusterArgs, commandOpts...)
}

func (e *ClusterE2ETest) generateClusterConfigYaml() []byte {
	childObjs := e.ClusterConfig.ChildObjects()
	yamlB := make([][]byte, 0, len(childObjs)+1)

	// This is required because Flux requires a namespace be specified for objects
	// to be able to reconcile right.
	if e.ClusterConfig.Cluster.Namespace == "" {
		e.ClusterConfig.Cluster.Namespace = "default"
	}
	clusterConfigB, err := yaml.Marshal(e.ClusterConfig.Cluster)
	if err != nil {
		e.T.Fatal(err)
	}
	yamlB = append(yamlB, clusterConfigB)
	for _, o := range childObjs {
		// This is required because Flux requires a namespace be specified for objects
		// to be able to reconcile right.
		if o.GetNamespace() == "" {
			o.SetNamespace("default")
		}
		objB, err := yaml.Marshal(o)
		if err != nil {
			e.T.Fatalf("Failed marshalling %s config: %v", o.GetName(), err)
		}
		yamlB = append(yamlB, objB)
	}

	return templater.AppendYamlResources(yamlB...)
}

func (e *ClusterE2ETest) buildClusterConfigFile() {
	yaml := e.generateClusterConfigYaml()

	writer, err := filewriter.NewWriter(e.ClusterConfigFolder)
	if err != nil {
		e.T.Fatalf("Error creating writer: %v", err)
	}

	writtenFile, err := writer.Write(filepath.Base(e.ClusterConfigLocation), yaml, filewriter.PersistentFile)
	if err != nil {
		e.T.Fatalf("Error writing cluster config to file %s: %v", e.ClusterConfigLocation, err)
	}
	e.ClusterConfigLocation = writtenFile
}

func (e *ClusterE2ETest) DeleteCluster(opts ...CommandOpt) {
	e.deleteCluster(opts...)
}

func (e *ClusterE2ETest) CleanupVms() {
	if !shouldCleanUpVms() {
		e.T.Logf("Skipping VM cleanup")
		return
	}

	if err := e.Provider.CleanupVMs(e.ClusterName); err != nil {
		e.T.Logf("failed to clean up VMs: %v", err)
	}
}

func (e *ClusterE2ETest) CleanupDockerEnvironment() {
	e.T.Logf("cleanup kind enviornment...")
	e.Run("kind", "delete", "clusters", "--all", "||", "true")
	e.T.Logf("cleanup docker enviornment...")
	e.Run("docker", "rm", "-vf", "$(docker ps -a -q)", "||", "true")
}

func shouldCleanUpVms() bool {
	shouldCleanupVms, err := getCleanupVmsVar()
	return err == nil && shouldCleanupVms
}

func (e *ClusterE2ETest) deleteCluster(opts ...CommandOpt) {
	deleteClusterArgs := []string{"delete", "cluster", e.ClusterName, "-v", "4"}
	if getBundlesOverride() == "true" {
		deleteClusterArgs = append(deleteClusterArgs, "--bundles-override", defaultBundleReleaseManifestFile)
	}
	e.RunEKSA(deleteClusterArgs, opts...)
}

func (e *ClusterE2ETest) Run(name string, args ...string) {
	command := strings.Join(append([]string{name}, args...), " ")
	shArgs := []string{"-c", command}

	e.T.Log("Running shell command", "[", command, "]")
	cmd := exec.CommandContext(context.Background(), "sh", shArgs...)

	envPath := os.Getenv("PATH")

	workDir, err := os.Getwd()
	if err != nil {
		e.T.Fatalf("Error finding current directory: %v", err)
	}

	var stdoutAndErr bytes.Buffer

	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, fmt.Sprintf("PATH=%s/bin:%s", workDir, envPath))
	cmd.Stderr = io.MultiWriter(os.Stderr, &stdoutAndErr)
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutAndErr)

	if err = cmd.Run(); err != nil {
		e.T.Log("Command failed, scanning output for error")
		scanner := bufio.NewScanner(&stdoutAndErr)
		var errorMessage string
		// Look for the last line of the out put that starts with 'Error:'
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Error:") {
				errorMessage = line
			}
		}

		if err := scanner.Err(); err != nil {
			e.T.Fatalf("Failed reading command output looking for error message: %v", err)
		}

		if errorMessage != "" {
			if e.ExpectFailure {
				e.T.Logf("This error was expected. Continuing...")
				return
			}
			e.T.Fatalf("Command %s %v failed with error: %v: %s", name, args, err, errorMessage)
		}

		e.T.Fatalf("Error running command %s %v: %v", name, args, err)
	}
}

func (e *ClusterE2ETest) RunEKSA(args []string, opts ...CommandOpt) {
	binaryPath := e.eksaBinaryLocation
	for _, o := range opts {
		err := o(&binaryPath, &args)
		if err != nil {
			e.T.Fatalf("Error executing EKS-A at path %s with args %s: %v", binaryPath, args, err)
		}
	}
	e.Run(binaryPath, args...)
}

func (e *ClusterE2ETest) StopIfFailed() {
	if e.T.Failed() {
		e.T.FailNow()
	}
}

func (e *ClusterE2ETest) cleanup(f func()) {
	e.T.Cleanup(func() {
		if !e.T.Failed() {
			f()
		}
	})
}

// Cluster builds a cluster obj using the ClusterE2ETest name and kubeconfig.
func (e *ClusterE2ETest) Cluster() *types.Cluster {
	return &types.Cluster{
		Name:           e.ClusterName,
		KubeconfigFile: e.kubeconfigFilePath(),
	}
}

func (e *ClusterE2ETest) managementCluster() *types.Cluster {
	return &types.Cluster{
		Name:           e.ClusterConfig.Cluster.ManagedBy(),
		KubeconfigFile: e.managementKubeconfigFilePath(),
	}
}

func (e *ClusterE2ETest) kubeconfigFilePath() string {
	return filepath.Join(e.ClusterName, fmt.Sprintf("%s-eks-a-cluster.kubeconfig", e.ClusterName))
}

func (e *ClusterE2ETest) managementKubeconfigFilePath() string {
	clusterConfig := e.ClusterConfig.Cluster
	if clusterConfig.IsSelfManaged() {
		return e.kubeconfigFilePath()
	}
	managementClusterName := e.ClusterConfig.Cluster.ManagedBy()
	return filepath.Join(managementClusterName, fmt.Sprintf("%s-eks-a-cluster.kubeconfig", managementClusterName))
}

func (e *ClusterE2ETest) GetEksaVSphereMachineConfigs() []v1alpha1.VSphereMachineConfig {
	clusterConfig := e.ClusterConfig.Cluster
	machineConfigNames := make([]string, 0, len(clusterConfig.Spec.WorkerNodeGroupConfigurations)+1)
	machineConfigNames = append(machineConfigNames, clusterConfig.Spec.ControlPlaneConfiguration.MachineGroupRef.Name)
	for _, workerNodeConf := range clusterConfig.Spec.WorkerNodeGroupConfigurations {
		machineConfigNames = append(machineConfigNames, workerNodeConf.MachineGroupRef.Name)
	}

	kubeconfig := e.kubeconfigFilePath()
	ctx := context.Background()

	machineConfigs := make([]v1alpha1.VSphereMachineConfig, 0, len(machineConfigNames))
	for _, name := range machineConfigNames {
		m, err := e.KubectlClient.GetEksaVSphereMachineConfig(ctx, name, kubeconfig, clusterConfig.Namespace)
		if err != nil {
			e.T.Fatalf("Failed getting VSphereMachineConfig: %v", err)
		}

		machineConfigs = append(machineConfigs, *m)
	}

	return machineConfigs
}

func GetTestNameHash(name string) string {
	h := sha1.New()
	h.Write([]byte(name))
	testNameHash := fmt.Sprintf("%x", h.Sum(nil))
	return testNameHash[:7]
}

func getClusterName(t T) string {
	value := os.Getenv(ClusterPrefixVar)
	// Append hash to make each cluster name unique per test. Using the testname will be too long
	// and would fail validations
	if len(value) == 0 {
		value = defaultClusterName
	}

	return fmt.Sprintf("%s-%s", value, GetTestNameHash(t.Name()))
}

func getBundlesOverride() string {
	return os.Getenv(BundlesOverrideVar)
}

func getCleanupVmsVar() (bool, error) {
	return strconv.ParseBool(os.Getenv(CleanupVmsVar))
}

func setEksctlVersionEnvVar() error {
	eksctlVersionEnv := os.Getenv(eksctlVersionEnvVar)
	if eksctlVersionEnv == "" {
		err := os.Setenv(eksctlVersionEnvVar, eksctlVersionEnvVarDummyVal)
		if err != nil {
			return fmt.Errorf(
				"couldn't set eksctl version env var %s to value %s",
				eksctlVersionEnvVar,
				eksctlVersionEnvVarDummyVal,
			)
		}
	}
	return nil
}

func (e *ClusterE2ETest) InstallHelmChart() {
	kubeconfig := e.kubeconfigFilePath()
	ctx := context.Background()

	err := e.HelmInstallConfig.HelmClient.InstallChart(ctx, e.HelmInstallConfig.chartName, e.HelmInstallConfig.chartURI, e.HelmInstallConfig.chartVersion, kubeconfig, "", "", e.HelmInstallConfig.chartValues)
	if err != nil {
		e.T.Fatalf("Error installing %s helm chart on the cluster: %v", e.HelmInstallConfig.chartName, err)
	}
}

func (e *ClusterE2ETest) CreateNamespace(namespace string) {
	kubeconfig := e.kubeconfigFilePath()
	err := e.KubectlClient.CreateNamespace(context.Background(), kubeconfig, namespace)
	if err != nil {
		e.T.Fatalf("Namespace creation failed for %s", namespace)
	}
}

func (e *ClusterE2ETest) DeleteNamespace(namespace string) {
	kubeconfig := e.kubeconfigFilePath()
	err := e.KubectlClient.DeleteNamespace(context.Background(), kubeconfig, namespace)
	if err != nil {
		e.T.Fatalf("Namespace deletion failed for %s", namespace)
	}
}

func (e *ClusterE2ETest) InstallCuratedPackagesController() {
	kubeconfig := e.kubeconfigFilePath()
	// TODO Add a test that installs the controller via the CLI.
	ctx := context.Background()
	charts, err := e.PackageConfig.HelmClient.ListCharts(ctx, kubeconfig)
	if err != nil {
		e.T.Fatalf("Unable to list charts: %v", err)
	}
	installed := false
	for _, c := range charts {
		if c == EksaPackagesInstallation {
			installed = true
			break
		}
	}
	if !installed {
		err = e.PackageConfig.HelmClient.InstallChart(ctx, e.PackageConfig.chartName, e.PackageConfig.chartURI, e.PackageConfig.chartVersion, kubeconfig, "eksa-packages", "", e.PackageConfig.chartValues)
		if err != nil {
			e.T.Fatalf("Unable to install %s helm chart on the cluster: %v",
				e.PackageConfig.chartName, err)
		}
	}
}

// SetPackageBundleActive will set the current packagebundle to the active state.
func (e *ClusterE2ETest) SetPackageBundleActive() {
	kubeconfig := e.kubeconfigFilePath()
	pbc, err := e.KubectlClient.GetPackageBundleController(context.Background(), kubeconfig, e.ClusterName)
	if err != nil {
		e.T.Fatalf("Error getting PackageBundleController: %v", err)
	}
	pb, err := e.KubectlClient.GetPackageBundleList(context.Background(), e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("Error getting PackageBundle: %v", err)
	}
	os.Setenv("KUBECONFIG", kubeconfig)
	if pbc.Spec.ActiveBundle != pb[0].ObjectMeta.Name {
		e.RunEKSA([]string{
			"upgrade", "packages",
			"--bundle-version", pb[0].ObjectMeta.Name, "-v=9",
			"--cluster=" + e.ClusterName,
		})
	}
}

// InstallCuratedPackage will install a curated package.
func (e *ClusterE2ETest) InstallCuratedPackage(packageName, packagePrefix, kubeconfig string, opts ...string) {
	os.Setenv("CURATED_PACKAGES_SUPPORT", "true")
	// The package install command doesn't (yet?) have a --kubeconfig flag.
	os.Setenv("KUBECONFIG", kubeconfig)
	e.RunEKSA([]string{
		"install", "package", packageName,
		"--package-name=" + packagePrefix, "-v=9",
		"--cluster=" + e.ClusterName,
		strings.Join(opts, " "),
	})
}

// InstallCuratedPackageFile will install a curated package from a yaml file, this is useful since target namespace isn't supported on the CLI.
func (e *ClusterE2ETest) InstallCuratedPackageFile(packageFile, kubeconfig string, opts ...string) {
	os.Setenv("CURATED_PACKAGES_SUPPORT", "true")
	os.Setenv("KUBECONFIG", kubeconfig)
	e.T.Log("Installing EKS-A Packages file", packageFile)
	e.RunEKSA([]string{
		"apply", "package", "-f", packageFile, "-v=9", strings.Join(opts, " "),
	})
}

func (e *ClusterE2ETest) generatePackageConfig(ns, targetns, prefix, packageName string) []byte {
	yamlB := make([][]byte, 0, 4)
	generatedName := fmt.Sprintf("%s-%s", prefix, packageName)
	if targetns == "" {
		targetns = ns
	}
	ns = fmt.Sprintf("%s-%s", ns, e.ClusterName)
	builtpackage := &packagesv1.Package{
		TypeMeta: metav1.TypeMeta{
			Kind:       packagesv1.PackageKind,
			APIVersion: "packages.eks.amazonaws.com/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      generatedName,
			Namespace: ns,
		},
		Spec: packagesv1.PackageSpec{
			PackageName:     packageName,
			TargetNamespace: targetns,
		},
	}
	builtpackageB, err := yaml.Marshal(builtpackage)
	if err != nil {
		e.T.Fatalf("marshalling package config file: %v", err)
	}
	yamlB = append(yamlB, builtpackageB)
	return templater.AppendYamlResources(yamlB...)
}

// BuildPackageConfigFile will create the file in the test directory for the curated package.
func (e *ClusterE2ETest) BuildPackageConfigFile(packageName, prefix, ns string) string {
	b := e.generatePackageConfig(ns, ns, prefix, packageName)

	writer, err := filewriter.NewWriter(e.ClusterConfigFolder)
	if err != nil {
		e.T.Fatalf("Error creating writer: %v", err)
	}
	packageFile := fmt.Sprintf("%s.yaml", packageName)

	writtenFile, err := writer.Write(packageFile, b, filewriter.PersistentFile)
	if err != nil {
		e.T.Fatalf("Error writing cluster config to file %s: %v", e.ClusterConfigLocation, err)
	}
	return writtenFile
}

func (e *ClusterE2ETest) CreateResource(ctx context.Context, resource string) {
	err := e.KubectlClient.ApplyKubeSpecFromBytes(ctx, e.Cluster(), []byte(resource))
	if err != nil {
		e.T.Fatalf("Failed to create required resource (%s): %v", resource, err)
	}
}

func (e *ClusterE2ETest) UninstallCuratedPackage(packagePrefix string, opts ...string) {
	e.RunEKSA([]string{
		"delete", "package", packagePrefix, "-v=9",
		"--cluster=" + e.ClusterName,
		strings.Join(opts, " "),
	})
}

func (e *ClusterE2ETest) InstallLocalStorageProvisioner() {
	ctx := context.Background()
	_, err := e.KubectlClient.ExecuteCommand(ctx, "apply", "-f",
		"https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.22/deploy/local-path-storage.yaml",
		"--kubeconfig", e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("Error installing local-path-provisioner: %v", err)
	}
}

// WithCluster helps with bringing up and tearing down E2E test clusters.
func (e *ClusterE2ETest) WithCluster(f func(e *ClusterE2ETest)) {
	e.GenerateClusterConfig()
	e.CreateCluster()
	defer e.DeleteCluster()
	f(e)
}

// Like WithCluster but does not delete the cluster. Useful for debugging.
func (e *ClusterE2ETest) WithPersistentCluster(f func(e *ClusterE2ETest)) {
	configPath := e.kubeconfigFilePath()
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		e.GenerateClusterConfig()
		e.CreateCluster()
	}
	f(e)
}

// VerifyHarborPackageInstalled is checking if the harbor package gets installed correctly.
func (e *ClusterE2ETest) VerifyHarborPackageInstalled(prefix string, namespace string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deployments := []string{"core", "jobservice", "nginx", "portal", "registry"}
	statefulsets := []string{"database", "redis", "trivy"}

	var wg sync.WaitGroup
	wg.Add(len(deployments) + len(statefulsets))
	errCh := make(chan error, 1)
	okCh := make(chan string, 1)

	time.Sleep(3 * time.Minute)

	for _, name := range deployments {
		go func(name string) {
			defer wg.Done()
			err := e.KubectlClient.WaitForDeployment(ctx,
				e.Cluster(), "5m", "Available", fmt.Sprintf("%s-harbor-%s", prefix, name), namespace)
			if err != nil {
				errCh <- err
			}
		}(name)
	}
	for _, name := range statefulsets {
		go func(name string) {
			defer wg.Done()
			err := e.KubectlClient.Wait(ctx, e.kubeconfigFilePath(), "5m", "Ready",
				fmt.Sprintf("pods/%s-harbor-%s-0", prefix, name), namespace)
			if err != nil {
				errCh <- err
			}
		}(name)
	}
	go func() {
		wg.Wait()
		okCh <- "completed"
	}()

	select {
	case err := <-errCh:
		e.T.Fatal(err)
	case <-okCh:
		return
	}
}

// VerifyHelloPackageInstalled is checking if the hello eks anywhere package gets installed correctly.
func (e *ClusterE2ETest) VerifyHelloPackageInstalled(name string, mgmtCluster *types.Cluster) {
	ctx := context.Background()
	ns := constants.EksaPackagesName

	e.T.Log("Waiting for Package", name, "To be installed")
	err := e.KubectlClient.WaitForPackagesInstalled(ctx,
		mgmtCluster, name, "5m", fmt.Sprintf("%s-%s", ns, e.ClusterName))
	if err != nil {
		e.T.Fatalf("waiting for hello-eks-anywhere package timed out: %s", err)
	}

	e.T.Log("Waiting for Package", name, "Deployment to be healthy")
	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", "hello-eks-anywhere", ns)
	if err != nil {
		e.T.Fatalf("waiting for hello-eks-anywhere deployment timed out: %s", err)
	}

	svcAddress := name + "." + ns + ".svc.cluster.local"
	e.T.Log("Validate content at endpoint", svcAddress)
	expectedLogs := "Amazon EKS Anywhere"
	e.ValidateEndpointContent(svcAddress, ns, expectedLogs)
}

// VerifyAdotPackageInstalled is checking if the ADOT package gets installed correctly.
func (e *ClusterE2ETest) VerifyAdotPackageInstalled(packageName string, targetNamespace string) {
	ctx := context.Background()
	packageMetadatNamespace := fmt.Sprintf("%s-%s", "eksa-packages", e.ClusterName)

	e.T.Log("Waiting for package", packageName, "to be installed")
	err := e.KubectlClient.WaitForPackagesInstalled(ctx,
		e.Cluster(), packageName, "10m", packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("waiting for adot package install timed out: %s", err)
	}

	e.T.Log("Waiting for package", packageName, "deployment to be available")
	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", fmt.Sprintf("%s-aws-otel-collector", packageName), targetNamespace)
	if err != nil {
		e.T.Fatalf("waiting for adot deployment timed out: %s", err)
	}

	e.T.Log("Reading", packageName, "pod logs")
	adotPodName, err := e.KubectlClient.GetPodNameByLabel(context.TODO(), targetNamespace, "app.kubernetes.io/name=aws-otel-collector", e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("unable to get name of the aws-otel-collector pod: %s", err)
	}
	expectedLogs := "Everything is ready"
	e.MatchLogs(targetNamespace, adotPodName, "aws-otel-collector", expectedLogs, 5*time.Minute)

	podIPAddress, err := e.KubectlClient.GetPodIP(context.TODO(), targetNamespace, adotPodName, e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("unable to get ip of the aws-otel-collector pod: %s", err)
	}
	podFullIPAddress := strings.Trim(podIPAddress, `'"`) + ":8888/metrics"
	e.T.Log("Validate content at endpoint", podFullIPAddress)
	expectedLogs = "otelcol_exporter"
	e.ValidateEndpointContent(podFullIPAddress, targetNamespace, expectedLogs)
}

//go:embed testdata/adot_package_deployment.yaml
var adotPackageDeployment []byte

//go:embed testdata/adot_package_daemonset.yaml
var adotPackageDaemonset []byte

// VerifyAdotPackageDeploymentUpdated is checking if deployment config changes trigger resource reloads correctly.
func (e *ClusterE2ETest) VerifyAdotPackageDeploymentUpdated(packageName string, targetNamespace string) {
	ctx := context.Background()
	packageMetadatNamespace := fmt.Sprintf("%s-%s", "eksa-packages", e.ClusterName)

	// Deploy ADOT as a deployment and scrape the apiservers
	e.T.Log("Apply changes to package", packageName)
	e.T.Log("This will update", packageName, "to be a deployment, and scrape the apiservers")
	err := e.KubectlClient.ApplyKubeSpecFromBytesWithNamespace(ctx, e.Cluster(), adotPackageDeployment, packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("Error upgrading adot package: %s", err)
		return
	}
	time.Sleep(30 * time.Second) // Add sleep to allow package to change state

	e.T.Log("Waiting for package", packageName, "to be updated")
	err = e.KubectlClient.WaitForPackagesInstalled(ctx,
		e.Cluster(), packageName, "10m", packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("waiting for adot package update timed out: %s", err)
	}

	e.T.Log("Waiting for package", packageName, "deployment to be available")
	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", fmt.Sprintf("%s-aws-otel-collector", packageName), targetNamespace)
	if err != nil {
		e.T.Fatalf("waiting for adot deployment timed out: %s", err)
	}

	e.T.Log("Reading", packageName, "pod logs")
	adotPodName, err := e.KubectlClient.GetPodNameByLabel(context.TODO(), targetNamespace, "app.kubernetes.io/name=aws-otel-collector", e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("unable to get name of the aws-otel-collector pod: %s", err)
	}
	logs, err := e.KubectlClient.GetPodLogs(context.TODO(), targetNamespace, adotPodName, "aws-otel-collector", e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("failure getting pod logs %s", err)
	}
	fmt.Printf("Logs from aws-otel-collector pod\n %s\n", logs)
	expectedLogs := "MetricsExporter	{\"kind\": \"exporter\", \"data_type\": \"metrics\", \"name\": \"logging\", \"#metrics\":"
	ok := strings.Contains(logs, expectedLogs)
	if !ok {
		e.T.Fatalf("expected to find %s in the log, got %s", expectedLogs, logs)
	}
}

// VerifyAdotPackageDaemonSetUpdated is checking if daemonset config changes trigger resource reloads correctly.
func (e *ClusterE2ETest) VerifyAdotPackageDaemonSetUpdated(packageName string, targetNamespace string) {
	ctx := context.Background()
	packageMetadatNamespace := fmt.Sprintf("%s-%s", "eksa-packages", e.ClusterName)

	// Deploy ADOT as a daemonset and scrape the node
	e.T.Log("Apply changes to package", packageName)
	e.T.Log("This will update", packageName, "to be a daemonset, and scrape the node")
	err := e.KubectlClient.ApplyKubeSpecFromBytesWithNamespace(ctx, e.Cluster(), adotPackageDaemonset, packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("Error upgrading adot package: %s", err)
		return
	}
	time.Sleep(30 * time.Second) // Add sleep to allow package to change state

	e.T.Log("Waiting for package", packageName, "to be updated")
	err = e.KubectlClient.WaitForPackagesInstalled(ctx,
		e.Cluster(), packageName, "10m", packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("waiting for adot package update timed out: %s", err)
	}

	e.T.Log("Waiting for package", packageName, "daemonset to be rolled out")
	err = retrier.New(6 * time.Minute).Retry(func() error {
		return e.KubectlClient.WaitForResourceRolledout(ctx,
			e.Cluster(), "5m", fmt.Sprintf("%s-aws-otel-collector-agent", packageName), targetNamespace, "daemonset")
	})
	if err != nil {
		e.T.Fatalf("waiting for adot daemonset timed out: %s", err)
	}

	e.T.Log("Reading", packageName, "pod logs")
	adotPodName, err := e.KubectlClient.GetPodNameByLabel(context.TODO(), targetNamespace, "app.kubernetes.io/name=aws-otel-collector", e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("unable to get name of the aws-otel-collector pod: %s", err)
	}
	expectedLogs := "MetricsExporter	{\"kind\": \"exporter\", \"data_type\": \"metrics\", \"name\": \"logging\", \"#metrics\":"
	err = retrier.New(5 * time.Minute).Retry(func() error {
		logs, err := e.KubectlClient.GetPodLogs(context.TODO(), targetNamespace, adotPodName, "aws-otel-collector", e.kubeconfigFilePath())
		if err != nil {
			e.T.Fatalf("failure getting pod logs %s", err)
		}
		fmt.Printf("Logs from aws-otel-collector pod\n %s\n", logs)
		ok := strings.Contains(logs, expectedLogs)
		if !ok {
			return fmt.Errorf("expected to find %s in the log, got %s", expectedLogs, logs)
		}
		return nil
	})
	if err != nil {
		e.T.Fatalf("unable to finish log comparison: %s", err)
	}
}

//go:embed testdata/emissary_listener.yaml
var emisarryListener []byte

//go:embed testdata/emissary_package.yaml
var emisarryPackage []byte

// VerifyEmissaryPackageInstalled is checking if emissary package gets installed correctly.
func (e *ClusterE2ETest) VerifyEmissaryPackageInstalled(name string, mgmtCluster *types.Cluster) {
	ctx := context.Background()
	ns := constants.EksaPackagesName

	e.T.Log("Waiting for Package", name, "To be installed")
	err := e.KubectlClient.WaitForPackagesInstalled(ctx,
		mgmtCluster, name, "5m", fmt.Sprintf("%s-%s", ns, e.ClusterName))
	if err != nil {
		e.T.Fatalf("waiting for emissary package timed out: %s", err)
	}

	e.T.Log("Waiting for Package", name, "Deployment to be healthy")
	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", name, ns)
	if err != nil {
		e.T.Fatalf("waiting for emissary deployment timed out: %s", err)
	}
	svcAddress := name + "-admin." + ns + ".svc.cluster.local" + ":8877/ambassador/v0/check_alive"
	e.T.Log("Validate content at endpoint", svcAddress)
	expectedLogs := "Ambassador is alive and well"
	e.ValidateEndpointContent(svcAddress, ns, expectedLogs)
}

// TestEmissaryPackageRouting is checking if emissary is able to create Ingress, host, and mapping that function correctly.
func (e *ClusterE2ETest) TestEmissaryPackageRouting(name string, mgmtCluster *types.Cluster) {
	ctx := context.Background()
	ns := constants.EksaPackagesName
	err := e.KubectlClient.ApplyKubeSpecFromBytes(ctx, e.Cluster(), emisarryPackage)
	if err != nil {
		e.T.Errorf("Error upgrading emissary package: %v", err)
		return
	}
	e.T.Log("Waiting for Package", name, "To be upgraded")
	err = e.KubectlClient.WaitForPackagesInstalled(ctx,
		mgmtCluster, name, "10m", fmt.Sprintf("%s-%s", ns, e.ClusterName))
	if err != nil {
		e.T.Fatalf("waiting for emissary package upgrade timed out: %s", err)
	}
	err = e.KubectlClient.ApplyKubeSpecFromBytes(ctx, e.Cluster(), emisarryListener)
	if err != nil {
		e.T.Errorf("Error applying roles for oids: %v", err)
		return
	}

	// Functional testing of Emissary Ingress
	ingresssvcAddress := name + "." + ns + ".svc.cluster.local" + "/backend/"
	e.T.Log("Validate content at endpoint", ingresssvcAddress)
	expectedLogs := "quote"
	e.ValidateEndpointContent(ingresssvcAddress, ns, expectedLogs)
}

// VerifyPrometheusPackageInstalled is checking if the Prometheus package gets installed correctly.
func (e *ClusterE2ETest) VerifyPrometheusPackageInstalled(packageName string, targetNamespace string) {
	ctx := context.Background()
	packageMetadatNamespace := fmt.Sprintf("%s-%s", "eksa-packages", e.ClusterName)

	e.T.Log("Waiting for package", packageName, "to be installed")
	err := e.KubectlClient.WaitForPackagesInstalled(ctx,
		e.Cluster(), packageName, "10m", packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("waiting for prometheus package install timed out: %s", err)
	}
}

// VerifyPrometheusPrometheusServerStates is checking if the Prometheus package prometheus-server component is functioning properly.
func (e *ClusterE2ETest) VerifyPrometheusPrometheusServerStates(packageName string, targetNamespace string, mode string) {
	ctx := context.Background()

	e.T.Log("Waiting for package", packageName, mode, "prometheus-server to be rolled out")
	err := retrier.New(6 * time.Minute).Retry(func() error {
		return e.KubectlClient.WaitForResourceRolledout(ctx,
			e.Cluster(), "5m", fmt.Sprintf("%s-server", packageName), targetNamespace, mode)
	})
	if err != nil {
		e.T.Fatalf("waiting for prometheus-server %s timed out: %s", mode, err)
	}

	e.T.Log("Reading package", packageName, "pod prometheus-server logs")
	podName, err := e.KubectlClient.GetPodNameByLabel(context.TODO(), targetNamespace, "app=prometheus,component=server", e.kubeconfigFilePath())
	if err != nil {
		e.T.Fatalf("unable to get name of the prometheus-server pod: %s", err)
	}

	expectedLogs := "Server is ready to receive web requests"
	e.MatchLogs(targetNamespace, podName, "prometheus-server", expectedLogs, 5*time.Minute)
}

// VerifyPrometheusNodeExporterStates is checking if the Prometheus package node-exporter component is functioning properly.
func (e *ClusterE2ETest) VerifyPrometheusNodeExporterStates(packageName string, targetNamespace string) {
	ctx := context.Background()

	e.T.Log("Waiting for package", packageName, "daemonset node-exporter to be rolled out")
	err := retrier.New(6 * time.Minute).Retry(func() error {
		return e.KubectlClient.WaitForResourceRolledout(ctx,
			e.Cluster(), "5m", fmt.Sprintf("%s-node-exporter", packageName), targetNamespace, "daemonset")
	})
	if err != nil {
		e.T.Fatalf("waiting for prometheus daemonset timed out: %s", err)
	}

	svcAddress := packageName + "-node-exporter." + targetNamespace + ".svc.cluster.local" + ":9100/metrics"
	e.T.Log("Validate content at endpoint", svcAddress)
	expectedLogs := "HELP go_gc_duration_seconds A summary of the pause duration of garbage collection cycles"
	e.ValidateEndpointContent(svcAddress, targetNamespace, expectedLogs)
}

//go:embed testdata/prometheus_package_deployment.yaml
var prometheusPackageDeployment []byte

//go:embed testdata/prometheus_package_statefulset.yaml
var prometheusPackageStatefulSet []byte

// ApplyPrometheusPackageServerDeploymentFile is checking if deployment config changes trigger resource reloads correctly.
func (e *ClusterE2ETest) ApplyPrometheusPackageServerDeploymentFile(packageName string, targetNamespace string) {
	e.T.Log("Update", packageName, "to be a deployment, and scrape the api-servers")
	e.ApplyPackageFile(packageName, targetNamespace, prometheusPackageDeployment)
}

// ApplyPrometheusPackageServerStatefulSetFile is checking if statefulset config changes trigger resource reloads correctly.
func (e *ClusterE2ETest) ApplyPrometheusPackageServerStatefulSetFile(packageName string, targetNamespace string) {
	e.T.Log("Update", packageName, "to be a statefulset, and scrape the api-servers")
	e.ApplyPackageFile(packageName, targetNamespace, prometheusPackageStatefulSet)
}

// VerifyPackageControllerNotInstalled is verifying that package controller is not installed.
func (e *ClusterE2ETest) VerifyPackageControllerNotInstalled() {
	ctx := context.Background()

	ns := constants.EksaPackagesName
	packageDeployment := "eks-anywhere-packages"

	_, err := e.KubectlClient.GetDeployment(ctx, packageDeployment, ns, e.Cluster().KubeconfigFile)

	if !apierrors.IsNotFound(err) {
		e.T.Fatalf("found deployment for package controller in workload cluster %s : %s", e.ClusterName, err)
	}
}

// VerifyAutoScalerPackageInstalled is verifying that the autoscaler package is installed and deployed.
func (e *ClusterE2ETest) VerifyAutoScalerPackageInstalled(name string, targetNamespace string, mgmtCluster *types.Cluster) {
	ctx := context.Background()
	deploymentName := "cluster-autoscaler-clusterapi-cluster-autoscaler"

	e.T.Log("Waiting for Package", name, "To be installed")
	err := e.KubectlClient.WaitForPackagesInstalled(ctx,
		mgmtCluster, name, "5m", fmt.Sprintf("%s-%s", targetNamespace, e.ClusterName))
	if err != nil {
		e.T.Fatalf("waiting for Autoscaler Package to be avaliable")
	}

	e.T.Log("Waiting for Package", name, "Deployment to be healthy")
	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", deploymentName, targetNamespace)
	if err != nil {
		e.T.Fatalf("waiting for cluster-autoscaler deployment timed out: %s", err)
	}
}

// VerifyMetricServerPackageInstalled is verifying that metrics-server is installed and deployed.
func (e *ClusterE2ETest) VerifyMetricServerPackageInstalled(name string, targetNamespace string, mgmtCluster *types.Cluster) {
	ctx := context.Background()
	deploymentName := "metrics-server"

	e.T.Log("Waiting for Package", name, "To be installed")
	err := e.KubectlClient.WaitForPackagesInstalled(ctx,
		mgmtCluster, name, "5m", fmt.Sprintf("%s-%s", targetNamespace, e.ClusterName))
	if err != nil {
		e.T.Fatalf("waiting for Metric Server Package to be avaliable")
	}

	e.T.Log("Waiting for Package", name, "Deployment to be healthy")
	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", deploymentName, targetNamespace)
	if err != nil {
		e.T.Fatalf("waiting for Metric Server deployment timed out: %s", err)
	}
}

//go:embed testdata/autoscaler_package.yaml
var autoscalerPackageDeploymentTemplate string

//go:embed testdata/metrics_server_package.yaml
var metricsServerPackageDeploymentTemplate string

// InstallAutoScalerWithMetricServer installs autoscaler and metrics-server with a given target namespace.
func (e *ClusterE2ETest) InstallAutoScalerWithMetricServer(targetNamespace string) {
	ctx := context.Background()
	packageInstallNamespace := fmt.Sprintf("%s-%s", "eksa-packages", e.ClusterName)
	data := map[string]interface{}{
		"targetNamespace": targetNamespace,
		"clusterName":     e.Cluster().Name,
	}

	metricsServerPackageDeployment, err := templater.Execute(metricsServerPackageDeploymentTemplate, data)
	if err != nil {
		e.T.Fatalf("Failed creating metrics-erver Package Deployment: %s", err)
	}

	err = e.KubectlClient.ApplyKubeSpecFromBytesWithNamespace(ctx, e.Cluster(), metricsServerPackageDeployment,
		packageInstallNamespace)
	if err != nil {
		e.T.Fatalf("Error installing metrics-sserver pacakge: %s", err)
	}

	autoscalerPackageDeployment, err := templater.Execute(autoscalerPackageDeploymentTemplate, data)
	if err != nil {
		e.T.Fatalf("Failed creating autoscaler Package Deployment: %s", err)
	}

	err = e.KubectlClient.ApplyKubeSpecFromBytesWithNamespace(ctx, e.Cluster(), autoscalerPackageDeployment,
		packageInstallNamespace)
	if err != nil {
		e.T.Fatalf("Error installing cluster autoscaler pacakge: %s", err)
	}
}

// CombinedAutoScalerMetricServerTest verifies that new nodes are spun up after using a HPA to scale a deployment.
func (e *ClusterE2ETest) CombinedAutoScalerMetricServerTest(autoscalerName string, metricServerName string, targetNamespace string, mgmtCluster *types.Cluster) {
	ctx := context.Background()
	ns := "default"
	name := "hpa-busybox-test"
	machineDeploymentName := e.ClusterName + "-" + "md-0"

	e.VerifyMetricServerPackageInstalled(metricServerName, targetNamespace, mgmtCluster)
	e.VerifyAutoScalerPackageInstalled(autoscalerName, targetNamespace, mgmtCluster)

	e.T.Log("Metrics Server and Cluster Autoscaler ready")

	err := e.KubectlClient.ApplyKubeSpecFromBytes(ctx, mgmtCluster, hpaBusybox)
	if err != nil {
		e.T.Fatalf("Failed to apply hpa busybox load %s", err)
	}

	e.T.Log("Deploying test workload")

	err = e.KubectlClient.WaitForDeployment(ctx,
		e.Cluster(), "5m", "Available", name, ns)
	if err != nil {
		e.T.Fatalf("Failed waiting for test workload deployent %s", err)
	}

	params := []string{"autoscale", "deployment", name, "--cpu-percent=50", "--min=1", "--max=20", "--kubeconfig", e.kubeconfigFilePath()}
	_, err = e.KubectlClient.ExecuteCommand(ctx, params...)
	if err != nil {
		e.T.Fatalf("Failed to autoscale deployent: %s", err)
	}

	e.T.Log("Waiting for machinedeployment to begin scaling up")
	err = e.KubectlClient.WaitJSONPathLoop(ctx, mgmtCluster.KubeconfigFile, "5m", "status.phase", "ScalingUp",
		fmt.Sprintf("machinedeployments.cluster.x-k8s.io/%s", machineDeploymentName), constants.EksaSystemNamespace)
	if err != nil {
		e.T.Fatalf("Failed to get ScalingUp phase for machinedeployment: %s", err)
	}

	e.T.Log("Waiting for machinedeployment to finish scaling up")
	err = e.KubectlClient.WaitJSONPathLoop(ctx, mgmtCluster.KubeconfigFile, "10m", "status.phase", "Running",
		fmt.Sprintf("machinedeployments.cluster.x-k8s.io/%s", machineDeploymentName), constants.EksaSystemNamespace)
	if err != nil {
		e.T.Fatalf("Failed to get Running phase for machinedeployment: %s", err)
	}

	err = e.KubectlClient.WaitForMachineDeploymentReady(ctx, mgmtCluster, "2m",
		machineDeploymentName)
	if err != nil {
		e.T.Fatalf("Machine deployment stuck in scaling up: %s", err)
	}

	e.T.Log("Finished scaling up machines")
}

// ValidateClusterState runs a set of validations against the cluster to identify an invalid cluster state.
func (e *ClusterE2ETest) ValidateClusterState() {
	e.T.Logf("Validating cluster %s", e.ClusterName)
	ctx := context.Background()
	err := retrier.Retry(60, 5*time.Second, func() error {
		return e.buildClusterValidator(ctx)
	})
	if err != nil {
		e.T.Fatalf("failed to build cluster validator %v", err)
	}

	if e.ClusterConfig.Cluster.IsManaged() {
		e.clusterValidator.WithWorkloadClusterValidations()
	}
	e.clusterValidator.WithExpectedObjectsExist()

	providerValidations := e.Provider.ClusterValidations()
	e.clusterValidator.WithValidations(providerValidations...)

	if err := e.clusterValidator.Validate(ctx); err != nil {
		e.T.Fatalf("failed to validate cluster %v", err)
	}
}

// ApplyPackageFile is applying a package file in the cluster.
func (e *ClusterE2ETest) ApplyPackageFile(packageName string, targetNamespace string, PackageFile []byte) {
	ctx := context.Background()
	packageMetadatNamespace := fmt.Sprintf("%s-%s", "eksa-packages", e.ClusterName)

	e.T.Log("Apply changes to package", packageName)
	err := e.KubectlClient.ApplyKubeSpecFromBytesWithNamespace(ctx, e.Cluster(), PackageFile, packageMetadatNamespace)
	if err != nil {
		e.T.Fatalf("Error upgrading package: %s", err)
		return
	}
	time.Sleep(30 * time.Second) // Add sleep to allow package to change state
}

// CurlEndpointByBusyBox creates a busybox pod with command to curl the target endpoint,
// and returns the created busybox pod name.
func (e *ClusterE2ETest) CurlEndpointByBusyBox(endpoint string, namespace string) string {
	ctx := context.Background()

	e.T.Log("Launching Busybox pod to curl endpoint", endpoint)
	randomname := fmt.Sprintf("%s-%s", "busybox-test", utilrand.String(7))
	busyBoxPodName, err := e.KubectlClient.RunBusyBoxPod(context.TODO(),
		namespace, randomname, e.kubeconfigFilePath(), []string{"curl", endpoint})
	if err != nil {
		e.T.Fatalf("error launching busybox pod: %s", err)
	}

	err = e.KubectlClient.WaitForPodCompleted(ctx,
		e.Cluster(), busyBoxPodName, "5m", namespace)
	if err != nil {
		e.T.Fatalf("waiting for busybox pod %s timed out: %s", busyBoxPodName, err)
	}

	return busyBoxPodName
}

// MatchLogs matches the log from a container to the expected content. Given it
// takes time for logs to be populated, a retrier with configurable timeout duration
// is added.
func (e *ClusterE2ETest) MatchLogs(targetNamespace string, targetPodName string,
	targetContainerName string, expectedLogs string, timeout time.Duration,
) {
	e.T.Logf("Match logs for pod %s, container %s in namespace %s", targetPodName,
		targetContainerName, targetNamespace)

	err := retrier.New(timeout).Retry(func() error {
		logs, err := e.KubectlClient.GetPodLogs(context.TODO(), targetNamespace,
			targetPodName, targetContainerName, e.kubeconfigFilePath())
		if err != nil {
			return fmt.Errorf("failure getting pod logs %s", err)
		}
		fmt.Printf("Logs from pod\n %s\n", logs)
		ok := strings.Contains(logs, expectedLogs)
		if !ok {
			return fmt.Errorf("expected to find %s in the log, got %s", expectedLogs, logs)
		}
		return nil
	})
	if err != nil {
		e.T.Fatalf("unable to match logs: %s", err)
	}
}

// ValidateEndpointContent validates the contents at the target endpoint.
func (e *ClusterE2ETest) ValidateEndpointContent(endpoint string, namespace string, expectedContent string) {
	busyBoxPodName := e.CurlEndpointByBusyBox(endpoint, namespace)
	e.MatchLogs(namespace, busyBoxPodName, busyBoxPodName, expectedContent, 5*time.Minute)
}

// ValidateClusterDelete verifies the cluster has been deleted.
func (e *ClusterE2ETest) ValidateClusterDelete() {
	ctx := context.Background()
	e.T.Logf("Validating cluster deletion %s", e.ClusterName)
	if e.clusterValidator == nil {
		return
	}

	e.clusterValidator.Reset()
	if e.ClusterConfig.Cluster.IsManaged() {
		e.clusterValidator.WithClusterDoesNotExist()
	}
	if err := e.clusterValidator.Validate(ctx); err != nil {
		e.T.Fatalf("failed to validate cluster deletion %v", err)
	}
	e.clusterValidator = nil
}

func (e *ClusterE2ETest) buildClusterValidator(ctx context.Context) error {
	mc, err := kubernetes.NewRuntimeClientFromFileName(e.managementKubeconfigFilePath())
	if err != nil {
		return fmt.Errorf("failed to create management cluster client: %s", err)
	}
	c := mc
	if e.managementKubeconfigFilePath() != e.kubeconfigFilePath() {
		c, err = kubernetes.NewRuntimeClientFromFileName(e.kubeconfigFilePath())
	}
	if err != nil {
		return fmt.Errorf("failed to create cluster client: %s", err)
	}

	spec, err := e.buildClusterSpec(ctx, c, e.ClusterConfig)
	if err != nil {
		return fmt.Errorf("failed to build cluster spec %s", err)
	}

	e.clusterValidator = NewClusterValidator(func(cv *ClusterValidator) {
		cv.Config.ClusterClient = c
		cv.Config.ManagementClusterClient = mc
		cv.Config.ClusterSpec = spec
	})

	return nil
}

func (e *ClusterE2ETest) buildClusterSpec(ctx context.Context, client client.Client, config *cluster.Config) (*cluster.Spec, error) {
	if config.Cluster.IsManaged() {
		spec := cluster.NewSpec(func(spec *cluster.Spec) {
			spec.Config = config
		})

		return spec, nil
	}

	parsedCluster, err := e.parseClusterConfigWithDefaultsFromDisk()
	if err != nil {
		return nil, fmt.Errorf("failed to parse cluster config with defaults from disk: %v", err)
	}

	config.Cluster.Spec.BundlesRef = parsedCluster.Spec.BundlesRef
	if config.Cluster.Namespace == "" {
		config.Cluster.Namespace = "default"
	}
	spec, err := cluster.BuildSpecFromConfig(ctx, clientutil.NewKubeClient(client), config)
	if err != nil {
		return nil, fmt.Errorf("failed to build cluster spec from config: %s", err)
	}

	return spec, err
}
