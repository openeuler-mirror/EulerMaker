package oldjob

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

type oldJobTransport func(*http.Request) (*http.Response, error)

func (f oldJobTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func oldJobResponse(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}
}

func newTestStorage(client *es.Client) *Storage {
	template := NewStorage()
	return NewLifecycle(esstore.New(client, "oldjob", "Job", template.Resource.(*genericregistry.Store)))
}

func TestOldJobsDelete(t *testing.T) {
	for _, tc := range []struct {
		name         string
		changeJob    func(*ebsv1.Job)
		changeOpts   func(*metav1.DeleteOptions)
		getStatus    int
		deleteStatus int
		admissionErr error
		wantReason   metav1.StatusReason
		wantDeletes  int
	}{
		{name: "conditional delete", wantDeletes: 1},
		{name: "without preconditions", changeOpts: func(o *metav1.DeleteOptions) { o.Preconditions = nil }, wantDeletes: 1},
		{name: "business eligibility belongs to caller", changeJob: func(j *ebsv1.Job) {
			j.Status.Phase = ebsv1.JobRunning
			j.Status.EndTime = metav1.Time{}
			j.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
		}, wantDeletes: 1},
		{name: "UID precondition", changeOpts: func(o *metav1.DeleteOptions) { o.Preconditions.ResourceVersion = nil }, wantDeletes: 1},
		{name: "version precondition", changeOpts: func(o *metav1.DeleteOptions) { o.Preconditions.UID = nil }, wantDeletes: 1},
		{name: "dry run", changeOpts: func(o *metav1.DeleteOptions) { o.DryRun = []string{metav1.DryRunAll} }},
		{name: "already deleting", changeJob: func(j *ebsv1.Job) {
			now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
			j.DeletionTimestamp = &now
			j.Finalizers = []string{ebsv1.JobCleanupFinalizer}
		}},
		{name: "archive already absent", getStatus: 404, wantReason: metav1.StatusReasonNotFound},
		{name: "concurrent removal", deleteStatus: 404, wantDeletes: 1, wantReason: metav1.StatusReasonNotFound},
		{name: "concurrent archive update", deleteStatus: 409, wantDeletes: 1, wantReason: metav1.StatusReasonConflict},
		{name: "delete unavailable", deleteStatus: 503, wantDeletes: 1, wantReason: metav1.StatusReasonServiceUnavailable},
		{name: "archive read unavailable", getStatus: 503, wantReason: metav1.StatusReasonServiceUnavailable},
		{name: "UID changed", changeJob: func(j *ebsv1.Job) { j.UID = "another" }, wantReason: metav1.StatusReasonConflict},
		{name: "ES version changed", changeOpts: func(o *metav1.DeleteOptions) { v := "v1:98:7"; o.Preconditions.ResourceVersion = &v }, wantReason: metav1.StatusReasonConflict},
		{name: "admission failure", admissionErr: apierrors.NewForbidden(ebsv1.Resource("oldjobs"), "job", nil), wantReason: metav1.StatusReasonForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
			job := &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "v1:99:7",
			}, Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded, EndTime: now}}
			uid, version := types.UID("uid"), "v1:99:7"
			opts := &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}
			if tc.changeJob != nil {
				tc.changeJob(job)
			}
			if tc.changeOpts != nil {
				tc.changeOpts(opts)
			}
			deletes := 0
			client := es.NewClientForTesting("http://es", &http.Client{Transport: oldJobTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
					t.Fatalf("unexpected ES path: %s", r.URL)
				}
				switch r.Method {
				case http.MethodGet:
					if r.URL.RawQuery != "" {
						t.Fatalf("delete must read complete metadata: %s", r.URL)
					}
					if tc.getStatus != 0 {
						return oldJobResponse(tc.getStatus, map[string]string{"error": "read failed"}), nil
					}
					return oldJobResponse(200, map[string]any{
						"_id": "project/job", "_seq_no": 99, "_primary_term": 7,
						"_source": map[string]any{"data": job},
					}), nil
				case http.MethodPut:
					deletes++
					q := r.URL.Query()
					if q.Get("if_seq_no") != "99" || q.Get("if_primary_term") != "7" || q.Get("refresh") != "wait_for" {
						t.Fatalf("delete must use ES CAS, not source version 123: %s", r.URL)
					}
					if tc.deleteStatus != 0 {
						return oldJobResponse(tc.deleteStatus, map[string]string{"error": "delete failed"}), nil
					}
					var doc es.Document
					if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
						t.Fatal(err)
					}
					var updated ebsv1.Job
					if err := json.Unmarshal(doc.Data, &updated); err != nil {
						t.Fatal(err)
					}
					if updated.DeletionTimestamp == nil || len(updated.Finalizers) == 0 {
						t.Fatalf("invalid deletion intent: %+v", doc)
					}
					return oldJobResponse(200, map[string]any{"_seq_no": 100, "_primary_term": 7}), nil
				default:
					t.Fatalf("unexpected ES request: %s", r.Method)
					return nil, nil
				}
			})})
			store := newTestStorage(client)
			ctx := esstore.WithSourceFilter(apirequest.WithNamespace(context.Background(), "project"), es.SourceFilter{Includes: []string{"data.status"}})
			admitted := false
			admission := func(_ context.Context, obj runtime.Object) error {
				admitted = true
				if !equality.Semantic.DeepEqual(obj, job) {
					t.Fatalf("admission snapshot=%+v", obj)
				}
				return tc.admissionErr
			}
			out, deleted, err := store.Delete(ctx, "job", admission, opts)
			if deletes != tc.wantDeletes {
				t.Fatalf("delete requests=%d want=%d", deletes, tc.wantDeletes)
			}
			if tc.wantReason != "" {
				if err == nil || apierrors.ReasonForError(err) != tc.wantReason || deleted {
					t.Fatalf("deleted=%v err=%v want=%s", deleted, err, tc.wantReason)
				}
				return
			}
			if err != nil || deleted || !admitted || out.(*ebsv1.Job).DeletionTimestamp == nil || !equality.Semantic.DeepEqual(out.(*ebsv1.Job).Status, job.Status) {
				t.Fatalf("delete=%+v deleted=%v admitted=%v err=%v", out, deleted, admitted, err)
			}
		})
	}
}

var _ rest.GracefulDeleter = (*Storage)(nil)

func TestOldJobsArchiveLifecycle(t *testing.T) {
	var stored *es.Document
	var revision int
	client := es.NewClientForTesting("http://es", &http.Client{Transport: oldJobTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			if stored == nil {
				return oldJobResponse(404, map[string]any{"found": false}), nil
			}
			return oldJobResponse(200, map[string]any{"_id": "project/job", "_seq_no": revision, "_primary_term": 1, "_source": stored}), nil
		}
		if r.Method != http.MethodPut && r.Method != http.MethodDelete {
			t.Fatalf("unexpected method %s", r.Method)
		}
		q := r.URL.Query()
		if q.Get("op_type") == "create" {
			if stored != nil {
				return oldJobResponse(409, map[string]string{"error": "exists"}), nil
			}
		} else if q.Get("if_seq_no") != fmt.Sprint(revision) || q.Get("if_primary_term") != "1" {
			return oldJobResponse(409, map[string]string{"error": "conflict"}), nil
		}
		if r.Method == http.MethodDelete {
			stored = nil
			return oldJobResponse(200, map[string]string{"result": "deleted"}), nil
		}
		if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
			t.Fatal(err)
		}
		revision++
		return oldJobResponse(200, map[string]any{"_seq_no": revision, "_primary_term": 1}), nil
	})})
	store := newTestStorage(client)
	ctx := apirequest.WithNamespace(context.Background(), "project")
	now := metav1.NewTime(time.Unix(100, 0))
	job := &ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "10",
			DeletionTimestamp: &now, Finalizers: []string{ebsv1.JobArchiveFinalizer, "example.test/other"},
			Annotations: map[string]string{ebsv1.JobArchiveActionAnnotation: "retain"}},
		Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded, EndTime: now},
	}
	old, err := store.Archive(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if old.ResourceVersion != "v1:1:1" || old.DeletionTimestamp != nil || !reflect.DeepEqual(old.Finalizers, []string{ebsv1.JobCleanupFinalizer}) {
		t.Fatalf("independent lifecycle: %+v", old.ObjectMeta)
	}
	digest, err := SnapshotDigest(job)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ArchiveState(ctx, job.Name)
	if err != nil || state.UID != "uid" || state.Action != "retain" || state.Digest != digest || state.Phase != "Stored" {
		t.Fatalf("confirmation=%+v err=%v", state, err)
	}
	if _, err := store.Archive(ctx, job); err != nil || revision != 1 {
		t.Fatalf("repeat archive: %v revision=%d", err, revision)
	}
	lifecycleChanged := job.DeepCopy()
	lifecycleChanged.ResourceVersion = "42"
	lifecycleChanged.Finalizers = []string{ebsv1.JobArchiveFinalizer}
	lifecycleChanged.Annotations[ebsv1.JobArchiveActionAnnotation] = "delete"
	if _, err := store.Archive(ctx, lifecycleChanged); err != nil || revision != 1 {
		t.Fatalf("lifecycle metadata changed snapshot: %v revision=%d", err, revision)
	}
	changed := job.DeepCopy()
	changed.Status.Message = "changed"
	if _, err := store.Archive(ctx, changed); !apierrors.IsConflict(err) {
		t.Fatalf("frozen snapshot error=%v", err)
	}
	pre := &metav1.Preconditions{UID: &old.UID, ResourceVersion: &old.ResourceVersion}
	obj, deleted, err := store.Delete(ctx, old.Name, nil, &metav1.DeleteOptions{Preconditions: pre})
	if err != nil || deleted {
		t.Fatalf("mark delete: %v deleted=%v", err, deleted)
	}
	old = obj.(*ebsv1.Job)
	if old.DeletionTimestamp == nil {
		t.Fatal("missing deletion intent")
	}
	state, err = store.ArchiveState(ctx, job.Name)
	if err != nil || state.Action != "delete" || state.Digest != digest {
		t.Fatalf("deleting confirmation=%+v err=%v", state, err)
	}
	if _, err := store.Archive(ctx, job); err != nil {
		t.Fatalf("confirm deleting archive: %v", err)
	}
	if _, err := store.Finalize(ctx, old.Name, pre); !apierrors.IsConflict(err) {
		t.Fatalf("stale CAS: %v", err)
	}
	pre.ResourceVersion = &old.ResourceVersion
	if _, err := store.Finalize(ctx, old.Name, pre); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("cleanup should delete the document: %+v", stored)
	}
	if _, err := store.Get(ctx, old.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("public GET: %v", err)
	}
	if _, err := store.ArchiveState(ctx, job.Name); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted archive confirmation: %v", err)
	}

}

func TestOldJobsFinalize(t *testing.T) {
	for _, tc := range []struct {
		name           string
		otherFinalizer bool
		deleteStatus   int
		lostResponse   bool
	}{
		{name: "delete"},
		{name: "other finalizer", otherFinalizer: true},
		{name: "CAS conflict", deleteStatus: 409},
		{name: "delete unavailable", deleteStatus: 503},
		{name: "delete response lost", lostResponse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.Now()
			stored := &ebsv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "job", Namespace: "project", UID: "uid", DeletionTimestamp: &now,
				Finalizers: []string{ebsv1.JobCleanupFinalizer},
			}}
			if tc.otherFinalizer {
				stored.Finalizers = append(stored.Finalizers, "example.test/other")
			}
			writes := 0
			client := es.NewClientForTesting("http://es", &http.Client{Transport: oldJobTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
					t.Fatalf("path=%s", r.URL.Path)
				}
				if r.Method == http.MethodGet {
					if stored == nil {
						return oldJobResponse(404, map[string]any{"found": false}), nil
					}
					return oldJobResponse(200, map[string]any{
						"_id": "project/job", "_seq_no": 7, "_primary_term": 2,
						"_source": map[string]any{"data": stored},
					}), nil
				}
				writes++
				if r.URL.Query().Get("if_seq_no") != "7" || r.URL.Query().Get("if_primary_term") != "2" {
					t.Fatalf("missing ES CAS: %s", r.URL)
				}
				if tc.otherFinalizer {
					if r.Method != http.MethodPut {
						t.Fatalf("must preserve other finalizers: %s", r.Method)
					}
					var doc es.Document
					if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(doc.Data, &stored); err != nil {
						t.Fatal(err)
					}
					return oldJobResponse(200, map[string]any{"_seq_no": 8, "_primary_term": 2}), nil
				}
				if r.Method != http.MethodDelete {
					t.Fatalf("expected physical deletion: %s", r.Method)
				}
				if tc.deleteStatus != 0 {
					return oldJobResponse(tc.deleteStatus, map[string]string{"error": "delete failed"}), nil
				}
				stored = nil
				if tc.lostResponse {
					return nil, fmt.Errorf("response lost after deletion")
				}
				return oldJobResponse(200, map[string]string{"result": "deleted"}), nil
			})})
			store := newTestStorage(client)
			ctx := apirequest.WithNamespace(context.Background(), "project")
			uid, rv := types.UID("uid"), "v1:7:2"
			_, err := store.Finalize(ctx, "job", &metav1.Preconditions{UID: &uid, ResourceVersion: &rv})
			if (err != nil) != (tc.deleteStatus != 0 || tc.lostResponse) || writes != 1 {
				t.Fatalf("writes=%d err=%v", writes, err)
			}
			if tc.otherFinalizer {
				if !reflect.DeepEqual(stored.Finalizers, []string{"example.test/other"}) {
					t.Fatalf("remaining finalizers=%v", stored.Finalizers)
				}
			} else if tc.deleteStatus != 0 {
				if !reflect.DeepEqual(stored.Finalizers, []string{ebsv1.JobCleanupFinalizer}) {
					t.Fatalf("failed deletion changed finalizers: %v", stored.Finalizers)
				}
			} else if _, err := store.Get(ctx, "job", &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("confirmation after deletion: %v", err)
			}
		})
	}
}
