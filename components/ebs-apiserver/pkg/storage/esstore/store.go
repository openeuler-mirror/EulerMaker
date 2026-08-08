package esstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"

	"ebs-apiserver/pkg/storage/es"
)

const (
	pitKeepAlive  = "5m"
	scanBatchSize = int64(500)
)

type Store struct {
	client          *es.Client
	resource        schema.GroupResource
	singular        schema.GroupResource
	resourceName    string
	kind            string
	namespaceScoped bool
	newFunc         func() runtime.Object
	newListFunc     func() runtime.Object
	createStrategy  rest.RESTCreateStrategy
	updateStrategy  rest.RESTUpdateStrategy
	deleteStrategy  rest.RESTDeleteStrategy
	tableConvertor  rest.TableConvertor
	deleteHook      func(context.Context, string) error
}

type StatusStore struct {
	parent         *Store
	updateStrategy rest.RESTUpdateStrategy
}

func New(client *es.Client, resourceName, kind string, template *genericregistry.Store) *Store {
	return &Store{
		client:          client,
		resource:        template.DefaultQualifiedResource,
		singular:        template.SingularQualifiedResource,
		resourceName:    resourceName,
		kind:            kind,
		namespaceScoped: template.CreateStrategy.NamespaceScoped(),
		newFunc:         template.NewFunc,
		newListFunc:     template.NewListFunc,
		createStrategy:  template.CreateStrategy,
		updateStrategy:  template.UpdateStrategy,
		deleteStrategy:  template.DeleteStrategy,
		tableConvertor:  template.TableConvertor,
	}
}

func NewStatus(parent *Store, template *genericregistry.Store) *StatusStore {
	return &StatusStore{parent: parent, updateStrategy: template.UpdateStrategy}
}

// SetDeleteHook registers cleanup that must succeed before an object is removed.
func (s *Store) SetDeleteHook(hook func(context.Context, string) error) { s.deleteHook = hook }

func (s *Store) New() runtime.Object     { return s.newFunc() }
func (s *Store) NewList() runtime.Object { return s.newListFunc() }
func (s *Store) NamespaceScoped() bool   { return s.namespaceScoped }
func (s *Store) GetSingularName() string { return s.singular.Resource }
func (s *Store) Destroy()                {}

func (s *Store) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.tableConvertor.ConvertToTable(ctx, obj, opts)
}

func (s *Store) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	hit, err := s.client.Get(ctx, s.resourceName, s.documentID(ctx, name))
	if err != nil {
		return nil, s.apiError(err, name)
	}
	return s.objectFromHit(hit)
}

func (s *Store) Create(ctx context.Context, obj runtime.Object, admission rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	return s.CreateWithCredential(ctx, obj, nil, admission, options)
}

func (s *Store) CreateWithCredential(ctx context.Context, obj runtime.Object, credential json.RawMessage, admission rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	if options == nil {
		options = &metav1.CreateOptions{}
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	rest.FillObjectMetaSystemFields(accessor)
	if accessor.GetUID() == "" {
		accessor.SetUID(types.UID(uuid.NewUUID()))
	}
	if accessor.GetGenerateName() != "" && accessor.GetName() == "" {
		accessor.SetName(s.createStrategy.GenerateName(accessor.GetGenerateName()))
	}
	if accessor.GetGeneration() == 0 {
		accessor.SetGeneration(1)
	}
	if err := rest.BeforeCreate(s.createStrategy, ctx, obj); err != nil {
		return nil, err
	}
	if admission != nil {
		if err := admission(ctx, obj.DeepCopyObject()); err != nil {
			return nil, err
		}
	}
	if dryrun.IsDryRun(options.DryRun) {
		accessor.SetResourceVersion("v1:0:0")
		return obj, nil
	}
	doc, id, err := s.documentFor(ctx, obj)
	if err != nil {
		return nil, err
	}
	doc.Credential = credential
	version, err := s.client.Create(ctx, s.resourceName, id, doc)
	if err != nil {
		if es.IsStatus(err, 409) {
			return nil, apierrors.NewAlreadyExists(s.resource, accessor.GetName())
		}
		return nil, s.apiError(err, accessor.GetName())
	}
	accessor.SetResourceVersion(encodeVersion(version.SeqNo, version.PrimaryTerm))
	return obj, nil
}

func (s *Store) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options, s.updateStrategy)
}

func (s *Store) update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions, strategy rest.RESTUpdateStrategy) (runtime.Object, bool, error) {
	if options == nil {
		options = &metav1.UpdateOptions{}
	}
	hit, err := s.client.Get(ctx, s.resourceName, s.documentID(ctx, name))
	if err != nil {
		if es.IsStatus(err, 404) && forceAllowCreate && strategy.AllowCreateOnUpdate() {
			obj, updateErr := objInfo.UpdatedObject(ctx, s.newFunc())
			if updateErr != nil {
				return nil, false, updateErr
			}
			created, createErr := s.Create(ctx, obj, createValidation, &metav1.CreateOptions{DryRun: options.DryRun})
			return created, true, createErr
		}
		return nil, false, s.apiError(err, name)
	}
	oldObj, err := s.objectFromHit(hit)
	if err != nil {
		return nil, false, err
	}
	newObj, err := objInfo.UpdatedObject(ctx, oldObj.DeepCopyObject())
	if err != nil {
		return nil, false, err
	}
	newMeta, err := meta.Accessor(newObj)
	if err != nil {
		return nil, false, apierrors.NewInternalError(err)
	}
	oldMeta, _ := meta.Accessor(oldObj)
	if pre := objInfo.Preconditions(); pre != nil && pre.UID != nil && *pre.UID != oldMeta.GetUID() {
		return nil, false, apierrors.NewConflict(s.resource, name, fmt.Errorf("UID precondition failed"))
	}
	if newMeta.GetResourceVersion() == "" && !strategy.AllowUnconditionalUpdate() {
		return nil, false, apierrors.NewConflict(s.resource, name, fmt.Errorf("metadata.resourceVersion is required"))
	}
	if newMeta.GetResourceVersion() != "" && newMeta.GetResourceVersion() != oldMeta.GetResourceVersion() {
		return nil, false, apierrors.NewConflict(s.resource, name, fmt.Errorf("resourceVersion does not match"))
	}
	oldSpec := objectField(oldObj, "spec")
	if err := rest.BeforeUpdate(strategy, ctx, newObj, oldObj); err != nil {
		return nil, false, err
	}
	if updateValidation != nil {
		if err := updateValidation(ctx, newObj.DeepCopyObject(), oldObj.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}
	if !reflect.DeepEqual(oldSpec, objectField(newObj, "spec")) {
		newMeta.SetGeneration(oldMeta.GetGeneration() + 1)
	} else {
		newMeta.SetGeneration(oldMeta.GetGeneration())
	}
	if oldMeta.GetDeletionTimestamp() != nil && len(newMeta.GetFinalizers()) == 0 {
		if dryrun.IsDryRun(options.DryRun) {
			return newObj, false, nil
		}
		if s.deleteHook != nil {
			if err := s.deleteHook(ctx, name); err != nil {
				return nil, false, apierrors.NewInternalError(err)
			}
		}
		if err := s.client.Delete(ctx, s.resourceName, hit.ID, hit.SeqNo, hit.PrimaryTerm); err != nil {
			return nil, false, s.apiError(err, name)
		}
		return newObj, false, nil
	}
	if dryrun.IsDryRun(options.DryRun) {
		return newObj, false, nil
	}
	doc, id, err := s.documentFor(ctx, newObj)
	if err != nil {
		return nil, false, err
	}
	doc.Credential = hit.Document.Credential
	version, err := s.client.Update(ctx, s.resourceName, id, doc, hit.SeqNo, hit.PrimaryTerm)
	if err != nil {
		return nil, false, s.apiError(err, name)
	}
	newMeta.SetResourceVersion(encodeVersion(version.SeqNo, version.PrimaryTerm))
	return newObj, false, nil
}

func (s *Store) Delete(ctx context.Context, name string, admission rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if options == nil {
		options = &metav1.DeleteOptions{}
	}
	hit, err := s.client.Get(ctx, s.resourceName, s.documentID(ctx, name))
	if err != nil {
		return nil, false, s.apiError(err, name)
	}
	obj, err := s.objectFromHit(hit)
	if err != nil {
		return nil, false, err
	}
	accessor, _ := meta.Accessor(obj)
	if _, _, err := rest.BeforeDelete(s.deleteStrategy, ctx, obj, options); err != nil {
		return nil, false, err
	}
	if options.Preconditions != nil {
		if options.Preconditions.UID != nil && *options.Preconditions.UID != accessor.GetUID() {
			return nil, false, apierrors.NewConflict(s.resource, name, fmt.Errorf("UID precondition failed"))
		}
		if options.Preconditions.ResourceVersion != nil && *options.Preconditions.ResourceVersion != accessor.GetResourceVersion() {
			return nil, false, apierrors.NewConflict(s.resource, name, fmt.Errorf("resourceVersion precondition failed"))
		}
	}
	if admission != nil {
		if err := admission(ctx, obj.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}
	if len(accessor.GetFinalizers()) > 0 {
		if accessor.GetDeletionTimestamp() == nil {
			now := metav1.Now()
			accessor.SetDeletionTimestamp(&now)
			zero := int64(0)
			accessor.SetDeletionGracePeriodSeconds(&zero)
			if !dryrun.IsDryRun(options.DryRun) {
				doc, id, docErr := s.documentFor(ctx, obj)
				if docErr != nil {
					return nil, false, docErr
				}
				doc.Credential = hit.Document.Credential
				version, updateErr := s.client.Update(ctx, s.resourceName, id, doc, hit.SeqNo, hit.PrimaryTerm)
				if updateErr != nil {
					return nil, false, s.apiError(updateErr, name)
				}
				accessor.SetResourceVersion(encodeVersion(version.SeqNo, version.PrimaryTerm))
			}
		}
		return obj, false, nil
	}
	if !dryrun.IsDryRun(options.DryRun) {
		if s.deleteHook != nil {
			if err := s.deleteHook(ctx, name); err != nil {
				return nil, false, apierrors.NewInternalError(err)
			}
		}
		if err := s.client.Delete(ctx, s.resourceName, hit.ID, hit.SeqNo, hit.PrimaryTerm); err != nil {
			return nil, false, s.apiError(err, name)
		}
	}
	return obj, true, nil
}

func (s *Store) DeleteCollection(ctx context.Context, admission rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *internalversion.ListOptions) (runtime.Object, error) {
	list, err := s.List(ctx, listOptions)
	if err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	for _, item := range items {
		accessor, _ := meta.Accessor(item)
		if _, _, err := s.Delete(ctx, accessor.GetName(), admission, options); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (s *Store) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	if options == nil {
		options = &internalversion.ListOptions{}
	}
	namespace := s.requestNamespace(ctx)
	query, err := selectorQuery(namespace, options)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	fingerprint := queryFingerprint(s.resourceName, namespace, options)
	token, err := decodeContinue(options.Continue, fingerprint)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	pitID := token.PIT
	if pitID == "" {
		pitID, err = s.client.OpenPIT(ctx, s.resourceName, pitKeepAlive)
		if err != nil {
			return nil, s.apiError(err, "")
		}
	}
	limit := options.Limit
	if limit < 0 {
		_ = s.client.ClosePIT(ctx, pitID)
		return nil, apierrors.NewBadRequest("limit must not be negative")
	}
	var hits []es.Hit
	var total int64
	searchAfter := token.SearchAfter
	for {
		size := limit
		if size == 0 {
			size = scanBatchSize
		}
		result, searchErr := s.client.SearchPIT(ctx, pitID, pitKeepAlive, query, size, searchAfter)
		if searchErr != nil {
			return nil, s.apiError(searchErr, "")
		}
		if result.PITID != "" {
			pitID = result.PITID
		}
		total = result.Total
		hits = append(hits, result.Hits...)
		if limit > 0 || int64(len(result.Hits)) < size {
			break
		}
		searchAfter = result.Hits[len(result.Hits)-1].Sort
	}
	list := s.newListFunc()
	objects := make([]runtime.Object, 0, len(hits))
	for i := range hits {
		obj, objErr := s.objectFromHit(&hits[i])
		if objErr != nil {
			return nil, objErr
		}
		objects = append(objects, obj)
	}
	if err := meta.SetList(list, objects); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	listMeta, err := meta.ListAccessor(list)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	consumed := token.Consumed + int64(len(hits))
	if limit > 0 && consumed < total && len(hits) > 0 {
		next, tokenErr := encodeContinue(continueToken{
			Version: 1, PIT: pitID, SearchAfter: hits[len(hits)-1].Sort,
			Fingerprint: fingerprint, Consumed: consumed, ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
		})
		if tokenErr != nil {
			return nil, apierrors.NewInternalError(tokenErr)
		}
		listMeta.SetContinue(next)
		remaining := total - consumed
		listMeta.SetRemainingItemCount(&remaining)
	} else {
		_ = s.client.ClosePIT(ctx, pitID)
		remaining := int64(0)
		listMeta.SetRemainingItemCount(&remaining)
	}
	return list, nil
}

func (s *StatusStore) New() runtime.Object   { return s.parent.New() }
func (s *StatusStore) NamespaceScoped() bool { return s.parent.NamespaceScoped() }
func (s *StatusStore) Destroy()              {}
func (s *StatusStore) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.parent.Get(ctx, name, options)
}
func (s *StatusStore) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	return s.parent.update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options, s.updateStrategy)
}

func (s *Store) documentID(ctx context.Context, name string) string {
	if ns := s.requestNamespace(ctx); ns != "" {
		return ns + "/" + name
	}
	return name
}

func (s *Store) requestNamespace(ctx context.Context) string {
	if !s.namespaceScoped {
		return ""
	}
	ns, _ := genericapirequest.NamespaceFrom(ctx)
	return ns
}

func (s *Store) documentFor(ctx context.Context, obj runtime.Object) (es.Document, string, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return es.Document{}, "", apierrors.NewInternalError(err)
	}
	if s.namespaceScoped {
		expected := s.requestNamespace(ctx)
		if accessor.GetNamespace() == "" {
			accessor.SetNamespace(expected)
		}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return es.Document{}, "", apierrors.NewInternalError(err)
	}
	labelKeys := make([]string, 0, len(accessor.GetLabels()))
	for key := range accessor.GetLabels() {
		labelKeys = append(labelKeys, key)
	}
	sort.Strings(labelKeys)
	labels := make([]es.Label, 0, len(labelKeys))
	for _, key := range labelKeys {
		labels = append(labels, es.Label{Key: key, Value: accessor.GetLabels()[key]})
	}
	id := accessor.GetName()
	if accessor.GetNamespace() != "" {
		id = accessor.GetNamespace() + "/" + id
	}
	apiVersion := "ebs/v1"
	if s.resource.Group != "" && s.resource.Group != "ebs" {
		apiVersion = s.resource.Group + "/v1"
	}
	return es.Document{
		APIVersion: apiVersion, Kind: s.kind, DocumentID: id,
		Metadata: es.Metadata{
			Name: accessor.GetName(), Namespace: accessor.GetNamespace(),
			CreationTimestamp: accessor.GetCreationTimestamp().UTC().Format(time.RFC3339Nano), Labels: labels,
		},
		Data: data,
	}, id, nil
}

func (s *Store) objectFromHit(hit *es.Hit) (runtime.Object, error) {
	obj := s.newFunc()
	if err := json.Unmarshal(hit.Document.Data, obj); err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("decode %s from Elasticsearch: %w", s.kind, err))
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	accessor.SetResourceVersion(encodeVersion(hit.SeqNo, hit.PrimaryTerm))
	return obj, nil
}

func (s *Store) apiError(err error, name string) error {
	switch {
	case es.IsStatus(err, 404):
		return apierrors.NewNotFound(s.resource, name)
	case es.IsStatus(err, 409):
		return apierrors.NewConflict(s.resource, name, fmt.Errorf("resourceVersion conflict"))
	case es.IsStatus(err, 400):
		return apierrors.NewBadRequest(err.Error())
	default:
		return apierrors.NewServiceUnavailable(err.Error())
	}
}

func encodeVersion(seqNo, primaryTerm int64) string {
	return "v1:" + strconv.FormatInt(seqNo, 10) + ":" + strconv.FormatInt(primaryTerm, 10)
}

func objectField(obj runtime.Object, field string) interface{} {
	data, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	return data[field]
}

type continueToken struct {
	Version     int               `json:"v"`
	PIT         string            `json:"pit"`
	SearchAfter []json.RawMessage `json:"after,omitempty"`
	Fingerprint string            `json:"query"`
	Consumed    int64             `json:"consumed"`
	ExpiresAt   int64             `json:"expires"`
	Checksum    string            `json:"checksum,omitempty"`
}

func encodeContinue(token continueToken) (string, error) {
	token.Checksum = ""
	unsigned, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(unsigned)
	token.Checksum = hex.EncodeToString(sum[:])
	data, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeContinue(value, fingerprint string) (continueToken, error) {
	if value == "" {
		return continueToken{Version: 1, Fingerprint: fingerprint}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return continueToken{}, fmt.Errorf("invalid continue token encoding")
	}
	var token continueToken
	if err := json.Unmarshal(data, &token); err != nil {
		return continueToken{}, fmt.Errorf("invalid continue token")
	}
	checksum := token.Checksum
	token.Checksum = ""
	unsigned, _ := json.Marshal(token)
	sum := sha256.Sum256(unsigned)
	if checksum != hex.EncodeToString(sum[:]) {
		return continueToken{}, fmt.Errorf("continue token checksum mismatch")
	}
	if token.Version != 1 || token.PIT == "" || token.Fingerprint != fingerprint {
		return continueToken{}, fmt.Errorf("continue token does not match this query")
	}
	if token.ExpiresAt <= time.Now().Unix() {
		return continueToken{}, fmt.Errorf("continue token has expired")
	}
	token.Checksum = checksum
	return token, nil
}

func queryFingerprint(resource, namespace string, options *internalversion.ListOptions) string {
	var labelSelector, fieldSelector string
	if options.LabelSelector != nil {
		labelSelector = options.LabelSelector.String()
	}
	if options.FieldSelector != nil {
		fieldSelector = options.FieldSelector.String()
	}
	raw := resource + "\x00" + namespace + "\x00" + labelSelector + "\x00" + fieldSelector
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func selectorQuery(namespace string, options *internalversion.ListOptions) (map[string]interface{}, error) {
	must := make([]interface{}, 0)
	mustNot := make([]interface{}, 0)
	if namespace != "" {
		must = append(must, term("metadata.namespace", namespace))
	}
	if options.FieldSelector != nil {
		for _, req := range options.FieldSelector.Requirements() {
			field := req.Field
			if field != "metadata.name" && field != "metadata.namespace" {
				return nil, fmt.Errorf("field selector %q is not supported", field)
			}
			switch req.Operator {
			case selection.Equals, selection.DoubleEquals:
				must = append(must, term(field, req.Value))
			case selection.NotEquals:
				mustNot = append(mustNot, term(field, req.Value))
			default:
				return nil, fmt.Errorf("operator %q is not supported for field %q", req.Operator, field)
			}
		}
	}
	if options.LabelSelector != nil {
		reqs, selectable := options.LabelSelector.Requirements()
		if !selectable {
			must = append(must, map[string]interface{}{"match_none": map[string]interface{}{}})
		}
		for _, req := range reqs {
			key := req.Key()
			values := req.Values().List()
			var nested map[string]interface{}
			switch req.Operator() {
			case selection.Equals, selection.DoubleEquals, selection.In:
				nested = nestedLabel(key, values, true)
				must = append(must, nested)
			case selection.NotEquals, selection.NotIn:
				nested = nestedLabel(key, values, true)
				mustNot = append(mustNot, nested)
			case selection.Exists:
				must = append(must, nestedLabel(key, nil, false))
			case selection.DoesNotExist:
				mustNot = append(mustNot, nestedLabel(key, nil, false))
			default:
				return nil, fmt.Errorf("label selector operator %q is not supported", req.Operator())
			}
		}
	}
	if len(must) == 0 && len(mustNot) == 0 {
		return map[string]interface{}{"match_all": map[string]interface{}{}}, nil
	}
	boolQuery := map[string]interface{}{}
	if len(must) > 0 {
		boolQuery["filter"] = must
	}
	if len(mustNot) > 0 {
		boolQuery["must_not"] = mustNot
	}
	return map[string]interface{}{"bool": boolQuery}, nil
}

func term(field, value string) map[string]interface{} {
	return map[string]interface{}{"term": map[string]string{field: value}}
}

func nestedLabel(key string, values []string, withValue bool) map[string]interface{} {
	filter := []interface{}{term("metadata.labels.key", key)}
	if withValue {
		if len(values) == 1 {
			filter = append(filter, term("metadata.labels.value", values[0]))
		} else {
			filter = append(filter, map[string]interface{}{"terms": map[string][]string{"metadata.labels.value": values}})
		}
	}
	return map[string]interface{}{"nested": map[string]interface{}{
		"path":  "metadata.labels",
		"query": map[string]interface{}{"bool": map[string]interface{}{"filter": filter}},
	}}
}

var (
	_ rest.Storage           = (*Store)(nil)
	_ rest.Getter            = (*Store)(nil)
	_ rest.Lister            = (*Store)(nil)
	_ rest.Creater           = (*Store)(nil)
	_ rest.Updater           = (*Store)(nil)
	_ rest.GracefulDeleter   = (*Store)(nil)
	_ rest.CollectionDeleter = (*Store)(nil)
	_ rest.Storage           = (*StatusStore)(nil)
	_ rest.Getter            = (*StatusStore)(nil)
	_ rest.Updater           = (*StatusStore)(nil)
)
