package features

import "testing"

func TestDefaultMatchesV137(t *testing.T) {
	f := Default()
	// Beta or GA in v1.37: on by default.
	if !f.AdminAccess || !f.PrioritizedList || !f.PartitionableDevices || !f.DeviceTaints ||
		!f.ConsumableCapacity || !f.FractionalCapacityRange || !f.DeviceBindingAndStatus {
		t.Errorf("a v1.37 default-on gate is off: %+v", f)
	}
	// Alpha in v1.37: off by default.
	if f.ListTypeAttributes || f.OptionalNodeOperations || f.DerivedAttributes || f.CompatibilityGroups {
		t.Errorf("a v1.37 alpha gate is on: %+v", f)
	}
}

func TestParse(t *testing.T) {
	f, err := Parse("DRADerivedAttributes=true, DRADeviceTaints=false")
	if err != nil {
		t.Fatal(err)
	}
	if !f.DerivedAttributes || f.DeviceTaints {
		t.Errorf("overrides not applied: %+v", f)
	}
	for _, bad := range []string{"DRADeviceTaints", "DRADeviceTaints=maybe", "NoSuchGate=true"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", bad)
		}
	}
}
