package diagnose

import (
	"fmt"
	"regexp"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
)

// refPattern finds attribute and capacity references in a DRA CEL selector,
// e.g. device.attributes["gpu.example.com"].model or
// device.capacity['gpu.example.com'].memory.
var refPattern = regexp.MustCompile(`device\.(attributes|capacity)\[\s*["']([^"']+)["']\s*\]\.([A-Za-z_][A-Za-z0-9_]*)`)

// Referenced returns the device's actual values for the attributes and
// capacities an expression reads, like "model=LATEST-GPU-MODEL, memory=80Gi".
// This is the evidence that makes a selector mismatch obvious at a glance.
func Referenced(expr string, dev *resourceapi.Device, driver string) string {
	var parts []string
	seen := map[string]bool{}
	for _, m := range refPattern.FindAllStringSubmatch(expr, -1) {
		kind, domain, name := m[1], m[2], m[3]
		k := kind + domain + name
		if seen[k] {
			continue
		}
		seen[k] = true
		var val string
		if kind == "attributes" {
			val = attributeValue(dev.Attributes, domain, name, driver)
		} else {
			val = capacityValue(dev.Capacity, domain, name, driver)
		}
		parts = append(parts, name+"="+val)
	}
	if strings.Contains(expr, "device.driver") {
		parts = append(parts, "driver="+driver)
	}
	return strings.Join(parts, ", ")
}

func keys(domain, name, driver string) []resourceapi.QualifiedName {
	full := resourceapi.QualifiedName(domain + "/" + name)
	if domain == driver {
		return []resourceapi.QualifiedName{resourceapi.QualifiedName(name), full}
	}
	return []resourceapi.QualifiedName{full}
}

func attributeValue(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, domain, name, driver string) string {
	for _, k := range keys(domain, name, driver) {
		if a, ok := attrs[k]; ok {
			return FormatAttribute(a)
		}
	}
	return "<unset>"
}

func capacityValue(caps map[resourceapi.QualifiedName]resourceapi.DeviceCapacity, domain, name, driver string) string {
	for _, k := range keys(domain, name, driver) {
		if c, ok := caps[k]; ok {
			return c.Value.String()
		}
	}
	return "<unset>"
}

// FormatAttribute renders a DeviceAttribute's value.
func FormatAttribute(a resourceapi.DeviceAttribute) string {
	switch {
	case a.IntValue != nil:
		return fmt.Sprint(*a.IntValue)
	case a.BoolValue != nil:
		return fmt.Sprint(*a.BoolValue)
	case a.StringValue != nil:
		return *a.StringValue
	case a.VersionValue != nil:
		return *a.VersionValue
	}
	return "<unsupported type>"
}
