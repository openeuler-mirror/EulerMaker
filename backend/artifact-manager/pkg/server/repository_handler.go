package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"artifact-manager/pkg/fileutil"
	"artifact-manager/pkg/repository"
)

func (s *Server) createRepository(c *gin.Context) {
	w, r := c.Writer, c.Request
	var body struct {
		RepositoryUID     string                         `json:"repositoryUID"`
		RepositoryName    string                         `json:"repositoryName"`
		TargetArch        string                         `json:"targetArch"`
		BaseRepositoryUID string                         `json:"baseRepositoryUID,omitempty"`
		Manifests         []repository.ManifestReference `json:"manifests"`
	}
	if err := decodeJSON(r.Body, s.cfg.Upload.MaxMetadataSize, &body); err != nil {
		writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "invalid request body", false, nil)
		return
	}
	request := repository.CreateRepositoryRequest{
		Project:           c.Param("project"),
		BuildName:         c.Param("build"),
		RepositoryUID:     body.RepositoryUID,
		RepositoryName:    body.RepositoryName,
		TargetArch:        body.TargetArch,
		BaseRepositoryUID: body.BaseRepositoryUID,
		Manifests:         body.Manifests,
	}
	record, status, err := s.repositories.Submit(request)
	if err != nil {
		s.writeRepositoryError(w, r, err)
		return
	}
	w.Header().Set("Location", r.URL.Path+"/"+record.RepositoryUID)
	writeJSON(w, status, repository.ResponseFor(record))
}

func (s *Server) getRepository(c *gin.Context) {
	w, r := c.Writer, c.Request
	uid := c.Param("uid")
	if !validIdentifier(uid) {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	record, ok, err := s.repositories.Get(uid)
	if err != nil {
		s.writeRepositoryError(w, r, &repository.Error{
			Code:      "RepositoryStorageUnavailable",
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		})
		return
	}
	if !ok || record.Project != c.Param("project") || record.BuildName != c.Param("build") {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	writeJSON(w, http.StatusOK, repository.ResponseFor(record))
}

func (s *Server) writeRepositoryError(w http.ResponseWriter, r *http.Request, err error) {
	var typed *repository.Error
	if !errors.As(err, &typed) {
		typed = &repository.Error{Code: "RepositoryInternalError", Status: http.StatusInternalServerError, Retryable: true}
	}
	if typed.Status == 0 {
		typed.Status = http.StatusInternalServerError
	}
	if typed.Status == http.StatusTooManyRequests || typed.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeErr(w, r, typed.Status, typed.Code, typed.Code, typed.Retryable, nil)
}

func (s *Server) repositoryContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		method(w, "GET, HEAD")
		return
	}
	if strings.Contains(r.Header.Get("Range"), ",") {
		writeErr(w, r, http.StatusRequestedRangeNotSatisfiable, "MultipleRangesNotSupported", "multiple ranges are not supported", false, nil)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repositories/v1/"), "/", 2)
	if len(parts) != 2 || !validIdentifier(parts[0]) {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	record, ok, err := s.repositories.Get(parts[0])
	if err != nil {
		s.writeRepositoryError(w, r, &repository.Error{
			Code:      "RepositoryStorageUnavailable",
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
		})
		return
	}
	if !ok {
		writeErr(w, r, http.StatusNotFound, "RepositoryNotFound", "repository not found", false, nil)
		return
	}
	if record.State == repository.RepositoryCreating {
		writeErr(w, r, http.StatusConflict, "RepositoryNotReady", "repository is not ready", true, nil)
		return
	}
	if record.State != repository.RepositoryReady {
		writeErr(w, r, http.StatusGone, "RepositoryUnavailable", "repository is unavailable", false, nil)
		return
	}
	relative, err := fileutil.SafeRelative(parts[1])
	if err != nil || relative == ".repository.json" {
		writeErr(w, r, http.StatusNotFound, "RepositoryContentNotFound", "repository content not found", false, nil)
		return
	}
	path := filepath.Join(repository.VersionPath(s.cfg.DataDir, record.Project, record.TargetArch, record.BuildName, record.RepositoryUID), filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeErr(w, r, http.StatusNotFound, "RepositoryContentNotFound", "repository content not found", false, nil)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeErr(w, r, http.StatusNotFound, "RepositoryContentNotFound", "repository content not found", false, nil)
		return
	}
	defer file.Close()
	sum, err := fileutil.FileSHA256(path)
	if err != nil {
		writeErr(w, r, http.StatusServiceUnavailable, "RepositoryStorageUnavailable", "repository storage unavailable", true, nil)
		return
	}
	w.Header().Set("ETag", `"`+sum+`"`)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
