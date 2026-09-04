package framework

import (
	"fmt"

	ebsv1 "ebs-api/ebs/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Resource struct {
	CPU    resource.Quantity
	Memory resource.Quantity
}

func ParseRequests(requirements ebsv1.ResourceRequirements) (Resource, error) {
	return parseResource(requirements.Requests)
}

func ParseAllocatable(values map[string]string) (Resource, error) {
	return parseResource(values)
}

func parseResource(values map[string]string) (Resource, error) {
	result := Resource{CPU: *resource.NewQuantity(0, resource.DecimalSI), Memory: *resource.NewQuantity(0, resource.BinarySI)}
	for name, target := range map[string]*resource.Quantity{"cpu": &result.CPU, "memory": &result.Memory} {
		value, found := values[name]
		if !found || value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(value)
		if err != nil {
			return Resource{}, fmt.Errorf("parse %s quantity %q: %w", name, value, err)
		}
		if quantity.Sign() < 0 {
			return Resource{}, fmt.Errorf("%s quantity must not be negative", name)
		}
		*target = quantity
	}
	return result, nil
}

func (r Resource) Add(other Resource) Resource {
	result := r.DeepCopy()
	result.CPU.Add(other.CPU)
	result.Memory.Add(other.Memory)
	return result
}

func (r Resource) Sub(other Resource) Resource {
	result := r.DeepCopy()
	result.CPU.Sub(other.CPU)
	result.Memory.Sub(other.Memory)
	return result
}

func (r Resource) Fits(request Resource) bool {
	return r.CPU.Cmp(request.CPU) >= 0 && r.Memory.Cmp(request.Memory) >= 0
}

func (r Resource) ClampZero() Resource {
	result := r.DeepCopy()
	if result.CPU.Sign() < 0 {
		result.CPU = *resource.NewQuantity(0, resource.DecimalSI)
	}
	if result.Memory.Sign() < 0 {
		result.Memory = *resource.NewQuantity(0, resource.BinarySI)
	}
	return result
}

func (r Resource) DeepCopy() Resource {
	return Resource{CPU: r.CPU.DeepCopy(), Memory: r.Memory.DeepCopy()}
}

func (r Resource) Equal(other Resource) bool {
	return r.CPU.Cmp(other.CPU) == 0 && r.Memory.Cmp(other.Memory) == 0
}
