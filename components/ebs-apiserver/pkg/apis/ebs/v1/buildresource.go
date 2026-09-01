package v1

// ResolveBuildResources returns the effective resources for a package and
// architecture. Later levels override individual request and limit keys while
// preserving unspecified values from the table default.
func ResolveBuildResources(spec BuildResourceSpec, packageName, arch string) ResourceRequirements {
	resources := MergeResourceRequirements(ResourceRequirements{}, spec.Default)
	if config, ok := spec.Packages[packageName]; ok {
		resources = MergeResourceRequirements(resources, config.Default)
		if archResources, ok := config.Arches[arch]; ok {
			resources = MergeResourceRequirements(resources, archResources)
		}
	}
	return resources
}

// MergeResourceRequirements overlays individual request and limit keys.
func MergeResourceRequirements(base, override ResourceRequirements) ResourceRequirements {
	merged := ResourceRequirements{Requests: map[string]string{}, Limits: map[string]string{}}
	for name, value := range base.Requests {
		merged.Requests[name] = value
		if _, hasLimit := base.Limits[name]; !hasLimit {
			merged.Limits[name] = value
		}
	}
	for name, value := range base.Limits {
		merged.Limits[name] = value
	}
	for name, value := range override.Requests {
		merged.Requests[name] = value
		if _, hasLimit := override.Limits[name]; !hasLimit {
			merged.Limits[name] = value
		}
	}
	for name, value := range override.Limits {
		merged.Limits[name] = value
	}
	return merged
}
