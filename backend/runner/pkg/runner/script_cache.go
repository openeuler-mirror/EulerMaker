package runner

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ScriptSource reads the current global Script through the Gateway.
type ScriptSource interface {
	GetScript(context.Context, string) (*ScriptResource, error)
}

type cachedScript struct {
	uid             string
	resourceVersion string
	content         string
}

// ScriptCache reuses content only when the Job's observed UID and
// resourceVersion match the metadata of the cached Script response.
type ScriptCache struct {
	source  ScriptSource
	mu      sync.Mutex
	entries map[string]cachedScript
}

func NewScriptCache(source ScriptSource) *ScriptCache {
	return &ScriptCache{source: source, entries: make(map[string]cachedScript)}
}

func (c *ScriptCache) Resolve(ctx context.Context, ref ScriptRef) (string, error) {
	if ref.Name == "" || ref.UID == "" || ref.ResourceVersion == "" {
		return "", fmt.Errorf("Job scriptRef must contain name, UID, and resourceVersion")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c == nil || c.source == nil {
		return "", fmt.Errorf("Script source is not configured")
	}
	c.mu.Lock()
	entry, hit := c.entries[ref.Name]
	c.mu.Unlock()
	if hit && entry.uid == ref.UID && entry.resourceVersion == ref.ResourceVersion {
		return entry.content, nil
	}
	script, err := c.fetch(ctx, ref.Name)
	if err != nil {
		return "", fmt.Errorf("get Script %q: %w", ref.Name, err)
	}
	if err := validateFetchedScript(script, ref.Name); err != nil {
		return "", err
	}
	// The Job reference is an observation, not a version lock. A newer
	// response is executable, but an older reference will not hit this cache.
	c.mu.Lock()
	c.entries[ref.Name] = cachedScript{
		uid: script.Metadata.UID, resourceVersion: script.Metadata.ResourceVersion,
		content: script.Spec.Content,
	}
	c.mu.Unlock()
	return script.Spec.Content, nil
}

func (c *ScriptCache) fetch(ctx context.Context, name string) (*ScriptResource, error) {
	delay := time.Second
	for {
		script, err := c.source.GetScript(ctx, name)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil || !retryableScriptRead(err) {
			return script, err
		}
		wait := time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
		var status StatusError
		if errors.As(err, &status) && status.RetryAfter > wait {
			wait = status.RetryAfter
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func retryableScriptRead(err error) bool {
	var status StatusError
	if errors.As(err, &status) {
		return status.Code == 429 || status.Code >= 500
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func validateFetchedScript(script *ScriptResource, name string) error {
	if script == nil || script.APIVersion != "ebs/v1" || script.Kind != "Script" ||
		script.Metadata.Name != name || script.Metadata.Namespace != "" ||
		script.Metadata.UID == "" || script.Metadata.ResourceVersion == "" {
		return fmt.Errorf("invalid Script response for %q", name)
	}
	content := script.Spec.Content
	if content == "" || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return fmt.Errorf("invalid Script %q content", name)
	}
	line, _, _ := strings.Cut(content, "\n")
	interpreter := strings.Fields(strings.TrimPrefix(line, "#!"))
	if !strings.HasPrefix(line, "#!") || len(interpreter) == 0 ||
		!strings.HasPrefix(interpreter[0], "/") || interpreter[0] == "/" ||
		strings.ContainsRune(line, '\r') {
		return fmt.Errorf("Script %q must start with an absolute shebang", name)
	}
	return nil
}
