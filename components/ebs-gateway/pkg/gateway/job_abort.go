package gateway

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
)

func (g *Gateway) jobAbortHandler(identity Identity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readAndRestoreBody(r, g.cfg.MaxRequestBodyBytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var input struct {
			UID    string `json:"uid"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(body, &input)
		if len(input.Reason) > 4096 {
			input.Reason = input.Reason[:4096]
		}
		if len(input.UID) > 256 {
			input.UID = input.UID[:256]
		}
		recorder := &abortAuditResponse{ResponseWriter: w, status: 200}
		g.proxy.ServeHTTP(recorder, r)
		var result struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		_ = json.Unmarshal(recorder.body.Bytes(), &result)
		route := parseRoute(r.URL.Path)
		log.Printf("operation=JobAbort operator=%q project=%q job=%q uid=%q reason=%q status=%d phase=%q", identity.Subject, route.project, route.name, input.UID, input.Reason, recorder.status, result.Status.Phase)
	}
}

type abortAuditResponse struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *abortAuditResponse) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *abortAuditResponse) Write(data []byte) (int, error) {
	if w.body.Len()+len(data) <= 1<<20 {
		_, _ = w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}
func (w *abortAuditResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
