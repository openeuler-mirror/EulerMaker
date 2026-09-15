package v1

import (
	"encoding/json"
	"testing"
)

func TestSnapshotDefaultRefRoundTripAndDeepCopy(t *testing.T) {
	for _, ref := range []GitRef{{}, {Type: GitRefBranch, Value: "main"}, {Type: GitRefTag, Value: "v1"}} {
		snapshot := &Snapshot{Spec: SnapshotSpec{DefaultRef: ref}}
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Snapshot
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Spec.DefaultRef != ref {
			t.Fatalf("defaultRef=%+v, want %+v", decoded.Spec.DefaultRef, ref)
		}
		copy := snapshot.DeepCopy()
		if copy.Spec.DefaultRef != ref {
			t.Fatal("DeepCopy lost defaultRef")
		}
		copy.Spec.DefaultRef.Value = "changed"
		if snapshot.Spec.DefaultRef != ref {
			t.Fatal("copy modified original")
		}
	}
}
