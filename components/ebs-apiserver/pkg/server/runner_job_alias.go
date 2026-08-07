package server

import (
	"net/http"
	"strings"

	"github.com/emicklei/go-restful/v3"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	genericapiserver "k8s.io/apiserver/pkg/server"
)

var runnerJobQueryParameters = map[string]struct{}{
	"watch":               {},
	"resourceVersion":     {},
	"timeoutSeconds":      {},
	"allowWatchBookmarks": {},
}

func installRunnerJobAliasRoutes(srv *genericapiserver.GenericAPIServer) {
	var ws *restful.WebService
	for _, existing := range srv.Handler.GoRestfulContainer.RegisteredWebServices() {
		if existing.RootPath() == "/apis/ebs/v1" {
			ws = existing
			break
		}
	}
	if ws == nil {
		return
	}

	ws.Route(ws.GET("/runners/{runner}/jobs").To(func(req *restful.Request, resp *restful.Response) {
		runner := req.PathParameter("runner")
		if len(utilvalidation.IsDNS1123Label(runner)) != 0 {
			http.Error(resp.ResponseWriter, "invalid runner name", http.StatusBadRequest)
			return
		}
		if !runnerJobIdentityAllowed(req.Request, runner) {
			http.Error(resp.ResponseWriter, "forbidden", http.StatusForbidden)
			return
		}
		query := req.Request.URL.Query()
		for key := range query {
			if _, ok := runnerJobQueryParameters[key]; !ok {
				http.Error(resp.ResponseWriter, "unsupported query parameter", http.StatusBadRequest)
				return
			}
		}
		query.Set("fieldSelector", "status.runner="+runner)
		rewritten := req.Request.Clone(req.Request.Context())
		rewritten.URL = cloneURL(req.Request.URL)
		rewritten.URL.Path = "/apis/ebs/v1/jobs"
		rewritten.URL.RawPath = ""
		rewritten.URL.RawQuery = query.Encode()
		rewritten.RequestURI = ""
		srv.Handler.ServeHTTP(resp.ResponseWriter, rewritten)
	}))
}

func runnerJobIdentityAllowed(req *http.Request, runner string) bool {
	scopes := strings.Split(req.Header.Get("X-EBS-Scopes"), ",")
	for _, scope := range scopes {
		switch strings.TrimSpace(scope) {
		case "ebs:system":
			return true
		case "ebs:runner":
			return req.Header.Get("X-EBS-User") == runner
		}
	}
	return false
}
