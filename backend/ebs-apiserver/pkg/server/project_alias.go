package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"

	ebsv1 "ebs-api/ebs/v1"
)

var projectScopedResources = map[string]struct{}{
	"snapshots":  {},
	"builds":     {},
	"buildinfos": {},
	"rpmrepos":   {},
	"jobs":       {},
	"oldjobs":    {},
}

// Named GET handlers do not decode ListOptions, so reject watch/version options before routing.
func withOldJobReadOptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := genericapirequest.RequestInfoFrom(r.Context())
		if ok && info.APIGroup == "ebs" && (info.Resource == "oldjobs" || info.Resource == "projects" && info.Subresource == "oldjobs") {
			for key, values := range r.URL.Query() {
				for _, value := range values {
					invalid := (key == "resourceVersion" || key == "resourceVersionMatch") && value != ""
					invalid = invalid || key == "watch" && value != "" && value != "false" && value != "0"
					if invalid {
						responsewriters.ErrorNegotiated(apierrors.NewBadRequest(fmt.Sprintf("oldjobs does not support %s", key)), Codecs, ebsv1.SchemeGroupVersion, w, r)
						return
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func installProjectAliasRoutes(srv *genericapiserver.GenericAPIServer) {
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

	handler := func(req *restful.Request, resp *restful.Response) {
		project := req.PathParameter("project")
		remainder := strings.Trim(req.PathParameter("remainder"), "/")
		parts := strings.Split(remainder, "/")
		if project == "" || len(parts) == 0 {
			http.NotFound(resp.ResponseWriter, req.Request)
			return
		}
		if _, ok := projectScopedResources[parts[0]]; !ok {
			http.NotFound(resp.ResponseWriter, req.Request)
			return
		}

		ctx := genericapirequest.WithNamespace(req.Request.Context(), project)
		rewritten := req.Request.Clone(ctx)
		rewritten.URL = cloneURL(req.Request.URL)
		rewritten.URL.Path = "/apis/ebs/v1/namespaces/" + url.PathEscape(project) + "/" + remainder
		rewritten.URL.RawPath = ""
		rewritten.RequestURI = ""

		srv.Handler.ServeHTTP(resp.ResponseWriter, rewritten)
	}

	for _, method := range []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
	} {
		ws.Route(ws.Method(method).Path("/projects/{project}/{remainder:*}").To(handler))
	}
}

func cloneURL(in *url.URL) *url.URL {
	if in == nil {
		return &url.URL{}
	}
	out := *in
	return &out
}
