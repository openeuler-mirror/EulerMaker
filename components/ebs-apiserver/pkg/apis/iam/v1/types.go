package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              UserSpec `json:"spec,omitempty"`
}

type UserSpec struct {
	Enabled     *bool  `json:"enabled,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
}

type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

type MachineAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MachineAccountSpec `json:"spec,omitempty"`
}

type MachineAccountSpec struct {
	TokenTTLSeconds int64 `json:"tokenTTLSeconds,omitempty"`
}

type MachineAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineAccount `json:"items"`
}
