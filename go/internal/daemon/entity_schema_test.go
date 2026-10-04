package daemon

import (
	"strings"
	"testing"
)

func TestEntityCaptureSchemaUsesExplicitDimension(t *testing.T) {
	for _, op := range []string{"entities", "snapshot"} {
		schema, failure := schemaFor(op)
		if failure != nil {
			t.Fatal(failure)
		}
		parameters := schema["parameters"].(map[string]any)
		if _, exists := parameters["radius"]; exists {
			t.Fatalf("%s still advertises radius", op)
		}
		dimension, ok := parameters["dimension"].(map[string]any)
		if !ok || !strings.Contains(dimension["description"].(string), "minecraft:overworld") {
			t.Fatalf("%s dimension: %v", op, dimension)
		}
	}
}
