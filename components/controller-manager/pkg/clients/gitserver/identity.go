package gitserver

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var scpURL = regexp.MustCompile(`^([^@/:[:space:]]+)@([^/:[:space:]]+):(.+)$`)

type ParsedURL struct {
	Original string
	Scheme   string
	User     string
	Host     string
	Port     string
	Path     string
	Key      string
}

func parseRepositoryURL(raw string) (ParsedURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r == 0 || r < 0x20 || r == 0x7f || r == ' ' }) >= 0 {
		return ParsedURL{}, fmt.Errorf("repository URL is empty or contains whitespace/control characters")
	}
	var u *url.URL
	if match := scpURL.FindStringSubmatch(raw); match != nil {
		u = &url.URL{Scheme: "ssh", User: url.User(match[1]), Host: match[2], Path: "/" + match[3]}
	} else {
		var err error
		u, err = url.Parse(raw)
		if err != nil || u.Scheme == "" {
			return ParsedURL{}, fmt.Errorf("invalid repository URL")
		}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" && scheme != "ssh" && scheme != "git" {
		return ParsedURL{}, fmt.Errorf("unsupported repository URL scheme %q", scheme)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return ParsedURL{}, fmt.Errorf("repository URL query and fragment are not allowed")
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
		if _, hasPassword := u.User.Password(); hasPassword && (scheme == "http" || scheme == "https") {
			return ParsedURL{}, fmt.Errorf("HTTP repository URL userinfo with password is not allowed")
		}
		if (scheme == "http" || scheme == "https") && user != "" {
			return ParsedURL{}, fmt.Errorf("HTTP repository URL userinfo is not allowed")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ParsedURL{}, fmt.Errorf("repository URL host is required")
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") || (scheme == "ssh" && port == "22") || (scheme == "git" && port == "9418") {
		port = ""
	}
	segments, err := normalizedSegments(u.EscapedPath())
	if err != nil {
		return ParsedURL{}, err
	}
	last := segments[len(segments)-1]
	if !strings.HasSuffix(last, ".git") {
		segments[len(segments)-1] += ".git"
	}
	hostPart := encodeSegment(host)
	if port != "" {
		hostPart += "~" + port
	}
	key := strings.Join(append([]string{hostPart}, segments...), "/")
	return ParsedURL{Original: raw, Scheme: scheme, User: user, Host: host, Port: port, Path: strings.Join(segments, "/"), Key: key}, nil
}

func normalizedSegments(escaped string) ([]string, error) {
	escaped = strings.Trim(escaped, "/")
	if escaped == "" {
		return nil, fmt.Errorf("repository path is required")
	}
	parts := strings.Split(escaped, "/")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		decoded, err := url.PathUnescape(part)
		if err != nil || !utf8.ValidString(decoded) || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\") || strings.IndexFunc(decoded, func(r rune) bool { return r == 0 || r < 0x20 || r == 0x7f }) >= 0 {
			return nil, fmt.Errorf("invalid repository path segment")
		}
		result = append(result, encodeSegment(decoded))
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("repository path is required")
	}
	return result, nil
}

func encodeSegment(value string) string {
	var b strings.Builder
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", rune(c)) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// RepositoryKey matches git-server repository identity without exposing credentials.
// Keep normalization aligned with git-server/pkg/repository/url.go.
func RepositoryKey(raw string) (string, error) {
	parsed, err := parseRepositoryURL(raw)
	return parsed.Key, err
}
