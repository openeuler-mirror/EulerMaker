package serviceconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type configGetter struct {
	obj   runtime.Object
	err   error
	calls atomic.Int64
}

func (g *configGetter) Get(_ context.Context, gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	g.calls.Add(1)
	if gvr != source.ConfigsGVR || namespace != "" || name != ebsv1.ServiceConfigName {
		return nil, fmt.Errorf("unexpected Config request")
	}
	return g.obj, g.err
}

func serviceObject(gitURL, artifactURL string) *ebsv1.Config {
	content, _ := json.Marshal(ebsv1.ServiceConfigContent{
		GitServer: ebsv1.ServiceEndpoints{InternalURL: gitURL},
		ArtifactManagers: map[string]ebsv1.ServiceEndpoints{
			"default": {InternalURL: artifactURL, RunnerExternalURL: "https://runner.example"},
		},
	})
	return &ebsv1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: ebsv1.ServiceConfigName, UID: "config-uid", ResourceVersion: "1"},
		Spec:       ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityOpsOnly, Content: string(content)},
	}
}

func gitOptions() gitserver.Config {
	return gitserver.Config{Timeout: time.Second, CacheTTL: time.Minute}
}

func TestRoundSharesConfigurationAndPinsAddresses(t *testing.T) {
	getter := &configGetter{obj: serviceObject("http://git-one:8080/prefix", "http://art-one:8081")}
	provider := New(getter, gitOptions())
	first := NewRound(provider)
	if getter.calls.Load() != 0 {
		t.Fatal("provider eagerly loaded configuration")
	}
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := first.GitClient(context.Background()); err != nil {
				t.Error(err)
			}
			if got, err := first.ArtifactURL(context.Background(), "default"); err != nil || got != "http://art-one:8081" {
				t.Errorf("address=%q error=%v", got, err)
			}
		}()
	}
	workers.Wait()
	if getter.calls.Load() != 1 {
		t.Fatalf("Config GET count=%d", getter.calls.Load())
	}
	oldGit, _ := first.GitClient(context.Background())
	getter.obj = serviceObject("http://git-two:8080", "http://art-two:8081")
	second := NewRound(provider)
	newGit, err := second.GitClient(context.Background())
	if err != nil || newGit == oldGit {
		t.Fatalf("new Git client=%v error=%v", newGit, err)
	}
	if got, _ := first.ArtifactURL(context.Background(), "default"); got != "http://art-one:8081" {
		t.Fatal(got)
	}
	if got, _ := first.GitClient(context.Background()); got != oldGit {
		t.Fatal("in-flight client changed")
	}
	if got, _ := second.ArtifactURL(context.Background(), "default"); got != "http://art-two:8081" {
		t.Fatal(got)
	}
	getter.err = apierrors.NewServiceUnavailable("unavailable")
	if _, err := NewRound(provider).ArtifactURL(context.Background(), "default"); err == nil || controller.IsPermanent(err) {
		t.Fatalf("read failure used stale configuration: %v", err)
	}
}

func TestGitClientCacheIsolatedByAddress(t *testing.T) {
	var firstCalls, secondCalls atomic.Int64
	serve := func(calls *atomic.Int64, clonePath string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/prefix/api/v1/repo/status" {
				t.Errorf("path=%s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sync_time": time.Now().UTC().Format(time.RFC3339), "clone_path": clonePath})
		}))
	}
	first := serve(&firstCalls, "/git/one/repo.git")
	defer first.Close()
	second := serve(&secondCalls, "/git/two/repo.git")
	defer second.Close()
	provider := New(&configGetter{}, gitOptions()).(*provider)
	old, err := provider.GitClient(first.URL + "/prefix")
	if err != nil {
		t.Fatal(err)
	}
	same, _ := provider.GitClient(first.URL + "/prefix")
	if same != old {
		t.Fatal("same address did not reuse client")
	}
	check := func(client gitserver.GitServerClient, cloneURL string) {
		result, err := client.CheckSynced(context.Background(), "https://example.com/repo.git", time.Time{})
		if err != nil || !result.Synced || result.CloneURL != cloneURL {
			t.Fatalf("result=%+v error=%v", result, err)
		}
	}
	check(old, "git-server:///git/one/repo.git")
	current, err := provider.GitClient(second.URL + "/prefix")
	if err != nil {
		t.Fatal(err)
	}
	check(current, "git-server:///git/two/repo.git")
	check(old, "git-server:///git/one/repo.git")
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 || provider.git != current {
		t.Fatal("Git sync cache crossed service addresses")
	}
}

func TestConfigErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"missing", apierrors.NewNotFound(ebsv1.Resource("configs"), ebsv1.ServiceConfigName), false},
		{"timeout", context.DeadlineExceeded, false},
		{"unavailable", apierrors.NewServiceUnavailable("retry"), false},
		{"forbidden", apierrors.NewForbidden(ebsv1.Resource("configs"), ebsv1.ServiceConfigName, fmt.Errorf("denied")), true},
		{"decode", &json.SyntaxError{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(&configGetter{err: tc.err}, gitOptions()).Load(context.Background())
			if !IsError(err) || controller.IsPermanent(err) != tc.permanent {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*ebsv1.Config)
	}{
		{"identity", func(c *ebsv1.Config) { c.Name = "other" }},
		{"version", func(c *ebsv1.Config) { c.ResourceVersion = "" }},
		{"visibility", func(c *ebsv1.Config) { c.Spec.Visibility = ebsv1.ConfigVisibilityPublic }},
		{"content", func(c *ebsv1.Config) { c.Spec.Content = "gitServer: [" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := serviceObject("http://git", "http://art")
			tc.change(obj)
			_, err := New(&configGetter{obj: obj}, gitOptions()).Load(context.Background())
			if !IsError(err) || !controller.IsPermanent(err) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
