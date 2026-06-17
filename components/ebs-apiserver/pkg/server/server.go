package server

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"

	ebsapi "ebs-apiserver/pkg/apis/ebs"
	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	buildstore "ebs-apiserver/pkg/registry/ebs/build"
	jobstore "ebs-apiserver/pkg/registry/ebs/job"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	runnerstore "ebs-apiserver/pkg/registry/ebs/runner"
	snapshotstore "ebs-apiserver/pkg/registry/ebs/snapshot"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/hybrid"
)

func objDef() openapicommon.OpenAPIDefinition {
	return openapicommon.OpenAPIDefinition{
		Schema: spec.Schema{SchemaProps: spec.SchemaProps{
			Type:       []string{"object"},
			Properties: map[string]spec.Schema{},
		}},
	}
}

var ebsOpenAPIDefinitions = map[string]openapicommon.OpenAPIDefinition{
	"k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta":               objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.TypeMeta":                 objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.ListMeta":                 objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.Status":                   objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.Time":                     objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.MicroTime":                objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.FieldsV1":                 objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.ManagedFieldsEntry":       objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.OwnerReference":           objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.Condition":                objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.LabelSelector":            objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.LabelSelectorRequirement": objDef(),
	"k8s.io/apimachinery/pkg/runtime.RawExtension":                  objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.DeleteOptions":            objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.CreateOptions":            objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.UpdateOptions":            objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.PatchOptions":             objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.GetOptions":               objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.ListOptions":              objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.Patch":                    objDef(),
	"k8s.io/apimachinery/pkg/apis/meta/v1.WatchEvent":               objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.Project":                         objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.ProjectSpec":                     objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.ProjectStatus":                   objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.ProjectList":                     objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.BuildTarget":                     objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.BuildTargetFlags":                objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.PackageRepo":                     objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.Snapshot":                        objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.SnapshotSpec":                    objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.SnapshotStatus":                  objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.SpecCommit":                      objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.SnapshotList":                    objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.Build":                           objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.BuildSpec":                       objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.BuildStatus":                     objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.PackageStatus":                   objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.BuildList":                       objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.Job":                             objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.JobSpec":                         objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.JobStatus":                       objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.JobList":                         objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.Runner":                          objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.RunnerSpec":                      objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.RunnerTaint":                     objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.RunnerStatus":                    objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.RunnerAddress":                   objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.RunnerInfo":                      objDef(),
	"ebs-apiserver/pkg/apis/ebs/v1.RunnerList":                      objDef(),
}

const etcdPrefix = "/registry/ebs"

var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	ebsapi.AddToScheme(Scheme)
	ebsv1.AddToScheme(Scheme)
}

type EulerMakerServerOptions struct {
	RecommendedOptions *options.RecommendedOptions
	EsServers          string
	esConfig           *es.Config
}

func NewEulerMakerServerOptions() *EulerMakerServerOptions {
	o := &EulerMakerServerOptions{
		RecommendedOptions: options.NewRecommendedOptions(
			etcdPrefix,
			Codecs.LegacyCodec(ebsv1.SchemeGroupVersion),
		),
		EsServers: "http://elasticsearch:9200",
		esConfig:  es.DefaultConfig(),
	}
	o.RecommendedOptions.Etcd.StorageConfig.Transport.ServerList = []string{"http://etcd:2379"}
	o.RecommendedOptions.SecureServing.BindPort = 8443
	o.RecommendedOptions.Authentication = nil
	o.RecommendedOptions.Authorization = nil
	o.RecommendedOptions.CoreAPI = nil
	o.RecommendedOptions.Admission = nil
	o.RecommendedOptions.Etcd.StorageConfig.EncodeVersioner = runtime.NewMultiGroupVersioner(
		ebsv1.SchemeGroupVersion,
		schema.GroupKind{Group: ebsapi.GroupName},
	)
	return o
}

func (o *EulerMakerServerOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.EsServers, "es-servers", o.EsServers, "elasticsearch server address")
	o.RecommendedOptions.AddFlags(fs)
}

func (o *EulerMakerServerOptions) Validate(args []string) error {
	errs := o.RecommendedOptions.Validate()
	if len(errs) > 0 {
		return fmt.Errorf("validation errors: %v", errs)
	}
	return nil
}

func (o *EulerMakerServerOptions) Complete() error {
	o.esConfig.Addresses = []string{o.EsServers}
	return nil
}

func (o *EulerMakerServerOptions) Config() (*genericapiserver.RecommendedConfig, error) {
	config := genericapiserver.NewRecommendedConfig(Codecs)
	config.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(
		func(ref openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
			return ebsOpenAPIDefinitions
		},
		openapinamer.NewDefinitionNamer(Scheme),
	)
	config.OpenAPIV3Config.GetDefinitionName = func(name string) (string, spec.Extensions) {
		return name, nil
	}
	config.OpenAPIV3Config.Definitions = config.OpenAPIV3Config.GetDefinitions(func(name string) spec.Ref {
		defName, _ := config.OpenAPIV3Config.GetDefinitionName(name)
		return spec.MustCreateRef("#/components/schemas/" + openapicommon.EscapeJsonPointer(defName))
	})
	config.OpenAPIV3Config.Info = &spec.Info{
		InfoProps: spec.InfoProps{
			Title:   "ebs-apiserver",
			Version: "v1",
		},
	}

	if err := o.RecommendedOptions.Etcd.ApplyTo(&config.Config); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.SecureServing.ApplyTo(&config.Config.SecureServing, &config.Config.LoopbackClientConfig); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.Authentication.ApplyTo(&config.Config.Authentication, config.SecureServing, config.OpenAPIConfig); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.Authorization.ApplyTo(&config.Config.Authorization); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.Audit.ApplyTo(&config.Config); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.Features.ApplyTo(&config.Config); err != nil {
		return nil, err
	}

	return config, nil
}

func Run(stopCh <-chan struct{}) error {
	fs := pflag.NewFlagSet("ebs-apiserver", pflag.ContinueOnError)

	opts := NewEulerMakerServerOptions()
	opts.AddFlags(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if err := opts.Complete(); err != nil {
		return err
	}
	if err := opts.Validate(nil); err != nil {
		return err
	}

	config, err := opts.Config()
	if err != nil {
		return err
	}

	esClient, err := opts.esConfig.NewClient()
	if err != nil {
		return err
	}

	srv, err := CreateServerChain(config, esClient)
	if err != nil {
		return err
	}

	prepared := srv.PrepareRun()
	return prepared.Run(stopCh)
}

func CreateServerChain(config *genericapiserver.RecommendedConfig, esClient *es.Client) (*genericapiserver.GenericAPIServer, error) {
	completedConfig := config.Complete()

	apiGroupInfo, err := CreateAPIGroupInfo(completedConfig.RESTOptionsGetter, esClient)
	if err != nil {
		return nil, err
	}

	srv, err := completedConfig.New("ebs-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	if err := srv.InstallAPIGroup(apiGroupInfo); err != nil {
		return nil, err
	}
	installProjectAliasRoutes(srv)

	return srv, nil
}

func CreateAPIGroupInfo(restOptionsGetter generic.RESTOptionsGetter, esClient *es.Client) (*genericapiserver.APIGroupInfo, error) {
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(
		ebsapi.GroupName,
		Scheme,
		metav1.ParameterCodec,
		Codecs,
	)

	storeOptions := &generic.StoreOptions{RESTOptions: restOptionsGetter}

	v1Storage := map[string]rest.Storage{}

	projectStorage := projectstore.NewStorage(Scheme)
	if err := projectStorage.Project.(*genericregistry.Store).CompleteWithOptions(storeOptions); err != nil {
		return nil, err
	}
	if err := completeStore(projectStorage.Status, storeOptions); err != nil {
		return nil, err
	}
	v1Storage["projects"] = hybrid.NewEnricherStore(projectStorage.Project.(*genericregistry.Store), esClient, "project", func() runtime.Object { return &ebsv1.Project{} }, func() runtime.Object { return &ebsv1.ProjectList{} })
	v1Storage["projects/status"] = projectStorage.Status

	snapshotStorage := snapshotstore.NewStorage(Scheme)
	if err := snapshotStorage.Snapshot.(*genericregistry.Store).CompleteWithOptions(storeOptions); err != nil {
		return nil, err
	}
	if err := completeStore(snapshotStorage.Status, storeOptions); err != nil {
		return nil, err
	}
	v1Storage["snapshots"] = hybrid.NewEnricherStore(snapshotStorage.Snapshot.(*genericregistry.Store), esClient, "snapshot", func() runtime.Object { return &ebsv1.Snapshot{} }, func() runtime.Object { return &ebsv1.SnapshotList{} })
	v1Storage["snapshots/status"] = snapshotStorage.Status

	buildStorage := buildstore.NewStorage(Scheme)
	if err := buildStorage.Build.(*genericregistry.Store).CompleteWithOptions(storeOptions); err != nil {
		return nil, err
	}
	if err := completeStore(buildStorage.Status, storeOptions); err != nil {
		return nil, err
	}
	v1Storage["builds"] = hybrid.NewEnricherStore(buildStorage.Build.(*genericregistry.Store), esClient, "build", func() runtime.Object { return &ebsv1.Build{} }, func() runtime.Object { return &ebsv1.BuildList{} })
	v1Storage["builds/status"] = buildStorage.Status
	v1Storage["builds/abort"] = buildStorage.Abort

	jobStorage := jobstore.NewStorage(Scheme)
	if err := jobStorage.Job.(*genericregistry.Store).CompleteWithOptions(storeOptions); err != nil {
		return nil, err
	}
	if err := completeStore(jobStorage.Status, storeOptions); err != nil {
		return nil, err
	}
	v1Storage["jobs"] = hybrid.NewEnricherStore(jobStorage.Job.(*genericregistry.Store), esClient, "job", func() runtime.Object { return &ebsv1.Job{} }, func() runtime.Object { return &ebsv1.JobList{} })
	v1Storage["jobs/status"] = jobStorage.Status

	runnerStorage := runnerstore.NewStorage(Scheme)
	if err := runnerStorage.Runner.(*genericregistry.Store).CompleteWithOptions(storeOptions); err != nil {
		return nil, err
	}
	if err := completeStore(runnerStorage.Status, storeOptions); err != nil {
		return nil, err
	}
	v1Storage["runners"] = hybrid.NewEnricherStore(runnerStorage.Runner.(*genericregistry.Store), esClient, "runner", func() runtime.Object { return &ebsv1.Runner{} }, func() runtime.Object { return &ebsv1.RunnerList{} })
	v1Storage["runners/status"] = runnerStorage.Status

	apiGroupInfo.VersionedResourcesStorageMap["v1"] = v1Storage

	return &apiGroupInfo, nil
}

func completeStore(storage rest.Storage, storeOptions *generic.StoreOptions) error {
	store, ok := storage.(*genericregistry.Store)
	if !ok {
		return nil
	}
	return store.CompleteWithOptions(storeOptions)
}
