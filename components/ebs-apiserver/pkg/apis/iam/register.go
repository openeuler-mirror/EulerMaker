package iam

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
)

const GroupName = "iam.ebs"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1"}

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &iamv1.User{}, &iamv1.UserList{})
	scheme.AddKnownTypes(schema.GroupVersion{Group: GroupName, Version: runtime.APIVersionInternal}, &iamv1.User{}, &iamv1.UserList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
