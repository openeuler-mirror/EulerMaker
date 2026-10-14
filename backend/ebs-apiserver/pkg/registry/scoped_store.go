package registry

import (
	"k8s.io/apiserver/pkg/registry/generic/registry"
)

type ScopedStore struct {
	*registry.Store
}

func (s *ScopedStore) NamespaceScoped() bool {
	return true
}