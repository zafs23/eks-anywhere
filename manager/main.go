package main

import (
	"context"
	"flag"
	"os"

	eksdv1alpha1 "github.com/aws/eks-distro-build-tooling/release/api/v1alpha1"
	etcdv1 "github.com/aws/etcdadm-controller/api/v1beta1"
	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	cloudstackv1 "sigs.k8s.io/cluster-api-provider-cloudstack/api/v1beta2"
	vspherev1 "sigs.k8s.io/cluster-api-provider-vsphere/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	kubeadmv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	addonsv1 "sigs.k8s.io/cluster-api/exp/addons/api/v1beta1"
	dockerv1 "sigs.k8s.io/cluster-api/test/infrastructure/docker/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/aws/eks-anywhere/controllers"
	anywherev1 "github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/clusterapi"
	"github.com/aws/eks-anywhere/pkg/features"
	snowv1 "github.com/aws/eks-anywhere/pkg/providers/snow/api/v1beta1"
	releasev1 "github.com/aws/eks-anywhere/release/api/v1alpha1"
)

var scheme = runtime.NewScheme()

const WEBHOOK = "webhook"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(anywherev1.AddToScheme(scheme))
	utilruntime.Must(releasev1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(clusterctlv1.AddToScheme(scheme))
	utilruntime.Must(controlplanev1.AddToScheme(scheme))
	utilruntime.Must(vspherev1.AddToScheme(scheme))
	utilruntime.Must(cloudstackv1.AddToScheme(scheme))
	utilruntime.Must(dockerv1.AddToScheme(scheme))
	utilruntime.Must(etcdv1.AddToScheme(scheme))
	utilruntime.Must(kubeadmv1.AddToScheme(scheme))
	utilruntime.Must(eksdv1alpha1.AddToScheme(scheme))
	utilruntime.Must(snowv1.AddToScheme(scheme))
	utilruntime.Must(addonsv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

type config struct {
	metricsAddr          string
	enableLeaderElection bool
	probeAddr            string
	gates                []string
	logging              *logsv1.LoggingConfiguration
}

func newConfig() *config {
	c := &config{
		logging: logsv1.NewLoggingConfiguration(),
	}
	c.logging.Format = logsv1.JSONLogFormat
	c.logging.Verbosity = logsv1.VerbosityLevel(4)

	return c
}

func initFlags(fs *pflag.FlagSet, config *config) {
	logsv1.AddFlags(config.logging, fs)

	fs.StringVar(&config.metricsAddr, "metrics-bind-address", "localhost:8080", "The address the metric endpoint binds to.")
	fs.StringVar(&config.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&config.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	fs.StringSliceVar(&config.gates, "feature-gates", []string{}, "A set of key=value pairs that describe feature gates for alpha/experimental features. ")
}

func main() {
	config := newConfig()
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	initFlags(pflag.CommandLine, config)
	pflag.Parse()

	// Temporary logger for initialization
	setupLog := ctrl.Log.WithName("setup")

	if err := logsv1.ValidateAndApply(config.logging, nil); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// klog.Background will automatically use the right logger.
	ctrl.SetLogger(klog.Background())
	// Once controller-runtime logger has been setup correctly, retrieve again
	setupLog = ctrl.Log.WithName("setup")

	features.FeedGates(config.gates)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     config.metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: config.probeAddr,
		LeaderElection:         config.enableLeaderElection,
		LeaderElectionID:       "f64ae69e.eks.amazonaws.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup the context that's going to be used in controllers and for the manager.
	ctx := ctrl.SetupSignalHandler()

	closer := setupReconcilers(ctx, setupLog, mgr)
	defer func() {
		setupLog.Info("Closing reconciler dependencies")
		if err := closer.Close(ctx); err != nil {
			setupLog.Error(err, "Failed closing reconciler dependencies")
		}
	}()
	setupWebhooks(setupLog, mgr)
	setupChecks(setupLog, mgr)
	//+kubebuilder:scaffold:builder

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type closable interface {
	Close(ctx context.Context) error
}

type noOpCloser struct{}

func (c noOpCloser) Close(ctx context.Context) error {
	return nil
}

func setupReconcilers(ctx context.Context, setupLog logr.Logger, mgr ctrl.Manager) closable {
	if features.IsActive(features.FullLifecycleAPI()) {
		return setupFullLifecycleReconcilers(ctx, setupLog, mgr)
	}

	setupLog.Info("Setting up legacy cluster controller")
	setupLegacyClusterReconciler(setupLog, mgr)
	return noOpCloser{}
}

func setupFullLifecycleReconcilers(ctx context.Context, setupLog logr.Logger, mgr ctrl.Manager) closable {
	setupLog.Info("Reading CAPI providers")
	providers, err := clusterapi.GetProviders(ctx, mgr.GetAPIReader())
	if err != nil {
		setupLog.Error(err, "unable to read installed providers")
		os.Exit(1)
	}

	factory := controllers.NewFactory(ctrl.Log, mgr).
		WithClusterReconciler(providers).
		WithVSphereDatacenterReconciler().
		WithSnowMachineConfigReconciler()

	reconcilers, err := factory.Build(ctx)
	if err != nil {
		setupLog.Error(err, "unable to build reconcilers")
		os.Exit(1)
	}

	failed := false
	setupLog.Info("Setting up cluster controller")
	if err := (reconcilers.ClusterReconciler).SetupWithManager(mgr, setupLog); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", anywherev1.ClusterKind)
		failed = true
	}

	setupLog.Info("Setting up vspheredatacenter controller")
	if err := (reconcilers.VSphereDatacenterReconciler).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", anywherev1.VSphereDatacenterKind)
		failed = true
	}

	setupLog.Info("Setting up snowmachineconfig controller")
	if err := (reconcilers.SnowMachineConfigReconciler).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", anywherev1.SnowMachineConfigKind)
		failed = true
	}

	if failed {
		if err := factory.Close(ctx); err != nil {
			setupLog.Error(err, "Failed closing controller factory")
		}
		os.Exit(1)
	}

	return factory
}

func setupLegacyClusterReconciler(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := (controllers.NewClusterReconcilerLegacy(
		mgr.GetClient(),
		ctrl.Log.WithName("controllers").WithName(anywherev1.ClusterKind),
		mgr.GetScheme(),
	)).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create legacy cluster controller", "controller", anywherev1.ClusterKind)
		os.Exit(1)
	}
}

func setupWebhooks(setupLog logr.Logger, mgr ctrl.Manager) {
	setupCoreWebhooks(setupLog, mgr)
	setupVSphereWebhooks(setupLog, mgr)
	setupCloudstackWebhooks(setupLog, mgr)
	setupSnowWebhooks(setupLog, mgr)
	setupTinkerbellWebhooks(setupLog, mgr)
}

func setupCoreWebhooks(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := (&anywherev1.Cluster{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.ClusterKind)
		os.Exit(1)
	}
	if err := (&anywherev1.GitOpsConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.GitOpsConfigKind)
		os.Exit(1)
	}
	if err := (&anywherev1.FluxConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.FluxConfigKind)
		os.Exit(1)
	}
	if err := (&anywherev1.OIDCConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.OIDCConfigKind)
		os.Exit(1)
	}
	if err := (&anywherev1.AWSIamConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.AWSIamConfigKind)
		os.Exit(1)
	}
}

func setupVSphereWebhooks(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := (&anywherev1.VSphereDatacenterConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.VSphereDatacenterKind)
		os.Exit(1)
	}
	if err := (&anywherev1.VSphereMachineConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.VSphereMachineConfigKind)
		os.Exit(1)
	}
}

func setupCloudstackWebhooks(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := (&anywherev1.CloudStackDatacenterConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.CloudStackDatacenterKind)
		os.Exit(1)
	}
	if err := (&anywherev1.CloudStackMachineConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.CloudStackMachineConfigKind)
		os.Exit(1)
	}
}

func setupSnowWebhooks(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := (&anywherev1.SnowMachineConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.SnowMachineConfigKind)
		os.Exit(1)
	}
	if err := (&anywherev1.SnowDatacenterConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "SnowDatacenterConfig")
		os.Exit(1)
	}
	if err := (&anywherev1.SnowIPPool{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "SnowIPPool")
		os.Exit(1)
	}
}

func setupTinkerbellWebhooks(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := (&anywherev1.TinkerbellDatacenterConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.TinkerbellDatacenterKind)
		os.Exit(1)
	}
	if err := (&anywherev1.TinkerbellMachineConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", WEBHOOK, anywherev1.TinkerbellMachineConfigKind)
		os.Exit(1)
	}
}

func setupChecks(setupLog logr.Logger, mgr ctrl.Manager) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}
