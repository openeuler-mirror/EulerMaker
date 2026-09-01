package openapi

import (
	common "k8s.io/kube-openapi/pkg/common"
	spec "k8s.io/kube-openapi/pkg/validation/spec"
)

func schema_pkg_apis_ebs_v1_BuildResource(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"kind":       stringSchema(),
				"apiVersion": stringSchema(),
				"metadata":   referenceSchema(ref("k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta")),
				"spec":       referenceSchema(ref("ebs-apiserver/pkg/apis/ebs/v1.BuildResourceSpec")),
			},
		}},
		Dependencies: []string{
			"ebs-apiserver/pkg/apis/ebs/v1.BuildResourceSpec",
			"k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta",
		},
	}
}

func schema_pkg_apis_ebs_v1_BuildResourceList(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"kind":       stringSchema(),
				"apiVersion": stringSchema(),
				"metadata":   referenceSchema(ref("k8s.io/apimachinery/pkg/apis/meta/v1.ListMeta")),
				"items": {
					SchemaProps: spec.SchemaProps{Type: []string{"array"}, Items: &spec.SchemaOrArray{
						Schema: &spec.Schema{SchemaProps: referenceSchema(ref("ebs-apiserver/pkg/apis/ebs/v1.BuildResource")).SchemaProps},
					}},
				},
			},
			Required: []string{"items"},
		}},
		Dependencies: []string{
			"ebs-apiserver/pkg/apis/ebs/v1.BuildResource",
			"k8s.io/apimachinery/pkg/apis/meta/v1.ListMeta",
		},
	}
}

func schema_pkg_apis_ebs_v1_BuildResourceSpec(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"default":  referenceSchema(ref("ebs-apiserver/pkg/apis/ebs/v1.ResourceRequirements")),
				"packages": mapReferenceSchema(ref("ebs-apiserver/pkg/apis/ebs/v1.PackageResourceConfig")),
			},
			Required: []string{"packages"},
		}},
		Dependencies: []string{
			"ebs-apiserver/pkg/apis/ebs/v1.PackageResourceConfig",
			"ebs-apiserver/pkg/apis/ebs/v1.ResourceRequirements",
		},
	}
}

func schema_pkg_apis_ebs_v1_PackageResourceConfig(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"default": referenceSchema(ref("ebs-apiserver/pkg/apis/ebs/v1.ResourceRequirements")),
				"arches":  mapReferenceSchema(ref("ebs-apiserver/pkg/apis/ebs/v1.ResourceRequirements")),
			},
		}},
		Dependencies: []string{"ebs-apiserver/pkg/apis/ebs/v1.ResourceRequirements"},
	}
}

func stringSchema() spec.Schema {
	return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: ""}}
}

func referenceSchema(ref spec.Ref) spec.Schema {
	return spec.Schema{SchemaProps: spec.SchemaProps{Default: map[string]interface{}{}, Ref: ref}}
}

func mapReferenceSchema(ref spec.Ref) spec.Schema {
	return spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		AdditionalProperties: &spec.SchemaOrBool{Allows: true, Schema: &spec.Schema{
			SchemaProps: referenceSchema(ref).SchemaProps,
		}},
	}}
}
