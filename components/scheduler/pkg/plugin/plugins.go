package plugin

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	ebsv1 "ebs-api/ebs/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"scheduler/pkg/framework"
)

type filter struct {
	name string
	fn   func(*framework.Session, *framework.RunnerSnapshot) *framework.Status
}

func (f filter) Name() string { return f.name }
func (f filter) Filter(_ context.Context, s *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
	return f.fn(s, r)
}
func ok(name string) *framework.Status { return framework.SuccessStatus(name) }
func reject(name, reason string) *framework.Status {
	return &framework.Status{Code: framework.Unschedulable, Plugin: name, Reason: reason}
}
func fail(name string, err error) *framework.Status {
	return &framework.Status{Code: framework.Error, Plugin: name, Reason: err.Error(), Err: err}
}

func Phase() framework.FilterPlugin {
	return filter{"PhaseFilter", func(_ *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
		if r.Invalid != nil {
			return fail("PhaseFilter", r.Invalid)
		}
		if r.Runner.Status.Phase != "Idle" && r.Runner.Status.Phase != "Running" {
			return reject("PhaseFilter", "runner phase is not schedulable")
		}
		return ok("PhaseFilter")
	}}
}
func Unschedulable() framework.FilterPlugin {
	return filter{"UnschedulableFilter", func(_ *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
		if r.Runner.Spec.Unschedulable {
			return reject("UnschedulableFilter", "runner is marked unschedulable")
		}
		return ok("UnschedulableFilter")
	}}
}
func Runtime() framework.FilterPlugin {
	return filter{"RuntimeFilter", func(s *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
		runtime := s.Job.Spec.Runtime
		if runtime == "" {
			runtime = "ct"
		}
		runnerRuntime := r.Runner.Spec.Type
		if runnerRuntime == "" {
			runnerRuntime = "ct"
		}
		if runtime != runnerRuntime {
			return reject("RuntimeFilter", "runtime does not match")
		}
		return ok("RuntimeFilter")
	}}
}
func NodeSelector() framework.FilterPlugin {
	return filter{"NodeSelectorFilter", func(s *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
		keys := make([]string, 0, len(s.Job.Spec.NodeSelector))
		for k := range s.Job.Spec.NodeSelector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if r.Runner.Labels[k] != s.Job.Spec.NodeSelector[k] {
				return reject("NodeSelectorFilter", "node selector does not match")
			}
		}
		return ok("NodeSelectorFilter")
	}}
}

func validateTolerations(items []ebsv1.Toleration) error {
	for _, t := range items {
		op := t.Operator
		if op == "" {
			op = "Equal"
		}
		if op != "Equal" && op != "Exists" {
			return fmt.Errorf("invalid toleration operator %q", t.Operator)
		}
		if t.Effect != "" && t.Effect != "NoSchedule" && t.Effect != "NoExecute" && t.Effect != "PreferNoSchedule" {
			return fmt.Errorf("invalid toleration effect %q", t.Effect)
		}
	}
	return nil
}
func tolerated(t ebsv1.RunnerTaint, items []ebsv1.Toleration) bool {
	for _, x := range items {
		op := x.Operator
		if op == "" {
			op = "Equal"
		}
		if x.Effect != "" && x.Effect != t.Effect {
			continue
		}
		if op == "Exists" && (x.Key == "" || x.Key == t.Key) {
			return true
		}
		if op == "Equal" && x.Key == t.Key && x.Value == t.Value {
			return true
		}
	}
	return false
}
func Taint() framework.FilterPlugin {
	return filter{"TaintFilter", func(s *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
		if err := validateTolerations(s.Job.Spec.Tolerations); err != nil {
			return fail("TaintFilter", err)
		}
		for _, t := range r.Runner.Spec.Taints {
			if (t.Effect == "NoSchedule" || t.Effect == "NoExecute") && !tolerated(t, s.Job.Spec.Tolerations) {
				return reject("TaintFilter", "runner taint is not tolerated")
			}
		}
		return ok("TaintFilter")
	}}
}
func Capacity() framework.FilterPlugin {
	return filter{"CapacityFilter", func(s *framework.Session, r *framework.RunnerSnapshot) *framework.Status {
		if r.Invalid != nil {
			return fail("CapacityFilter", r.Invalid)
		}
		if !r.Available.Fits(s.Requests) {
			return reject("CapacityFilter", "insufficient resources")
		}
		return ok("CapacityFilter")
	}}
}

type scorer struct {
	name   string
	weight int64
	fn     func(*framework.Session, *framework.RunnerSnapshot) (int64, error)
}

func (s scorer) Name() string  { return s.name }
func (s scorer) Weight() int64 { return s.weight }
func (s scorer) Score(_ context.Context, se *framework.Session, r *framework.RunnerSnapshot) (int64, *framework.Status) {
	v, err := s.fn(se, r)
	if err != nil {
		return 0, fail(s.name, err)
	}
	return clamp(v), ok(s.name)
}
func clamp(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
func quantityScore(remaining, allocatable resource.Quantity) (int64, bool) {
	if allocatable.Sign() <= 0 {
		return 0, false
	}
	a := allocatable.AsDec()
	r := remaining.AsDec()
	scale := a.Scale()
	if r.Scale() > scale {
		scale = r.Scale()
	}
	ai := new(big.Int).Set(a.UnscaledBig())
	ri := new(big.Int).Set(r.UnscaledBig())
	ten := big.NewInt(10)
	if d := scale - a.Scale(); d > 0 {
		ai.Mul(ai, new(big.Int).Exp(ten, big.NewInt(int64(d)), nil))
	}
	if d := scale - r.Scale(); d > 0 {
		ri.Mul(ri, new(big.Int).Exp(ten, big.NewInt(int64(d)), nil))
	}
	if ri.Sign() < 0 {
		return 0, true
	}
	ri.Mul(ri, big.NewInt(100))
	ri.Quo(ri, ai)
	if !ri.IsInt64() {
		return 100, true
	}
	return clamp(ri.Int64()), true
}
func remaining(available, requested resource.Quantity) resource.Quantity {
	result := available.DeepCopy()
	result.Sub(requested)
	return result
}
func LeastAllocated() framework.ScorePlugin {
	return scorer{"LeastAllocated", 60, func(s *framework.Session, r *framework.RunnerSnapshot) (int64, error) {
		scores := []int64{}
		if v, yes := quantityScore(remaining(r.Available.CPU, s.Requests.CPU), r.Allocatable.CPU); yes {
			scores = append(scores, v)
		}
		if v, yes := quantityScore(remaining(r.Available.Memory, s.Requests.Memory), r.Allocatable.Memory); yes {
			scores = append(scores, v)
		}
		if len(scores) == 0 {
			return 0, nil
		}
		sum := int64(0)
		for _, v := range scores {
			sum += v
		}
		return sum / int64(len(scores)), nil
	}}
}
func BalancedJobs() framework.ScorePlugin {
	return scorer{"BalancedJobs", 40, func(_ *framework.Session, r *framework.RunnerSnapshot) (int64, error) {
		return 100 / (1 + r.RunningJobCount + r.AssumedJobCount), nil
	}}
}
func TaintPreference() framework.ScorePlugin {
	return scorer{"TaintPreference", 10, func(s *framework.Session, r *framework.RunnerSnapshot) (int64, error) {
		if err := validateTolerations(s.Job.Spec.Tolerations); err != nil {
			return 0, err
		}
		score := int64(100)
		for _, t := range r.Runner.Spec.Taints {
			if t.Effect == "PreferNoSchedule" && !tolerated(t, s.Job.Spec.Tolerations) {
				score -= 20
			}
		}
		return clamp(score), nil
	}}
}
func DefaultFilters() []framework.FilterPlugin {
	return []framework.FilterPlugin{Phase(), Unschedulable(), Runtime(), NodeSelector(), Taint(), Capacity()}
}
func DefaultScores() []framework.ScorePlugin {
	return []framework.ScorePlugin{LeastAllocated(), BalancedJobs(), TaintPreference()}
}
