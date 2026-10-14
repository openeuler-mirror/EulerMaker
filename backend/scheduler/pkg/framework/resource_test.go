package framework

import (
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestParseRequestsAndFits(t *testing.T) {
	r, err := ParseRequests(ebsv1.ResourceRequirements{Requests: map[string]string{"cpu": "1500m", "memory": "2Gi"}})
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := ParseAllocatable(map[string]string{"cpu": "2", "memory": "4Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if !capacity.Fits(r) {
		t.Fatal("request should fit")
	}
	if _, err := ParseAllocatable(map[string]string{"cpu": "-1"}); err == nil {
		t.Fatal("negative quantity must fail")
	}
}
