package validation

import (
	_ "crypto/sha256"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/api/resource"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
)

var architecturePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var resourcePackagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*(:[A-Za-z0-9][A-Za-z0-9+._-]*)*$`)

func ValidateConfig(obj *ebsv1.Config) field.ErrorList {
	var errs field.ErrorList
	name := field.NewPath("metadata", "name")
	if obj.Name == "" {
		errs = append(errs, field.Required(name, "name is required"))
	} else if messages := apivalidation.NameIsDNSSubdomain(obj.Name, false); len(messages) > 0 {
		errs = append(errs, field.Invalid(name, obj.Name, strings.Join(messages, "; ")))
	}
	if obj.Namespace != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "namespace"), "cluster-scoped resource"))
	}
	if obj.GenerateName != "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "generateName"), "not supported"))
	}
	visibility := field.NewPath("spec", "visibility")
	if obj.Spec.Visibility != ebsv1.ConfigVisibilityPublic && obj.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly {
		errs = append(errs, field.NotSupported(visibility, obj.Spec.Visibility, []string{string(ebsv1.ConfigVisibilityPublic), string(ebsv1.ConfigVisibilityOpsOnly)}))
	} else if obj.Name == ebsv1.ServiceConfigName && obj.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly {
		errs = append(errs, field.Forbidden(visibility, "service-config must be OpsOnly"))
	}
	content := field.NewPath("spec", "content")
	if obj.Spec.Content == "" {
		errs = append(errs, field.Required(content, "content is required"))
	} else if !utf8.ValidString(obj.Spec.Content) || strings.ContainsRune(obj.Spec.Content, 0) {
		errs = append(errs, field.Invalid(content, "<redacted>", "must be UTF-8 text without NUL"))
	} else if obj.Name == ebsv1.BuildResourceConfigName {
		var resources ebsv1.BuildResourceContent
		if err := yaml.UnmarshalStrict([]byte(obj.Spec.Content), &resources); err != nil {
			errs = append(errs, field.Invalid(content, "<redacted>", err.Error()))
		} else {
			errs = append(errs, validateBuildResourceContent(&resources)...)
		}
	} else if obj.Name == ebsv1.BuildTargetConfigName {
		var targets ebsv1.BuildTargetContent
		if err := yaml.UnmarshalStrict([]byte(obj.Spec.Content), &targets); err != nil {
			errs = append(errs, field.Invalid(content, "<redacted>", err.Error()))
		} else {
			errs = append(errs, validateBuildTargetContent(&targets)...)
		}
	} else if obj.Name == ebsv1.ServiceConfigName {
		var services ebsv1.ServiceConfigContent
		if err := yaml.UnmarshalStrict([]byte(obj.Spec.Content), &services); err != nil {
			errs = append(errs, field.Invalid(content, "<redacted>", err.Error()))
		} else {
			errs = append(errs, validateServiceConfigContent(&services)...)
		}
	}
	return errs
}

// validateBuildResourceContent validates resource rules and their effective values after inheritance.
func validateBuildResourceContent(obj *ebsv1.BuildResourceContent) field.ErrorList {
	path := field.NewPath("spec", "content")
	var errs field.ErrorList
	for _, key := range []string{"cpu", "memory"} {
		if obj.Default.Requests[key] == "" {
			errs = append(errs, field.Required(path.Child("default", "requests", key), "default request is required"))
		}
	}
	errs = append(errs, validateResourceLevel(obj.Default, path.Child("default"))...)
	errs = append(errs, validateEffectiveResourceLevels(path.Child("default"), obj.Default)...)
	for name, pkg := range obj.Packages {
		p := path.Child("packages").Key(name)
		if !resourcePackagePattern.MatchString(name) {
			errs = append(errs, field.Invalid(p, name, "invalid package name"))
		}
		if len(pkg.Default.Requests) == 0 && len(pkg.Default.Limits) == 0 && len(pkg.Arches) == 0 {
			errs = append(errs, field.Required(p, "at least one resource rule is required"))
		}
		errs = append(errs, validateResourceLevel(pkg.Default, p.Child("default"))...)
		errs = append(errs, validateEffectiveResourceLevels(p.Child("default"), obj.Default, pkg.Default)...)
		for arch, level := range pkg.Arches {
			a := p.Child("arches").Key(arch)
			if !architecturePattern.MatchString(arch) {
				errs = append(errs, field.Invalid(a, arch, "invalid architecture"))
			}
			errs = append(errs, validateResourceLevel(level, a)...)
			errs = append(errs, validateEffectiveResourceLevels(a, obj.Default, pkg.Default, level)...)
		}
	}
	return errs
}

func validateResourceLevel(level ebsv1.ResourceRequirements, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for name, values := range map[string]map[string]string{"requests": level.Requests, "limits": level.Limits} {
		for key, value := range values {
			p := path.Child(name).Key(key)
			if key != "cpu" && key != "memory" {
				errs = append(errs, field.NotSupported(p, key, []string{"cpu", "memory"}))
				continue
			}
			quantity, err := resource.ParseQuantity(value)
			if err != nil || quantity.Sign() <= 0 {
				errs = append(errs, field.Invalid(p, value, "must be a positive resource quantity"))
			}
		}
	}
	return errs
}

// Each level's missing limits inherit its own requests before overriding the less specific level.
func validateEffectiveResourceLevels(path *field.Path, levels ...ebsv1.ResourceRequirements) field.ErrorList {
	var errs field.ErrorList
	for _, key := range []string{"cpu", "memory"} {
		var request, limit string
		for _, level := range levels {
			if value, exists := level.Requests[key]; exists {
				request = value
			}
			value := level.Limits[key]
			if value == "" {
				value = level.Requests[key]
			}
			if value != "" {
				limit = value
			}
		}
		req, reqErr := resource.ParseQuantity(request)
		lim, limErr := resource.ParseQuantity(limit)
		if reqErr == nil && limErr == nil && lim.Cmp(req) < 0 {
			errs = append(errs, field.Invalid(path.Child("limits", key), limit, "effective limit must be at least the effective request"))
		}
	}
	return errs
}

// validateServiceConfigContent validates all instance names and service addresses.
func validateServiceConfigContent(obj *ebsv1.ServiceConfigContent) field.ErrorList {
	path := field.NewPath("spec", "content", "artifactManagers")
	var errs field.ErrorList
	gitPath := field.NewPath("spec", "content", "gitServer", "internalURL")
	if obj.GitServer.InternalURL == "" {
		errs = append(errs, field.Required(gitPath, "internalURL is required"))
	} else if !validServiceURL(obj.GitServer.InternalURL) {
		errs = append(errs, field.Invalid(gitPath, "<redacted>",
			"must be an absolute HTTP(S) URL without credentials, query, fragment or whitespace"))
	}
	if len(obj.ArtifactManagers) == 0 {
		errs = append(errs, field.Required(path, "at least one instance is required"))
	}
	for name, instance := range obj.ArtifactManagers {
		p := path.Key(name)
		if reasons := validation.IsDNS1123Label(name); len(reasons) != 0 {
			errs = append(errs, field.Invalid(p, name, strings.Join(reasons, "; ")))
		}
		if instance.InternalURL == "" {
			errs = append(errs, field.Required(p.Child("internalURL"), "internalURL is required"))
		}
		for _, address := range []struct {
			field, value string
		}{
			{"internalURL", instance.InternalURL},
			{"runnerInternalURL", instance.RunnerInternalURL},
			{"runnerExternalURL", instance.RunnerExternalURL},
		} {
			if address.value == "" {
				continue
			}
			if !validServiceURL(address.value) {
				detail := "must be an absolute HTTP(S) URL without credentials, query, fragment or whitespace"
				errs = append(errs, field.Invalid(p.Child(address.field), "<redacted>", detail))
			}
		}
	}
	return errs
}

func validServiceURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") ||
		strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return false
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	return true
}

func validateBuildTargetContent(obj *ebsv1.BuildTargetContent) field.ErrorList {
	var errs field.ErrorList
	targetsPath := field.NewPath("spec", "content", "targets")
	if obj.Targets == nil {
		errs = append(errs, field.Required(targetsPath, "an object is required; use {} for no targets"))
	}
	for os, target := range obj.Targets {
		path := targetsPath.Key(os)
		if os == "" || len(validation.IsValidLabelValue(os)) > 0 {
			errs = append(errs, field.Invalid(path, os, "must be a nonempty label value"))
		}
		if len(target.Arches) == 0 {
			errs = append(errs, field.Required(path.Child("arches"), "at least one architecture is required"))
		}
		for arch, config := range target.Arches {
			p := path.Child("arches").Key(arch)
			if !architecturePattern.MatchString(arch) {
				errs = append(errs, field.Invalid(p, arch, "invalid architecture"))
			}
			_, err := reference.ParseNormalizedNamed(config.Image)
			if err != nil || strings.ContainsAny(config.Image, " \t\r\n") {
				errs = append(errs, field.Invalid(p.Child("image"), config.Image, "invalid container image reference"))
			}
		}
	}
	return errs
}
