// solution from https://github.com/billiford/go-clouddriver/blob/master/pkg/kubernetes/client.go

package internal

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/types"

	"github.com/pkg/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// metadata is an internal type to transfer data to the adapter
type Metadata struct {
	Name      string
	Namespace string
	Resource  string
	Group     string
	Version   string
	Kind      string
}

type KubeClient struct {
	dynamicClient dynamic.Interface
	config        *rest.Config
	mapper        *restmapper.DeferredDiscoveryRESTMapper
}

func NewKubeClient(kubeconfig string, logger *zap.SugaredLogger) (*KubeClient, error) {
	config, err := getRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	config.WarningHandler = &loggingWarningHandler{logger: logger}
	return newForConfig(config)
}

func NewInClusterClient(logger *zap.SugaredLogger) (*KubeClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	config.WarningHandler = &loggingWarningHandler{logger: logger}
	return newForConfig(config)
}

func newForConfig(config *rest.Config) (*KubeClient, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	mapper, err := getDiscoveryMapper(config)
	if err != nil {
		return nil, err
	}

	return &KubeClient{
		dynamicClient: dynamicClient,
		config:        config,
		mapper:        mapper,
	}, nil
}

func (kube *KubeClient) Apply(u *unstructured.Unstructured) (*Metadata, error) {
	return kube.ApplyWithNamespaceOverride(u, "")
}

// ApplyWithNamespaceOverride applies a given manifest with an optional namespace to override.
// If no namespace is set on the manifest and no namespace override is passed in then we set the namespace to 'default'.
// If namespaceOverride is empty it will NOT override the namespace set on the manifest.
// We only override the namespace if the manifest is NOT cluster scoped (i.e. a ClusterRole) and namespaceOverride is NOT an
// empty string.
func (kube *KubeClient) ApplyWithNamespaceOverride(u *unstructured.Unstructured, namespaceOverride string) (*Metadata, error) {
	metadata := &Metadata{}
	gvk := u.GroupVersionKind()

	restMapping, err := kube.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return metadata, err
	}

	gv := gvk.GroupVersion()
	kube.config.GroupVersion = &gv

	restClient, err := newRestClient(*kube.config, gv)
	if err != nil {
		return metadata, err
	}

	helper := resource.NewHelper(restClient, restMapping)

	setDefaultNamespaceIfScopedAndNoneSet(namespaceOverride, u, helper)

	setNamespaceIfScoped(namespaceOverride, u, helper)

	info := &resource.Info{
		Client:          restClient,
		Mapping:         restMapping,
		Namespace:       u.GetNamespace(),
		Name:            u.GetName(),
		Source:          "",
		ResourceVersion: restMapping.Resource.Version,
	}

	if err := info.Get(); err != nil {
		if !k8serrors.IsNotFound(err) {
			return metadata, err
		}

		// Then create the resource and skip the three-way merge
		_, err := helper.Create(u.GetNamespace(), true, u)
		if err != nil {
			return metadata, err
		}

		metadata.Name = u.GetName()
		metadata.Namespace = u.GetNamespace()
		metadata.Kind = u.GroupVersionKind().Kind
		return metadata, nil
	}

	replace := newReplace(helper)
	replacedObject, err := replace(u, u.GetNamespace(), u.GetName())
	if err != nil {
		return metadata, err
	}

	_ = info.Refresh(replacedObject, true)

	metadata.Name = u.GetName()
	metadata.Namespace = u.GetNamespace()
	metadata.Kind = gvk.Kind

	return metadata, nil
}

func (kube *KubeClient) GetClientSet() (*kubernetes.Clientset, error) {
	return kubernetes.NewForConfig(kube.config)
}

func (kube *KubeClient) DeleteResourceByKindAndNameAndNamespace(kind, name, namespace string, do metav1.DeleteOptions) (*Metadata, error) {
	gvk, err := kube.mapper.KindFor(schema.GroupVersionResource{
		Resource: kind,
	})
	if err != nil {
		return nil, err
	}

	var isNamespaceResource = strings.ToLower(gvk.Kind) == "namespace"

	if !isNamespaceResource && namespace == "" { //set qualified namespace (except resource is of kind 'namespace')
		namespace = "default"
	}

	restMapping, err := kube.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	restClient, err := newRestClient(*kube.config, gvk.GroupVersion())
	if err != nil {
		return nil, err
	}

	helper := resource.NewHelper(restClient, restMapping)

	if helper.NamespaceScoped {
		err = kube.dynamicClient.
			Resource(restMapping.Resource).
			Namespace(namespace).
			Delete(context.TODO(), name, do)
	} else {
		err = kube.dynamicClient.
			Resource(restMapping.Resource).
			Delete(context.TODO(), name, do)
	}

	//return deleted resource
	if isNamespaceResource {
		namespace = "" //namespace resources have always an empty namespace field
	}
	return &Metadata{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	}, err
}

// Get a manifest by resource/kind (example: 'pods' or 'pod'),
// name (example: 'my-pod'), and namespace (example: 'my-namespace').
func (kube *KubeClient) Get(kind, name, namespace string) (*unstructured.Unstructured, error) {
	gvk, err := kube.mapper.KindFor(schema.GroupVersionResource{Resource: kind})
	if err != nil {
		return nil, err
	}

	restMapping, err := kube.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	restClient, err := newRestClient(*kube.config, gvk.GroupVersion())
	if err != nil {
		return nil, err
	}

	var u *unstructured.Unstructured

	helper := resource.NewHelper(restClient, restMapping)
	if helper.NamespaceScoped {
		u, err = kube.dynamicClient.
			Resource(restMapping.Resource).
			Namespace(namespace).
			Get(context.TODO(), name, metav1.GetOptions{})
	} else {
		u, err = kube.dynamicClient.
			Resource(restMapping.Resource).
			Get(context.TODO(), name, metav1.GetOptions{})
	}

	return u, err
}

// ListResource lists all resources by their kind or resource (e.g. "replicaset" or "replicasets").
func (kube *KubeClient) ListResource(resource string, lo metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	gvr, err := kube.mapper.ResourceFor(schema.GroupVersionResource{Resource: resource})
	if err != nil {
		return nil, err
	}
	return kube.dynamicClient.Resource(gvr).List(context.TODO(), lo)
}

func (kube *KubeClient) Patch(kind, name, namespace string, p []byte) (*Metadata, *unstructured.Unstructured, error) {
	return kube.PatchUsingStrategy(kind, name, namespace, p, types.StrategicMergePatchType)
}

func (kube *KubeClient) PatchUsingStrategy(kind, name, namespace string, p []byte, strategy types.PatchType) (*Metadata, *unstructured.Unstructured, error) {
	metadata := &Metadata{}
	gvk, err := kube.mapper.KindFor(schema.GroupVersionResource{Resource: kind})
	if err != nil {
		return metadata, nil, err
	}

	restMapping, err := kube.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return metadata, nil, err
	}

	restClient, err := newRestClient(*kube.config, gvk.GroupVersion())
	if err != nil {
		return metadata, nil, err
	}

	helper := resource.NewHelper(restClient, restMapping)

	var u *unstructured.Unstructured

	if helper.NamespaceScoped {
		u, err = kube.dynamicClient.
			Resource(restMapping.Resource).
			Namespace(namespace).
			Patch(context.TODO(), name, strategy, p, metav1.PatchOptions{})
	} else {
		u, err = kube.dynamicClient.
			Resource(restMapping.Resource).
			Patch(context.TODO(), name, strategy, p, metav1.PatchOptions{})
	}

	if err != nil {
		return metadata, nil, err
	}

	gvr := restMapping.Resource

	metadata.Name = u.GetName()
	metadata.Namespace = u.GetNamespace()
	metadata.Group = gvr.Group
	metadata.Resource = gvr.Resource
	metadata.Version = gvr.Version
	metadata.Kind = gvk.Kind

	return metadata, u, err
}

func (kube *KubeClient) DeleteNamespace(namespace string) error {
	getter := NewRESTClientGetter(kube.config)
	factory := cmdutil.NewFactory(getter)
	r := factory.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().
		LabelSelectorParam("").
		FieldSelectorParam("").
		RequestChunksOf(500).
		ResourceTypeOrNameArgs(true, "all").
		ContinueOnError().
		Latest().
		Flatten().
		Do()
	infos, err := r.Infos()
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		namespaceRes := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
		err = kube.dynamicClient.
			Resource(namespaceRes).
			Delete(context.TODO(), namespace, metav1.DeleteOptions{})
	}
	return err
}

func newRestClient(restConfig rest.Config, gv schema.GroupVersion) (rest.Interface, error) {
	restConfig.ContentConfig = resource.UnstructuredPlusDefaultContentConfig()
	restConfig.GroupVersion = &gv

	if len(gv.Group) == 0 {
		restConfig.APIPath = "/api"
	} else {
		restConfig.APIPath = "/apis"
	}

	return rest.RESTClientFor(&restConfig)
}

func getDiscoveryMapper(restConfig *rest.Config) (*restmapper.DeferredDiscoveryRESTMapper, error) {
	// Prepare a RESTMapper to find GVR
	dc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create new discovery client")
	}

	discoveryMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	return discoveryMapper, nil
}

func getRestConfig(kubeconfig string) (*rest.Config, error) {
	return clientcmd.BuildConfigFromKubeconfigGetter("", func() (config *clientcmdapi.Config, e error) {
		return clientcmd.Load([]byte(kubeconfig))
	})
}

func setDefaultNamespaceIfScopedAndNoneSet(namespace string, u *unstructured.Unstructured, helper *resource.Helper) {
	if helper.NamespaceScoped {
		resNamespace := u.GetNamespace()
		if resNamespace == "" {
			if namespace == "" {
				namespace = "default"
			}
			u.SetNamespace(namespace)
		}
	}
}

func setNamespaceIfScoped(namespace string, u *unstructured.Unstructured, helper *resource.Helper) {
	if u.GetNamespace() == "" && helper.NamespaceScoped {
		u.SetNamespace(namespace)
	}
}
