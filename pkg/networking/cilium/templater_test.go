package cilium_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"

	"github.com/aws/eks-anywhere/internal/test"
	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/networking/cilium"
	"github.com/aws/eks-anywhere/pkg/networking/cilium/mocks"
	"github.com/aws/eks-anywhere/pkg/retrier"
	"github.com/aws/eks-anywhere/pkg/semver"
)

type templaterTest struct {
	*WithT
	ctx                     context.Context
	t                       *cilium.Templater
	h                       *mocks.MockHelm
	manifest                []byte
	uri, version, namespace string
	spec, currentSpec       *cluster.Spec
}

func newtemplaterTest(t *testing.T) *templaterTest {
	ctrl := gomock.NewController(t)
	h := mocks.NewMockHelm(ctrl)
	return &templaterTest{
		WithT:     NewWithT(t),
		ctx:       context.Background(),
		h:         h,
		t:         cilium.NewTemplater(h),
		manifest:  []byte("manifestContent"),
		uri:       "oci://public.ecr.aws/isovalent/cilium",
		version:   "1.9.11-eksa.1",
		namespace: "kube-system",
		currentSpec: test.NewClusterSpec(func(s *cluster.Spec) {
			s.VersionsBundle.Cilium.Version = "v1.9.10-eksa.1"
			s.VersionsBundle.Cilium.Cilium.URI = "public.ecr.aws/isovalent/cilium:v1.9.10-eksa.1"
			s.VersionsBundle.Cilium.Operator.URI = "public.ecr.aws/isovalent/operator-generic:v1.9.10-eksa.1"
			s.VersionsBundle.Cilium.HelmChart.URI = "public.ecr.aws/isovalent/cilium:1.9.10-eksa.1"
			s.VersionsBundle.KubeDistro.Kubernetes.Tag = "v1.22.5-eks-1-22-9"
			s.Cluster.Spec.ClusterNetwork.CNIConfig = &v1alpha1.CNIConfig{Cilium: &v1alpha1.CiliumConfig{}}
		}),
		spec: test.NewClusterSpec(func(s *cluster.Spec) {
			s.VersionsBundle.Cilium.Version = "v1.9.11-eksa.1"
			s.VersionsBundle.Cilium.Cilium.URI = "public.ecr.aws/isovalent/cilium:v1.9.11-eksa.1"
			s.VersionsBundle.Cilium.Operator.URI = "public.ecr.aws/isovalent/operator-generic:v1.9.11-eksa.1"
			s.VersionsBundle.Cilium.HelmChart.URI = "public.ecr.aws/isovalent/cilium:1.9.11-eksa.1"
			s.VersionsBundle.KubeDistro.Kubernetes.Tag = "v1.22.5-eks-1-22-9"
			s.Cluster.Spec.ClusterNetwork.CNIConfig = &v1alpha1.CNIConfig{Cilium: &v1alpha1.CiliumConfig{}}
		}),
	}
}

func (t *templaterTest) expectHelmTemplateWith(wantValues gomock.Matcher, kubeVersion string) *gomock.Call {
	return t.h.EXPECT().Template(t.ctx, t.uri, t.version, t.namespace, wantValues, kubeVersion)
}

func eqMap(m map[string]interface{}) gomock.Matcher {
	return &mapMatcher{m: m}
}

// mapMacher implements a gomock matcher for maps
// transforms any map or struct into a map[string]interface{} and uses DeepEqual to compare.
type mapMatcher struct {
	m map[string]interface{}
}

func (e *mapMatcher) Matches(x interface{}) bool {
	xJson, err := json.Marshal(x)
	if err != nil {
		return false
	}
	xMap := &map[string]interface{}{}
	err = json.Unmarshal(xJson, xMap)
	if err != nil {
		return false
	}

	return reflect.DeepEqual(e.m, *xMap)
}

func (e *mapMatcher) String() string {
	return fmt.Sprintf("matches map %v", e.m)
}

func TestTemplaterGenerateUpgradePreflightManifestSuccess(t *testing.T) {
	wantValues := map[string]interface{}{
		"cni": map[string]interface{}{
			"chainingMode": "portmap",
		},
		"ipam": map[string]interface{}{
			"mode": "kubernetes",
		},
		"identityAllocationMode": "crd",
		"prometheus": map[string]interface{}{
			"enabled": true,
		},
		"rollOutCiliumPods": true,
		"tunnel":            "geneve",
		"image": map[string]interface{}{
			"repository": "public.ecr.aws/isovalent/cilium",
			"tag":        "v1.9.11-eksa.1",
		},
		"operator": map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "public.ecr.aws/isovalent/operator",
				"tag":        "v1.9.11-eksa.1",
			},
			"prometheus": map[string]interface{}{
				"enabled": true,
			},
			"enabled": false,
		},
		"preflight": map[string]interface{}{
			"enabled": true,
			"image": map[string]interface{}{
				"repository": "public.ecr.aws/isovalent/cilium",
				"tag":        "v1.9.11-eksa.1",
			},
		},
		"agent": false,
	}

	tt := newtemplaterTest(t)
	tt.expectHelmTemplateWith(eqMap(wantValues), "1.22").Return(tt.manifest, nil)

	tt.Expect(tt.t.GenerateUpgradePreflightManifest(tt.ctx, tt.spec)).To(Equal(tt.manifest), "templater.GenerateUpgradePreflightManifest() should return right manifest")
}

func TestTemplaterGenerateUpgradePreflightManifestError(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.expectHelmTemplateWith(gomock.Any(), "1.22").Return(nil, errors.New("error from helm")) // Using any because we only want to test the returned error

	_, err := tt.t.GenerateUpgradePreflightManifest(tt.ctx, tt.spec)
	tt.Expect(err).To(HaveOccurred(), "templater.GenerateUpgradePreflightManifest() should fail")
	tt.Expect(err).To(MatchError(ContainSubstring("error from helm")))
}

func TestTemplaterGenerateUpgradePreflightManifestInvalidKubeVersion(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.spec.VersionsBundle.KubeDistro.Kubernetes.Tag = "v1-invalid"
	_, err := tt.t.GenerateUpgradePreflightManifest(tt.ctx, tt.spec)
	tt.Expect(err).To(HaveOccurred(), "templater.GenerateUpgradePreflightManifest() should fail")
	tt.Expect(err).To(MatchError(ContainSubstring("invalid major version in semver")))
}

func TestTemplaterGenerateManifestSuccess(t *testing.T) {
	wantValues := map[string]interface{}{
		"cni": map[string]interface{}{
			"chainingMode": "portmap",
		},
		"ipam": map[string]interface{}{
			"mode": "kubernetes",
		},
		"identityAllocationMode": "crd",
		"prometheus": map[string]interface{}{
			"enabled": true,
		},
		"rollOutCiliumPods": true,
		"tunnel":            "geneve",
		"image": map[string]interface{}{
			"repository": "public.ecr.aws/isovalent/cilium",
			"tag":        "v1.9.11-eksa.1",
		},
		"operator": map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "public.ecr.aws/isovalent/operator",
				"tag":        "v1.9.11-eksa.1",
			},
			"prometheus": map[string]interface{}{
				"enabled": true,
			},
		},
	}

	tt := newtemplaterTest(t)
	tt.expectHelmTemplateWith(eqMap(wantValues), "1.22").Return(tt.manifest, nil)

	tt.Expect(tt.t.GenerateManifest(tt.ctx, tt.spec)).To(Equal(tt.manifest), "templater.GenerateManifest() should return right manifest")
}

func TestTemplaterGenerateManifestPolicyEnforcementModeSuccess(t *testing.T) {
	wantValues := map[string]interface{}{
		"cni": map[string]interface{}{
			"chainingMode": "portmap",
		},
		"ipam": map[string]interface{}{
			"mode": "kubernetes",
		},
		"identityAllocationMode": "crd",
		"prometheus": map[string]interface{}{
			"enabled": true,
		},
		"rollOutCiliumPods": true,
		"tunnel":            "geneve",
		"image": map[string]interface{}{
			"repository": "public.ecr.aws/isovalent/cilium",
			"tag":        "v1.9.11-eksa.1",
		},
		"operator": map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "public.ecr.aws/isovalent/operator",
				"tag":        "v1.9.11-eksa.1",
			},
			"prometheus": map[string]interface{}{
				"enabled": true,
			},
		},
		"policyEnforcementMode": "always",
	}

	tt := newtemplaterTest(t)
	tt.spec.Cluster.Spec.ManagementCluster.Name = "managed"
	tt.spec.Cluster.Spec.ClusterNetwork.CNIConfig.Cilium.PolicyEnforcementMode = v1alpha1.CiliumPolicyModeAlways
	tt.expectHelmTemplateWith(eqMap(wantValues), "1.22").Return(tt.manifest, nil)

	gotManifest, err := tt.t.GenerateManifest(tt.ctx, tt.spec)
	tt.Expect(err).NotTo(HaveOccurred())
	test.AssertContentToFile(t, string(gotManifest), "testdata/manifest_network_policy.yaml")
}

func TestTemplaterGenerateManifestError(t *testing.T) {
	expectedAttempts := 2
	tt := newtemplaterTest(t)
	tt.expectHelmTemplateWith(gomock.Any(), "1.22").Return(nil, errors.New("error from helm")).Times(expectedAttempts) // Using any because we only want to test the returned error

	_, err := tt.t.GenerateManifest(tt.ctx, tt.spec, cilium.WithRetrier(retrier.NewWithMaxRetries(expectedAttempts, 0)))
	tt.Expect(err).To(HaveOccurred(), "templater.GenerateManifest() should fail")
	tt.Expect(err).To(MatchError(ContainSubstring("error from helm")))
}

func TestTemplaterGenerateManifestInvalidKubeVersion(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.spec.VersionsBundle.KubeDistro.Kubernetes.Tag = "v1-invalid"
	_, err := tt.t.GenerateManifest(tt.ctx, tt.spec)
	tt.Expect(err).To(HaveOccurred(), "templater.GenerateManifest() should fail")
	tt.Expect(err).To(MatchError(ContainSubstring("invalid major version in semver")))
}

func TestTemplaterGenerateManifestUpgradeSameKubernetesVersionSuccess(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.expectHelmTemplateWith(eqMap(wantUpgradeValues()), "1.22").Return(tt.manifest, nil)

	oldCiliumVersion, err := semver.New(tt.currentSpec.VersionsBundle.Cilium.Version)
	tt.Expect(err).NotTo(HaveOccurred())

	tt.Expect(
		tt.t.GenerateManifest(tt.ctx, tt.spec,
			cilium.WithUpgradeFromVersion(*oldCiliumVersion),
		),
	).To(Equal(tt.manifest), "templater.GenerateUpgradeManifest() should return right manifest")
}

func TestTemplaterGenerateManifestUpgradeNewKubernetesVersionSuccess(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.expectHelmTemplateWith(eqMap(wantUpgradeValues()), "1.21").Return(tt.manifest, nil)

	oldCiliumVersion, err := semver.New(tt.currentSpec.VersionsBundle.Cilium.Version)
	tt.Expect(err).NotTo(HaveOccurred())

	tt.Expect(
		tt.t.GenerateManifest(tt.ctx, tt.spec,
			cilium.WithKubeVersion("1.21"),
			cilium.WithUpgradeFromVersion(*oldCiliumVersion),
		),
	).To(Equal(tt.manifest), "templater.GenerateUpgradeManifest() should return right manifest")
}

func wantUpgradeValues() map[string]interface{} {
	return map[string]interface{}{
		"cni": map[string]interface{}{
			"chainingMode": "portmap",
		},
		"ipam": map[string]interface{}{
			"mode": "kubernetes",
		},
		"identityAllocationMode": "crd",
		"prometheus": map[string]interface{}{
			"enabled": true,
		},
		"rollOutCiliumPods": true,
		"tunnel":            "geneve",
		"image": map[string]interface{}{
			"repository": "public.ecr.aws/isovalent/cilium",
			"tag":        "v1.9.11-eksa.1",
		},
		"operator": map[string]interface{}{
			"image": map[string]interface{}{
				"repository": "public.ecr.aws/isovalent/operator",
				"tag":        "v1.9.11-eksa.1",
			},
			"prometheus": map[string]interface{}{
				"enabled": true,
			},
		},
		"upgradeCompatibility": "1.9",
	}
}

func TestTemplaterGenerateNetworkPolicy(t *testing.T) {
	tests := []struct {
		name                    string
		k8sVersion              string
		selfManaged             bool
		gitopsEnabled           bool
		infraProviderNamespaces []string
		wantNetworkPolicyFile   string
	}{
		{
			name:                    "CAPV mgmt cluster",
			k8sVersion:              "v1.21.9-eks-1-21-10",
			selfManaged:             true,
			gitopsEnabled:           false,
			infraProviderNamespaces: []string{"capv-system"},
			wantNetworkPolicyFile:   "testdata/network_policy_mgmt_capv.yaml",
		},
		{
			name:                    "CAPT mgmt cluster with flux",
			k8sVersion:              "v1.21.9-eks-1-21-10",
			selfManaged:             true,
			gitopsEnabled:           true,
			infraProviderNamespaces: []string{"capt-system"},
			wantNetworkPolicyFile:   "testdata/network_policy_mgmt_capt_flux.yaml",
		},
		{
			name:                    "workload cluster 1.20",
			k8sVersion:              "v1.20.9-eks-1-20-10",
			selfManaged:             false,
			gitopsEnabled:           false,
			infraProviderNamespaces: []string{},
			wantNetworkPolicyFile:   "testdata/network_policy_workload_120.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			temp := newtemplaterTest(t)
			temp.spec.VersionsBundle.KubeDistro.Kubernetes.Tag = tt.k8sVersion
			if !tt.selfManaged {
				temp.spec.Cluster.Spec.ManagementCluster.Name = "managed"
			}
			if tt.gitopsEnabled {
				temp.spec.Cluster.Spec.GitOpsRef = &v1alpha1.Ref{
					Kind: v1alpha1.FluxConfigKind,
					Name: "eksa-unit-test",
				}
				temp.spec.Config.GitOpsConfig = &v1alpha1.GitOpsConfig{
					Spec: v1alpha1.GitOpsConfigSpec{
						Flux: v1alpha1.Flux{Github: v1alpha1.Github{FluxSystemNamespace: "flux-system"}},
					},
				}
			}
			networkPolicy, err := temp.t.GenerateNetworkPolicyManifest(temp.spec, tt.infraProviderNamespaces)
			if err != nil {
				t.Fatalf("failed to generate network policy template: %v", err)
			}
			test.AssertContentToFile(t, string(networkPolicy), tt.wantNetworkPolicyFile)
		})
	}
}

func TestTemplaterGenerateManifestForSingleNodeCluster(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.spec.Cluster.Spec.WorkerNodeGroupConfigurations = nil
	tt.spec.Cluster.Spec.ControlPlaneConfiguration.Count = 1

	tt.h.EXPECT().
		Template(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, _ interface{}, _ interface{}, _ interface{}, values map[string]interface{}, _ interface{}) ([]byte, error) {
			tt.Expect(reflect.ValueOf(values["operator"]).MapIndex(reflect.ValueOf("replicas")).Interface().(int)).To(Equal(1))
			return tt.manifest, nil
		})

	tt.Expect(tt.t.GenerateManifest(tt.ctx, tt.spec)).To(Equal(tt.manifest), "templater.GenerateManifest() should return right manifest")
}

func TestTemplaterGenerateManifestForRegistryAuth(t *testing.T) {
	tt := newtemplaterTest(t)
	tt.spec.Cluster.Spec.RegistryMirrorConfiguration = &v1alpha1.RegistryMirrorConfiguration{
		Endpoint:     "1.2.3.4",
		Port:         "443",
		Authenticate: true,
		OCINamespaces: []v1alpha1.OCINamespace{
			{
				Registry:  "public.ecr.aws",
				Namespace: "eks-anywhere",
			},
			{
				Registry:  "783794618700.dkr.ecr.us-west-2.amazonaws.com",
				Namespace: "curated-packages",
			},
		},
	}

	os.Unsetenv("REGISTRY_USERNAME")
	os.Unsetenv("REGISTRY_PASSWORD")
	_, err := tt.t.GenerateManifest(tt.ctx, tt.spec)
	tt.Expect(err).To(HaveOccurred(), "templater.GenerateManifest() should fail")

	t.Setenv("REGISTRY_USERNAME", "username")
	t.Setenv("REGISTRY_PASSWORD", "password")

	tt.h.EXPECT().
		RegistryLogin(gomock.Any(), "1.2.3.4:443", "username", "password").
		Return(nil)

	tt.h.EXPECT().
		Template(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(tt.manifest, nil)

	tt.Expect(tt.t.GenerateManifest(tt.ctx, tt.spec)).To(Equal(tt.manifest), "templater.GenerateManifest() should return right manifest")
}
