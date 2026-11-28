package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              UserSpec `json:"spec,omitempty"`
}

type UserSpec struct {
	Enabled     *bool    `json:"enabled,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Email       string   `json:"email,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MachineAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MachineAccountSpec `json:"spec,omitempty"`
}

type MachineAccountSpec struct {
	// Type is immutable. Omitted registration input defaults to runner.
	Type            MachineAccountType `json:"type,omitempty"`
	TokenTTLSeconds int64              `json:"tokenTTLSeconds,omitempty"`
	// Projects accepts exact names or a standalone * for all current and future projects. Only service accounts may specify it.
	Projects []string `json:"projects,omitempty"`
	// Permissions applies to every authorized project; either empty list grants no resource access.
	Permissions []MachineAccountPermission `json:"permissions,omitempty"`
}

const MachineAccountAllProjects = "*"

// +enum
type MachineAccountPermission string

const (
	MachinePermissionProjectsUpdate MachineAccountPermission = "projects:update"
	MachinePermissionBuildsCreate   MachineAccountPermission = "builds:create"
	MachinePermissionBuildsAbort    MachineAccountPermission = "builds:abort"
)

func (p MachineAccountPermission) IsValid() bool {
	return p == MachinePermissionProjectsUpdate || p == MachinePermissionBuildsCreate || p == MachinePermissionBuildsAbort
}

// +enum
type MachineAccountType string

const (
	MachineAccountRunner  MachineAccountType = "runner"
	MachineAccountService MachineAccountType = "service"
)

func (t MachineAccountType) IsValid() bool {
	return t == MachineAccountRunner || t == MachineAccountService
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MachineAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineAccount `json:"items"`
}
