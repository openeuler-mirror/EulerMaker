package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"

	ebsv1 "ebs-api/ebs/v1"
	iamv1 "ebs-api/iam/v1"
	ebsapi "ebs-apiserver/pkg/apis/ebs"
	iamapi "ebs-apiserver/pkg/apis/iam"
	generatedopenapi "ebs-apiserver/pkg/generated/openapi"
	iammodule "ebs-apiserver/pkg/iam"
	"ebs-apiserver/pkg/iam/credential"
	buildstore "ebs-apiserver/pkg/registry/ebs/build"
	buildgraphstore "ebs-apiserver/pkg/registry/ebs/buildgraph"
	buildinfostore "ebs-apiserver/pkg/registry/ebs/buildinfo"
	configstore "ebs-apiserver/pkg/registry/ebs/config"
	jobstore "ebs-apiserver/pkg/registry/ebs/job"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	rpmrepostore "ebs-apiserver/pkg/registry/ebs/rpmrepo"
	runnerstore "ebs-apiserver/pkg/registry/ebs/runner"
	scriptstore "ebs-apiserver/pkg/registry/ebs/script"
	snapshotstore "ebs-apiserver/pkg/registry/ebs/snapshot"
	machineaccountstore "ebs-apiserver/pkg/registry/iam/machineaccount"
	userstore "ebs-apiserver/pkg/registry/iam/user"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

type readDeleteStorage struct{ store *esstore.Store }

func (s *readDeleteStorage) New() runtime.Object     { return s.store.New() }
func (s *readDeleteStorage) NewList() runtime.Object { return s.store.NewList() }
func (s *readDeleteStorage) Destroy()                { s.store.Destroy() }
func (s *readDeleteStorage) NamespaceScoped() bool   { return false }
func (s *readDeleteStorage) GetSingularName() string { return s.store.GetSingularName() }
func (s *readDeleteStorage) Get(ctx context.Context, name string, opts *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, opts)
}
func (s *readDeleteStorage) List(ctx context.Context, opts *internalversion.ListOptions) (runtime.Object, error) {
	return s.store.List(ctx, opts)
}
func (s *readDeleteStorage) Delete(ctx context.Context, name string, validate rest.ValidateObjectFunc, opts *metav1.DeleteOptions) (runtime.Object, bool, error) {
	return s.store.Delete(ctx, name, validate, opts)
}
func (s *readDeleteStorage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

func objDef() openapicommon.OpenAPIDefinition {
	return openapicommon.OpenAPIDefinition{
		Schema: spec.Schema{SchemaProps: spec.SchemaProps{
			Type:       []string{"object"},
			Properties: map[string]spec.Schema{},
		}},
	}
}

var openAPISupportDefinitions = map[string]openapicommon.OpenAPIDefinition{
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
}

func getOpenAPIDefinitions(ref openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
	definitions := generatedopenapi.GetOpenAPIDefinitions(ref)
	for name, definition := range openAPISupportDefinitions {
		definitions[name] = definition
	}
	return definitions
}

const (
	etcdPrefix = "/registry/ebs"
	// TODO: delete this after we have a better solution for large request body
	maxRequestBodyBytes = 5 << 20
)

var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme, serializer.EnableStrict)
)

func init() {
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	utilruntime.Must(ebsapi.AddToScheme(Scheme))
	utilruntime.Must(ebsv1.AddToScheme(Scheme))
	utilruntime.Must(iamapi.AddToScheme(Scheme))
	utilruntime.Must(iamv1.AddToScheme(Scheme))
}

type EulerMakerServerOptions struct {
	RecommendedOptions                  *options.RecommendedOptions
	EsServers                           string
	EsAPIKeyFile                        string
	EnableIAM                           bool
	InternalClientCAFile                string
	InsecureAllowUnauthenticatedClients bool
	esConfig                            *es.Config
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
	fs.StringVar(&o.EsAPIKeyFile, "es-api-key-file", "", "file containing the encoded Elasticsearch API key")
	fs.BoolVar(&o.EnableIAM, "enable-iam", o.EnableIAM, "enable the built-in IAM API and password authenticator")
	fs.StringVar(&o.InternalClientCAFile, "internal-client-ca-file", "", "CA bundle for internal component client certificates")
	fs.BoolVar(&o.InsecureAllowUnauthenticatedClients, "insecure-allow-unauthenticated-clients", false, "disable internal client certificate authentication (isolated development only)")
	o.RecommendedOptions.AddFlags(fs)
}

func (o *EulerMakerServerOptions) Validate(args []string) error {
	if err := o.validateInternalClientAuthentication(); err != nil {
		return err
	}
	errs := o.RecommendedOptions.Validate()
	if len(errs) > 0 {
		return fmt.Errorf("validation errors: %v", errs)
	}
	return nil
}

func (o *EulerMakerServerOptions) Complete() error {
	o.esConfig.Addresses = []string{o.EsServers}
	if o.EsAPIKeyFile != "" {
		data, err := os.ReadFile(o.EsAPIKeyFile)
		if err != nil {
			return fmt.Errorf("read Elasticsearch API key file: %w", err)
		}
		o.esConfig.APIKey = strings.TrimSpace(string(data))
		if o.esConfig.APIKey == "" {
			return fmt.Errorf("Elasticsearch API key file is empty")
		}
	}
	return nil
}

func (o *EulerMakerServerOptions) Config() (*genericapiserver.RecommendedConfig, error) {
	if err := o.validateInternalClientAuthentication(); err != nil {
		return nil, err
	}
	config := genericapiserver.NewRecommendedConfig(Codecs)
	// Large per-spec results and dependency graphs can exceed the 3 MiB default.
	config.MaxRequestBodyBytes = maxRequestBodyBytes
	buildHandlerChain := config.BuildHandlerChainFunc
	config.BuildHandlerChainFunc = func(handler http.Handler, serverConfig *genericapiserver.Config) http.Handler {
		return buildHandlerChain(withOldJobReadOptions(withFieldProjection(handler)), serverConfig)
	}
	config.LongRunningFunc = runnerJobLongRunningCheck(config.LongRunningFunc)
	config.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(
		getOpenAPIDefinitions,
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
	if o.InternalClientCAFile != "" {
		if err := configureInternalClientAuthentication(&config.Config, o.InternalClientCAFile); err != nil {
			return nil, err
		}
	}
	if err := o.RecommendedOptions.Audit.ApplyTo(&config.Config); err != nil {
		return nil, err
	}
	if err := o.RecommendedOptions.Features.ApplyTo(&config.Config); err != nil {
		return nil, err
	}
	return config, nil
}

func (o *EulerMakerServerOptions) validateInternalClientAuthentication() error {
	if o.InternalClientCAFile == "" && !o.InsecureAllowUnauthenticatedClients {
		return fmt.Errorf("internal-client-ca-file is required unless insecure-allow-unauthenticated-clients is enabled")
	}
	if o.InternalClientCAFile != "" && o.InsecureAllowUnauthenticatedClients {
		return fmt.Errorf("internal-client-ca-file and insecure-allow-unauthenticated-clients cannot be used together")
	}
	return nil
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

	srv, err := CreateServerChain(config, esClient, opts.EnableIAM)
	if err != nil {
		return err
	}

	prepared := srv.PrepareRun()
	return prepared.Run(stopCh)
}

func CreateServerChain(config *genericapiserver.RecommendedConfig, esClient *es.Client, enableIAM bool) (*genericapiserver.GenericAPIServer, error) {
	completedConfig := config.Complete()

	apiGroupInfo, buildClaims, err := CreateAPIGroupInfo(completedConfig.RESTOptionsGetter, esClient)
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
	if err := srv.AddPostStartHook("build-target-claim-recovery", func(hook genericapiserver.PostStartHookContext) error {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-hook.StopCh; cancel() }()
		go buildClaims.Run(ctx)
		return nil
	}); err != nil {
		return nil, err
	}
	configES := newConfigStore(esClient)
	if err := ensureDefaultConfigs(context.Background(), configES); err != nil {
		return nil, err
	}
	if err := ensureDefaultScript(context.Background(), newScriptStore(esClient)); err != nil {
		return nil, fmt.Errorf("initialize default script: %w", err)
	}
	if enableIAM {
		if err := esClient.EnsureIAMIndices(); err != nil {
			return nil, fmt.Errorf("ensure IAM indices: %w", err)
		}
		credentials := credential.NewStore(esClient)
		iamGroupInfo, users, machines := CreateIAMAPIGroupInfo(esClient)
		if err := srv.InstallAPIGroup(iamGroupInfo); err != nil {
			return nil, err
		}
		iammodule.InstallInternalRoutes(srv, credentials, users, machines)
	}
	installProjectAliasRoutes(srv)
	installRunnerJobAliasRoutes(srv)

	return srv, nil
}

func CreateIAMAPIGroupInfo(esClient *es.Client) (*genericapiserver.APIGroupInfo, *esstore.Store, *esstore.Store) {
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(iamapi.GroupName, Scheme, metav1.ParameterCodec, Codecs)
	storage := userstore.NewStorage()
	userES := esstore.New(esClient, "user", "User", storage.User.(*genericregistry.Store))
	machineStorage := machineaccountstore.NewStorage()
	machineES := esstore.New(esClient, "machineaccount", "MachineAccount", machineStorage.MachineAccount.(*genericregistry.Store))
	apiGroupInfo.VersionedResourcesStorageMap["v1"] = map[string]rest.Storage{"users": userES, "machineaccounts": &readDeleteStorage{store: machineES}}
	return &apiGroupInfo, userES, machineES
}

func CreateAPIGroupInfo(restOptionsGetter generic.RESTOptionsGetter, esClient *es.Client) (*genericapiserver.APIGroupInfo, *buildstore.ClaimManager, error) {
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(
		ebsapi.GroupName,
		Scheme,
		metav1.ParameterCodec,
		Codecs,
	)

	storeOptions := &generic.StoreOptions{RESTOptions: restOptionsGetter}

	v1Storage := map[string]rest.Storage{}
	configES := newConfigStore(esClient)
	v1Storage["configs"] = configES
	v1Storage["scripts"] = newScriptStore(esClient)

	projectStorage := projectstore.NewStorage()
	projectES := esstore.New(esClient, "project", "Project", projectStorage.Resource.(*genericregistry.Store))
	projectES.SetCreateHook(projectstore.ValidateArtifactsConfig(configES))
	projectES.SetUpdateHook(projectstore.ValidateArtifactsConfigUpdate(configES))
	v1Storage["projects"] = projectES
	v1Storage["projects/status"] = esstore.NewStatus(projectES, projectStorage.Status.(*genericregistry.Store))

	snapshotStorage := snapshotstore.NewStorage()
	snapshotES := esstore.New(esClient, "snapshot", "Snapshot", snapshotStorage.Resource.(*genericregistry.Store))
	v1Storage["snapshots"] = snapshotES
	v1Storage["snapshots/status"] = esstore.NewStatus(snapshotES, snapshotStorage.Status.(*genericregistry.Store))

	buildStorage := buildstore.NewStorage()
	buildES := esstore.New(esClient, "build", "Build", buildStorage.Resource.(*genericregistry.Store))
	buildES.SetCreateHook(buildstore.ValidateBuildInputs(newConfigStore(esClient), projectES))
	buildStatusES := esstore.NewStatus(buildES, buildStorage.Status.(*genericregistry.Store))
	buildClaims := buildstore.NewClaimManager(buildES, esClient)
	v1Storage["builds"] = buildES
	v1Storage["builds/status"] = buildStatusES
	v1Storage["builds/abort"] = buildstore.NewAbortStorage(buildES, buildStatusES)

	buildInfoStorage := buildinfostore.NewStorage()
	buildInfoES := esstore.New(esClient, "buildinfo", "BuildInfo", buildInfoStorage.Resource.(*genericregistry.Store))
	v1Storage["buildinfos"] = buildInfoES
	v1Storage["buildinfos/status"] = esstore.NewStatus(buildInfoES, buildInfoStorage.Status.(*genericregistry.Store))
	buildGraphStorage := buildgraphstore.NewStorage()
	buildGraphES := esstore.New(esClient, "buildgraph", "BuildGraph", buildGraphStorage.Resource.(*genericregistry.Store))
	buildGraphES.SetCreateHook(buildgraphstore.ValidateOwner(buildInfoES))
	v1Storage["buildgraphs"] = buildGraphES
	v1Storage["buildgraphs/status"] = esstore.NewStatus(buildGraphES, buildGraphStorage.Status.(*genericregistry.Store))

	rpmRepoStorage := rpmrepostore.NewStorage()
	rpmRepoES := esstore.New(esClient, "rpmrepo", "RpmRepo", rpmRepoStorage.Resource.(*genericregistry.Store))
	v1Storage["rpmrepos"] = rpmRepoES
	v1Storage["rpmrepos/status"] = esstore.NewStatus(rpmRepoES, rpmRepoStorage.Status.(*genericregistry.Store))

	jobStorage := jobstore.NewStorage()
	if err := jobStorage.CompleteWithOptions(storeOptions); err != nil {
		return nil, nil, err
	}
	v1Storage["jobs"] = jobStorage.Resource
	v1Storage["jobs/status"] = jobStorage.Status
	v1Storage["jobs/abort"] = jobstore.NewAbortStorage(jobStorage.Resource, jobStorage.Status.(rest.Updater))
	oldJobs := esstore.NewOldJobs(esClient, jobStorage.Resource.(*genericregistry.Store))
	v1Storage["oldjobs"] = oldJobs
	v1Storage["jobs/archive"] = jobstore.NewArchiveStorage(jobStorage.Resource, oldJobs)

	runnerStorage := runnerstore.NewStorage()
	if err := runnerStorage.CompleteWithOptions(storeOptions); err != nil {
		return nil, nil, err
	}
	v1Storage["runners"] = runnerStorage.Resource
	v1Storage["runners/status"] = runnerStorage.Status

	apiGroupInfo.VersionedResourcesStorageMap["v1"] = v1Storage

	return &apiGroupInfo, buildClaims, nil
}

func newConfigStore(client *es.Client) *esstore.Store {
	return esstore.New(client, "config", "Config", configstore.NewStorage().Resource.(*genericregistry.Store))
}

func newScriptStore(client *es.Client) *esstore.Store {
	return esstore.New(client, "script", "Script", scriptstore.NewStorage().Resource.(*genericregistry.Store))
}

func completeStore(storage rest.Storage, storeOptions *generic.StoreOptions) error {
	store, ok := storage.(*genericregistry.Store)
	if !ok {
		return nil
	}
	return store.CompleteWithOptions(storeOptions)
}
