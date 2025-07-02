package addonfactory

import (
	"fmt"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/klog/v2"
	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"open-cluster-management.io/addon-framework/pkg/agent"
	"open-cluster-management.io/addon-framework/pkg/assets"
)

// templateBuiltinValues includes the built-in values for template agentAddon.
// the values for template config should begin with an uppercase letter, so we need convert it to Values by StructToValues.
// the built-in values can not be overrided by getValuesFuncs
type templateBuiltinValues struct {
	ClusterName           string
	AddonInstallNamespace string
	InstallMode           string
}

// templateDefaultValues includes the default values for template agentAddon.
// the values for template config should begin with an uppercase letter, so we need convert it to Values by StructToValues.
// the default values can be overrided by getValuesFuncs
type templateDefaultValues struct {
	HubKubeConfigSecret     string
	CustomCertSecrets       []string
	ManagedKubeConfigSecret string
	AwsIrsaRegistration     *templateHubAwsIrsaDriver
}

type templateHubAwsIrsaDriver struct {
	IamConfigSecret string
}

type templateFile struct {
	name    string
	content []byte
}

type TemplateAgentAddon struct {
	decoder               runtime.Decoder
	templateFiles         []templateFile
	getValuesFuncs        []GetValuesFunc
	agentAddonOptions     agent.AgentAddonOptions
	trimCRDDescription    bool
	agentInstallNamespace func(addon *addonapiv1alpha1.ManagedClusterAddOn) (string, error)
}

func newTemplateAgentAddon(factory *AgentAddonFactory) *TemplateAgentAddon {
	return &TemplateAgentAddon{
		decoder:               serializer.NewCodecFactory(factory.scheme).UniversalDeserializer(),
		getValuesFuncs:        factory.getValuesFuncs,
		agentAddonOptions:     factory.agentAddonOptions,
		trimCRDDescription:    factory.trimCRDDescription,
		agentInstallNamespace: factory.agentInstallNamespace,
	}
}

func (a *TemplateAgentAddon) Manifests(
	cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) ([]runtime.Object, error) {
	var objects []runtime.Object

	configValues, err := a.getValues(cluster, addon)
	if err != nil {
		return objects, err
	}

	for _, file := range a.templateFiles {
		if len(file.content) == 0 {
			continue
		}
		klog.V(4).Infof("rendered template: %v", file.content)
		raw := assets.MustCreateAssetFromTemplate(file.name, file.content, configValues).Data
		object, _, err := a.decoder.Decode(raw, nil, nil)
		if err != nil {
			if runtime.IsMissingKind(err) {
				klog.V(4).Infof("Skipping template %v, reason: %v", file.name, err)
				continue
			}
			return nil, err
		}
		objects = append(objects, object)
	}

	if a.trimCRDDescription {
		objects = trimCRDDescription(objects)
	}
	return objects, nil
}

func (a *TemplateAgentAddon) GetAgentAddonOptions() agent.AgentAddonOptions {
	return a.agentAddonOptions
}

func (a *TemplateAgentAddon) getValues(
	cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (Values, error) {
	overrideValues := map[string]interface{}{}

	defaultValues := a.getDefaultValues(cluster, addon)
	overrideValues = MergeValues(overrideValues, defaultValues)

	for i := 0; i < len(a.getValuesFuncs); i++ {
		if a.getValuesFuncs[i] != nil {
			userValues, err := a.getValuesFuncs[i](cluster, addon)
			if err != nil {
				return overrideValues, err
			}
			overrideValues = MergeValues(overrideValues, userValues)
		}
	}
	builtinValues, err := a.getBuiltinValues(cluster, addon)
	if err != nil {
		return overrideValues, err
	}
	overrideValues = MergeValues(overrideValues, builtinValues)

	return overrideValues, nil
}

func (a *TemplateAgentAddon) getBuiltinValues(
	cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) (Values, error) {
	builtinValues := templateBuiltinValues{}
	builtinValues.ClusterName = cluster.GetName()

	installNamespace := addon.Spec.InstallNamespace
	if len(installNamespace) == 0 {
		installNamespace = AddonDefaultInstallNamespace
	}
	if a.agentInstallNamespace != nil {
		ns, err := a.agentInstallNamespace(addon)
		if err != nil {
			klog.Errorf("failed to get agent install namespace for addon %s: %v", addon.Name, err)
			return nil, err
		}
		if len(ns) > 0 {
			installNamespace = ns
		}
	}
	builtinValues.AddonInstallNamespace = installNamespace

	builtinValues.InstallMode, _ = a.agentAddonOptions.HostedModeInfoFunc(addon, cluster)

	return StructToValues(builtinValues), nil
}

func (a *TemplateAgentAddon) getDefaultValues(
	cluster *clusterv1.ManagedCluster,
	addon *addonapiv1alpha1.ManagedClusterAddOn) Values {
	defaultValues := templateDefaultValues{}

	for _, registration := range addon.Status.Registrations {
		switch registration.Type {
		case addonapiv1alpha1.RegistrationAuthTypeCsr:
			if a.agentAddonOptions.Registration != nil {
				switch registration.CSR.SignerName {
				case certificatesv1.KubeAPIServerClientSignerName:
					defaultValues.HubKubeConfigSecret = hubKubeConfigSecret(a.agentAddonOptions.AddonName)
				default:
					defaultValues.CustomCertSecrets = append(
						defaultValues.CustomCertSecrets,
						customCertSecret(a.agentAddonOptions.AddonName, registration.CSR.SignerName),
					)
				}
			}
		case addonapiv1alpha1.RegistrationAuthTypeAwsIrsa:
			defaultValues.HubKubeConfigSecret = hubKubeConfigSecret(a.agentAddonOptions.AddonName)
			defaultValues.AwsIrsaRegistration = &templateHubAwsIrsaDriver{}
			if registration.AwsIrsa != nil {
				defaultValues.AwsIrsaRegistration.IamConfigSecret = registration.AwsIrsa.IamConfigSecret
			}
		}
	}

	defaultValues.ManagedKubeConfigSecret = fmt.Sprintf("%s-managed-kubeconfig", addon.Name)

	return StructToValues(defaultValues)
}

func (a *TemplateAgentAddon) addTemplateData(file string, data []byte) {
	a.templateFiles = append(a.templateFiles, templateFile{name: file, content: data})
}

func hubKubeConfigSecret(addonName string) string {
	return fmt.Sprintf("%s-hub-kubeconfig", addonName)
}

func customCertSecret(addonName, signerName string) string {
	return fmt.Sprintf("%s-%s-client-cert", addonName, strings.ReplaceAll(signerName, "/", "-"))
}
