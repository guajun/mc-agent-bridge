package session

import "testing"

func TestLegacyEntityCaptureMapping(t *testing.T) {
	for _, test := range []struct {
		op     string
		params map[string]any
		want   string
	}{
		{"entities", nil, "ENTITIES"},
		{"entities", map[string]any{"dimension": "minecraft:the_nether"}, "ENTITIES minecraft:the_nether"},
		{"snapshot", map[string]any{"name": "capture", "dimension": "minecraft:the_end"}, "SNAPSHOT capture minecraft:the_end"},
		{"snapshot", map[string]any{"dimension": "minecraft:the_end"}, "SNAPSHOT minecraft:the_end"},
	} {
		got, failure := legacyLine(test.op, test.params)
		if failure != nil || got != test.want {
			t.Fatalf("%s: got %q, %v", test.op, got, failure)
		}
	}
	for _, op := range []string{"entities", "snapshot"} {
		if _, failure := legacyLine(op, map[string]any{"radius": 0}); failure == nil {
			t.Fatalf("%s silently accepted radius", op)
		}
	}
}
