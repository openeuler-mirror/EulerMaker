package buildinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"controller-manager/pkg/artifacturl"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

func newReleaseClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("release probe redirect rejected")
			}
			return nil
		},
	}
}

// singleContentURL probes the stable release once per dispatch round. Jobs retain the logical instance reference.
func (c *Controller) singleContentURL(ctx context.Context, round *reconcileRound) (string, error) {
	if round.singleRepoChecked {
		return round.singleRepoURL, nil
	}
	instance := "default"
	if round.project != nil && round.project.Annotations[ebsv1.ProjectArtifactsAnnotation] != "" {
		instance = round.project.Annotations[ebsv1.ProjectArtifactsAnnotation]
	}
	target := round.build.Spec.BuildTarget
	path := fmt.Sprintf("/repositories/%s/%s/%s/", url.PathEscape(round.current.Namespace),
		url.PathEscape(target.Os), url.PathEscape(target.Arch))
	reference, err := artifacturl.ReferenceForInstance(instance, path)
	if err != nil {
		return "", controller.NewPermanentError(err)
	}
	contentURL, err := c.resolveContentURL(ctx, round, reference)
	if err != nil {
		if errors.Is(err, ErrNotFound) || deterministicRpmRepoError(err) {
			return "", controller.NewPermanentError(err)
		}
		return "", err
	}
	published, err := c.checkReleasePublished(ctx, contentURL)
	if err != nil {
		return "", err
	}
	if !published {
		reference, contentURL = "", ""
		if len(bootstrapRepoURLs(round.current.Spec.BootstrapRepo, target.Arch)) == 0 {
			return "", controller.NewPermanentError(fmt.Errorf("BootstrapRepoRequired: no published release or bootstrap repository"))
		}
	}
	round.singleRepoReference = reference
	round.singleRepoURL = contentURL
	round.singleRepoChecked = true
	return contentURL, nil
}

// checkReleasePublished distinguishes an unpublished target from unavailable or damaged release content.
func (c *Controller) checkReleasePublished(ctx context.Context, baseURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/repodata/repomd.xml", nil)
	if err != nil {
		return false, controller.NewPermanentError(err)
	}
	req.Header.Set("User-Agent", "eulermaker-controller-manager/1.0")
	resp, err := c.releaseClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("probe published repository: %w", err)
	}
	defer resp.Body.Close()
	limit := int64(64 << 10)
	if resp.StatusCode == http.StatusOK {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return false, fmt.Errorf("read release probe: %w", err)
	}
	if int64(len(body)) > limit {
		return false, fmt.Errorf("release probe response exceeds %d bytes", limit)
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Code string `json:"code"`
		}
		decoded := json.Unmarshal(body, &failure) == nil
		if resp.StatusCode == http.StatusNotFound && decoded && failure.Code == "ReleaseNotPublished" {
			return false, nil
		}
		err := fmt.Errorf("release probe returned HTTP %d (%s)", resp.StatusCode, failure.Code)
		if deterministicHTTPStatus(resp.StatusCode) {
			return false, controller.NewPermanentError(err)
		}
		return false, err
	}
	var metadata struct {
		XMLName xml.Name `xml:"repomd"`
		Data    []struct {
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&metadata); err != nil {
		return false, fmt.Errorf("invalid release repomd.xml: %w", err)
	}
	// Reject a second document or non-whitespace content after the root.
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("invalid release repomd.xml: %w", err)
		}
		switch value := token.(type) {
		case xml.Comment, xml.ProcInst:
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return false, fmt.Errorf("invalid trailing release metadata")
			}
		default:
			return false, fmt.Errorf("invalid trailing release metadata")
		}
	}
	for _, data := range metadata.Data {
		if strings.TrimSpace(data.Location.Href) != "" {
			return true, nil
		}
	}
	return false, fmt.Errorf("release repomd.xml has no data location")
}
