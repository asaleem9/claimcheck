package diagnose

import (
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func TestReferenced(t *testing.T) {
	dev := &resourceapi.Device{
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"model":                    {StringValue: ptr.To("H100")},
			"index":                    {IntValue: ptr.To[int64](3)},
			"other.example.com/region": {StringValue: ptr.To("eu")},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {Value: resource.MustParse("80Gi")},
		},
	}
	tests := []struct{ expr, want string }{
		{`device.attributes["gpu.example.com"].model == "A100"`, "model=H100"},
		{`device.capacity['gpu.example.com'].memory.compareTo(quantity("120Gi")) >= 0`, "memory=80Gi"},
		{`device.attributes["gpu.example.com"].index > 4 && device.attributes["gpu.example.com"].index < 8`, "index=3"},
		{`device.attributes["other.example.com"].region == "us"`, "region=eu"},
		{`device.attributes["gpu.example.com"].arch == "hopper"`, "arch=<unset>"},
		{`device.driver == "tpu.example.com"`, "driver=gpu.example.com"},
		{`true`, ""},
	}
	for _, tt := range tests {
		if got := Referenced(tt.expr, dev, "gpu.example.com"); got != tt.want {
			t.Errorf("Referenced(%s) = %q, want %q", tt.expr, got, tt.want)
		}
	}
}

func TestNaturalLess(t *testing.T) {
	if !naturalLess("gpu-2", "gpu-10") || naturalLess("gpu-10", "gpu-2") || !naturalLess("a", "b") {
		t.Error("natural ordering broken")
	}
}

func TestShortCELError(t *testing.T) {
	in := "no such key: arch. consider using CEL optional chaining (.? followed by orValue())"
	if got := ShortCELError(in); got != "no such key: arch" {
		t.Errorf("got %q", got)
	}
}
