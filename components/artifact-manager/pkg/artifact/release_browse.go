package artifact

import (
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const releaseBrowsePageSize = 200

var releaseBrowseTemplate = template.Must(template.New("browse").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Index of {{.Path}}</title></head>
<body><h1>Index of {{.Path}}</h1><ul>
{{range .Entries}}<li><a href="{{.URL}}">{{.Name}}</a></li>{{end}}
</ul>{{if .Previous}}<a href="{{.Previous}}">Previous</a>{{end}} {{if .Next}}<a href="{{.Next}}">Next</a>{{end}}</body></html>
`))

type releaseBrowseEntry struct {
	Name string
	URL  string
}

func (s *Server) browseCurrentRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		method(w, "GET, HEAD")
		return
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repositories/"), "/"), "/")
	if len(parts) < 1 || len(parts) > 4 {
		s.releaseNotFound(w, r)
		return
	}
	for _, part := range parts {
		if !validIdentifier(part) {
			s.releaseNotFound(w, r)
			return
		}
	}
	if len(parts) == 4 && parts[3] != "Packages" && parts[3] != "repodata" {
		s.releaseNotFound(w, r)
		return
	}

	var entries []releaseBrowseEntry
	root := filepath.Join(s.cfg.DataDir, "repositories")
	switch len(parts) {
	case 1:
		for _, targetOS := range browseDirectoryNames(filepath.Join(root, parts[0])) {
			for _, targetArch := range browseDirectoryNames(filepath.Join(root, parts[0], targetOS)) {
				if s.currentRelease(parts[0], targetOS, targetArch) != nil {
					entries = append(entries, releaseBrowseEntry{Name: targetOS + "/", URL: url.PathEscape(targetOS) + "/"})
					break
				}
			}
		}
	case 2:
		for _, targetArch := range browseDirectoryNames(filepath.Join(root, parts[0], parts[1])) {
			if s.currentRelease(parts[0], parts[1], targetArch) != nil {
				entries = append(entries, releaseBrowseEntry{Name: targetArch + "/", URL: url.PathEscape(targetArch) + "/"})
			}
		}
	case 3, 4:
		record := s.currentRelease(parts[0], parts[1], parts[2])
		if record == nil {
			s.releaseNotFound(w, r)
			return
		}
		if len(parts) == 3 {
			entries = []releaseBrowseEntry{{Name: "Packages/", URL: "Packages/"}, {Name: "repodata/", URL: "repodata/"}}
			key := filepath.Join(s.releases.releasePath(record), "RPM-GPG-KEY-openEuler")
			if info, err := os.Lstat(key); err == nil && info.Mode().IsRegular() {
				entries = append(entries, releaseBrowseEntry{Name: "RPM-GPG-KEY-openEuler", URL: "RPM-GPG-KEY-openEuler"})
			}
		} else {
			contentDir := filepath.Join(s.releases.releasePath(record), parts[3])
			for _, name := range browseFileNames(contentDir) {
				entries = append(entries, releaseBrowseEntry{Name: name, URL: url.PathEscape(name)})
			}
		}
	}
	if len(entries) == 0 && len(parts) < 3 {
		s.releaseNotFound(w, r)
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			writeErr(w, r, http.StatusBadRequest, "InvalidPage", "page must be a positive integer", false, nil)
			return
		}
	}
	pageCount := (len(entries) + releaseBrowsePageSize - 1) / releaseBrowsePageSize
	if page > pageCount && page != 1 {
		s.releaseNotFound(w, r)
		return
	}
	start := (page - 1) * releaseBrowsePageSize
	end := start + releaseBrowsePageSize
	if end > len(entries) {
		end = len(entries)
	}
	view := struct {
		Path, Previous, Next string
		Entries              []releaseBrowseEntry
	}{Path: r.URL.Path, Entries: entries[start:end]}
	if page > 1 {
		view.Previous = "?page=" + strconv.Itoa(page-1)
	}
	if end < len(entries) {
		view.Next = "?page=" + strconv.Itoa(page+1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = releaseBrowseTemplate.Execute(w, view)
}

func browseDirectoryNames(path string) []string {
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		return nil
	}
	items, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var names []string
	for _, item := range items {
		if item.IsDir() && validIdentifier(item.Name()) {
			names = append(names, item.Name())
		}
	}
	return names
}

func browseFileNames(path string) []string {
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		return nil
	}
	items, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var names []string
	for _, item := range items {
		if info, err := item.Info(); err == nil && info.Mode().IsRegular() && !strings.ContainsAny(item.Name(), "\\\x00\r\n") {
			names = append(names, item.Name())
		}
	}
	return names
}
