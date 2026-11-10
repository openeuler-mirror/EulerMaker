package server

import "net/http"

func (s *Server) deleteJobData(w http.ResponseWriter, r *http.Request, project, job string) {
	if !validIdentifier(project) || !validIdentifier(job) {
		writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "invalid project or job name", false, nil)
		return
	}
	if r.ContentLength != 0 {
		writeErr(w, r, http.StatusBadRequest, "InvalidRequest", "request body is not allowed", false, nil)
		return
	}
	if err := s.store.DeleteJob(project, job); err != nil {
		recordFailure(r, "", err)
		w.Header().Set("Retry-After", "5")
		writeErr(w, r, http.StatusServiceUnavailable, "JobDataCleanupFailed", "failed to delete job data", true, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
