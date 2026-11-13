package diagnose

// The capacity arithmetic below is adapted from the DRA allocator in
// k8s.io/dynamic-resource-allocation/structured/internal/experimental
// (consumable_capacity.go, Apache License 2.0, Copyright The Kubernetes
// Authors). It lives in an internal package, so it can't be imported; it is
// ported here so explanations use exactly the same rounding and policy rules.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/structured"
)

var errNotRepresentable = errors.New("capacity request cannot be represented in the device's range arithmetic")

// capacityCheck mirrors CmpRequestOverCapacity. It returns ok, or a
// human-readable reason why the request doesn't fit, or an error the
// allocator would also fail with.
func capacityCheck(consumed map[resourceapi.QualifiedName]resource.Quantity, req *resourceapi.CapacityRequirements,
	capacity map[resourceapi.QualifiedName]resourceapi.DeviceCapacity, fractional bool) (ok bool, why string, err error) {
	if req != nil {
		for name := range req.Requests {
			if _, found := capacity[name]; !found {
				return false, fmt.Sprintf("requests capacity %q, which the device doesn't have", name), nil
			}
		}
	}
	use, err := consumption(req, capacity, fractional)
	if err != nil {
		return false, "", err
	}
	names := make([]string, 0, len(capacity))
	for n := range capacity {
		names = append(names, string(n))
	}
	sort.Strings(names)
	for _, n := range names {
		name := resourceapi.QualifiedName(n)
		c := capacity[name]
		u := use[name]
		if violatesPolicy(u, c.RequestPolicy, fractional) {
			return false, fmt.Sprintf("%s=%s is not allowed by the device's request policy%s", name, u.String(), describePolicy(c.RequestPolicy)), nil
		}
		total := u.DeepCopy()
		if prev, found := consumed[name]; found {
			total.Add(prev)
		}
		if total.Cmp(c.Value) > 0 {
			left := c.Value.DeepCopy()
			if prev, found := consumed[name]; found {
				left.Sub(prev)
			}
			return false, fmt.Sprintf("needs %s=%s, %s of %s left", name, u.String(), left.String(), c.Value.String()), nil
		}
	}
	return true, "", nil
}

// consumption computes what a request takes from each of a device's
// capacities, with defaults and rounding applied (GetConsumedCapacityFromRequest).
func consumption(req *resourceapi.CapacityRequirements, capacity map[resourceapi.QualifiedName]resourceapi.DeviceCapacity, fractional bool) (map[resourceapi.QualifiedName]resource.Quantity, error) {
	out := make(map[resourceapi.QualifiedName]resource.Quantity, len(capacity))
	for name, c := range capacity {
		var requested *resource.Quantity
		if req != nil && req.Requests != nil {
			if v, found := req.Requests[name]; found {
				requested = &v
			}
		}
		q, err := consumedValue(requested, c, fractional)
		if err != nil {
			return nil, fmt.Errorf("capacity %q: %w", name, err)
		}
		if q.Sign() < 0 {
			return nil, fmt.Errorf("capacity %q: capacity value is negative", name)
		}
		out[name] = q
	}
	return out, nil
}

func consumedValue(requested *resource.Quantity, c resourceapi.DeviceCapacity, fractional bool) (resource.Quantity, error) {
	if requested == nil {
		if c.RequestPolicy != nil && c.RequestPolicy.Default != nil {
			return c.RequestPolicy.Default.DeepCopy(), nil
		}
		return c.Value.DeepCopy(), nil
	}
	if c.RequestPolicy == nil {
		return requested.DeepCopy(), nil
	}
	switch {
	case c.RequestPolicy.ValidRange != nil && c.RequestPolicy.ValidRange.Min != nil:
		return roundUpRange(requested, c.RequestPolicy.ValidRange, fractional)
	case c.RequestPolicy.ValidValues != nil:
		for _, v := range c.RequestPolicy.ValidValues {
			if requested.Cmp(v) <= 0 {
				return v.DeepCopy(), nil
			}
		}
	}
	return *requested, nil
}

func roundUpRange(requested *resource.Quantity, r *resourceapi.CapacityRequestPolicyRange, fractional bool) (resource.Quantity, error) {
	if requested.Cmp(*r.Min) < 0 {
		return r.Min.DeepCopy(), nil
	}
	if r.Step == nil {
		return *requested, nil
	}
	if useMilli(r, fractional) {
		req, err := milli(*requested)
		if err != nil {
			return resource.Quantity{}, fmt.Errorf("%w: request %s is not a representable milli value", errNotRepresentable, requested.String())
		}
		step, minV := r.Step.MilliValue(), r.Min.MilliValue()
		n := (req - minV) / step
		if (req-minV)%step != 0 {
			n++
		}
		if n > (math.MaxInt64-minV)/step {
			return resource.Quantity{}, fmt.Errorf("%w: rounding request %s up passes the milli-value range", errNotRepresentable, requested.String())
		}
		v := minV + step*n
		if v%1000 == 0 {
			return *resource.NewQuantity(v/1000, r.Step.Format), nil
		}
		return *resource.NewMilliQuantity(v, r.Step.Format), nil
	}
	if requested.CmpInt64(math.MaxInt64) > 0 {
		return resource.Quantity{}, fmt.Errorf("%w: request %s exceeds MaxInt64", errNotRepresentable, requested.String())
	}
	req, step, minV := requested.Value(), r.Step.Value(), r.Min.Value()
	n := (req - minV) / step
	if (req-minV)%step != 0 {
		n++
	}
	if minV >= 0 && n > (math.MaxInt64-minV)/step {
		return resource.Quantity{}, fmt.Errorf("%w: rounding request %s up passes MaxInt64", errNotRepresentable, requested.String())
	}
	return *resource.NewQuantity(minV+step*n, r.Step.Format), nil
}

func violatesPolicy(v resource.Quantity, p *resourceapi.CapacityRequestPolicy, fractional bool) bool {
	if p == nil {
		return false
	}
	if p.Default != nil && v == *p.Default {
		return false
	}
	switch {
	case p.ValidRange != nil:
		r := *p.ValidRange
		if r.Max != nil && v.Cmp(*r.Max) > 0 {
			return true
		}
		if r.Step != nil {
			var req, step, minV int64
			if useMilli(&r, fractional) {
				var err error
				if req, err = milli(v); err != nil {
					return true
				}
				step, minV = r.Step.MilliValue(), r.Min.MilliValue()
			} else {
				req, step, minV = v.Value(), r.Step.Value(), r.Min.Value()
			}
			if (req-minV)%step != 0 {
				return true
			}
		}
	case len(p.ValidValues) > 0:
		for _, ok := range p.ValidValues {
			if v.Cmp(ok) == 0 {
				return false
			}
		}
		return true
	}
	return false
}

func useMilli(r *resourceapi.CapacityRequestPolicyRange, fractional bool) bool {
	if !fractional {
		return false
	}
	for i, q := range []*resource.Quantity{r.Min, r.Max, r.Step} {
		if q == nil {
			continue
		}
		if _, err := milli(*q); err != nil {
			return false
		}
		if i == 2 {
			if s, err := milli(*r.Step); err == nil && s < 1 {
				return false
			}
		}
	}
	return true
}

func milli(q resource.Quantity) (int64, error) {
	m := q.MilliValue()
	if q.Cmp(*resource.NewMilliQuantity(m, q.Format)) != 0 {
		return 0, errors.New("value must be representable in milli units")
	}
	return m, nil
}

func describePolicy(p *resourceapi.CapacityRequestPolicy) string {
	if p == nil {
		return ""
	}
	switch {
	case p.ValidRange != nil:
		var parts []string
		if p.ValidRange.Min != nil {
			parts = append(parts, "min "+p.ValidRange.Min.String())
		}
		if p.ValidRange.Max != nil {
			parts = append(parts, "max "+p.ValidRange.Max.String())
		}
		if p.ValidRange.Step != nil {
			parts = append(parts, "step "+p.ValidRange.Step.String())
		}
		return " (" + strings.Join(parts, ", ") + ")"
	case len(p.ValidValues) > 0:
		var vs []string
		for _, v := range p.ValidValues {
			vs = append(vs, v.String())
		}
		return " (valid values " + strings.Join(vs, ", ") + ")"
	}
	return ""
}

// consumedSoFar returns what existing shared allocations already take from
// a device's capacities.
func (d *Diagnoser) consumedSoFar(id structured.DeviceID) map[resourceapi.QualifiedName]resource.Quantity {
	out := map[resourceapi.QualifiedName]resource.Quantity{}
	for name, q := range d.Allocated.AggregatedCapacity[id] {
		if q != nil {
			out[name] = q.DeepCopy()
		}
	}
	return out
}
