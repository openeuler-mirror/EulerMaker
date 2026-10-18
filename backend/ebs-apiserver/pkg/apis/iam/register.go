package iam

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	iamv1 "ebs-api/iam/v1"
)

const GroupName = "iam.ebs"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: runtime.APIVersionInternal}

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&iamv1.User{}, &iamv1.UserList{},
		&iamv1.MachineAccount{}, &iamv1.MachineAccountList{},
	)
	return nil
}
