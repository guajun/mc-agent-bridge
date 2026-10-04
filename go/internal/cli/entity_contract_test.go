package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/guajun/mc-agent-bridge/internal/protocol"
)

func TestEntityCaptureRejectsRadiusBeforeContactingDaemon(t *testing.T) {
	for _, command := range []string{"entities", "snapshot"} {
		for _, radius := range []string{"0", "-1", "10"} {
			_, failure := (&app{}).cmdConvenience(context.Background(), command, []string{"--radius", radius})
			if failure == nil || failure.Code != protocol.CodeUsage || !strings.Contains(failure.Message, "removed") {
				t.Fatalf("%s radius %s: %v", command, radius, failure)
			}
		}
	}
}
