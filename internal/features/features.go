// Package features maps DRA feature gate names to the allocator's feature
// set. Feature gates can't be read through the Kubernetes API, so claimcheck
// assumes the v1.37 defaults and lets users override them with --features.
package features

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/structured"
)

type gate struct {
	name string
	set  func(*structured.Features, bool)
	// enabled by default in Kubernetes v1.37 (beta or GA)
	def bool
}

var gates = []gate{
	{"DRAAdminAccess", func(f *structured.Features, v bool) { f.AdminAccess = v }, true},
	{"DRAPrioritizedList", func(f *structured.Features, v bool) { f.PrioritizedList = v }, true},
	{"DRAPartitionableDevices", func(f *structured.Features, v bool) { f.PartitionableDevices = v }, true},
	{"DRADeviceTaints", func(f *structured.Features, v bool) { f.DeviceTaints = v }, true},
	{"DRAConsumableCapacity", func(f *structured.Features, v bool) { f.ConsumableCapacity = v }, true},
	{"DRAFractionalCapacityRange", func(f *structured.Features, v bool) { f.FractionalCapacityRange = v }, true},
	{"DRADeviceBindingConditions", func(f *structured.Features, v bool) { f.DeviceBindingAndStatus = v }, true},
	{"DRAListTypeAttributes", func(f *structured.Features, v bool) { f.ListTypeAttributes = v }, false},
	{"DRAOptionalNodeOperations", func(f *structured.Features, v bool) { f.OptionalNodeOperations = v }, false},
	{"DRADerivedAttributes", func(f *structured.Features, v bool) { f.DerivedAttributes = v }, false},
	{"DRADeviceCompatibilityGroups", func(f *structured.Features, v bool) { f.CompatibilityGroups = v }, false},
}

// Default returns the feature set enabled by default in Kubernetes v1.37.
func Default() structured.Features {
	var f structured.Features
	for _, g := range gates {
		g.set(&f, g.def)
	}
	return f
}

// Parse applies a comma-separated list of overrides to the defaults, in the
// same "Name=bool" form kube-scheduler's --feature-gates flag uses.
func Parse(spec string) (structured.Features, error) {
	f := Default()
	if strings.TrimSpace(spec) == "" {
		return f, nil
	}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		name, val, ok := strings.Cut(item, "=")
		if !ok {
			return f, fmt.Errorf("feature %q: expected Name=true|false", item)
		}
		var enabled bool
		switch strings.ToLower(val) {
		case "true":
			enabled = true
		case "false":
		default:
			return f, fmt.Errorf("feature %q: value must be true or false", item)
		}
		found := false
		for _, g := range gates {
			if g.name == name {
				g.set(&f, enabled)
				found = true
			}
		}
		if !found {
			return f, fmt.Errorf("unknown feature %q (known: %s)", name, strings.Join(Names(), ", "))
		}
	}
	return f, nil
}

// Names lists the feature gates claimcheck understands.
func Names() []string {
	out := make([]string, 0, len(gates))
	for _, g := range gates {
		out = append(out, g.name)
	}
	sort.Strings(out)
	return out
}

// CEL returns the matching feature set for the CEL selector compiler.
func CEL(f structured.Features) cel.Features {
	return cel.Features{
		EnableConsumableCapacity: f.ConsumableCapacity,
		EnableListTypeAttributes: f.ListTypeAttributes,
	}
}
