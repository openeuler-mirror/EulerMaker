package v1

import runtime "k8s.io/apimachinery/pkg/runtime"

func (in *User) DeepCopyInto(out *User) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.Enabled != nil {
		out.Spec.Enabled = new(bool)
		*out.Spec.Enabled = *in.Spec.Enabled
	}
	if in.Spec.Scopes != nil {
		out.Spec.Scopes = append([]string(nil), in.Spec.Scopes...)
	}
}

func (in *User) DeepCopy() *User {
	if in == nil {
		return nil
	}
	out := new(User)
	in.DeepCopyInto(out)
	return out
}

func (in *User) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *UserList) DeepCopyInto(out *UserList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]User, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *UserList) DeepCopy() *UserList {
	if in == nil {
		return nil
	}
	out := new(UserList)
	in.DeepCopyInto(out)
	return out
}

func (in *UserList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MachineAccount) DeepCopyInto(out *MachineAccount) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
}
func (in *MachineAccount) DeepCopy() *MachineAccount {
	if in == nil {
		return nil
	}
	out := new(MachineAccount)
	in.DeepCopyInto(out)
	return out
}
func (in *MachineAccount) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
func (in *MachineAccountList) DeepCopyInto(out *MachineAccountList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MachineAccount, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *MachineAccountList) DeepCopy() *MachineAccountList {
	if in == nil {
		return nil
	}
	out := new(MachineAccountList)
	in.DeepCopyInto(out)
	return out
}
func (in *MachineAccountList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
