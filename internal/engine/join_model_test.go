package engine

import (
	"context"
	"fmt"
	"math/bits"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

// This runtime truth table is the counterpart of docs/formal/JoinSafety.tla.
// A skipped branch has settled but did not cast a join vote. Exercise all
// three-branch guard outcomes and vary Go map/goroutine scheduling order.
func threeBranchJoinFixture(mode string, quorum int) string {
	modeLine := fmt.Sprintf("mode = %q", mode)
	if mode == "quorum" {
		modeLine += fmt.Sprintf("\n    quorum = %d", quorum)
	}
	var src strings.Builder
	src.WriteString(`pipeline "three" {
  entry = "start"
  node "start" { kind = "input" }
`)
	for _, branch := range []string{"a", "b", "c"} {
		fmt.Fprintf(&src, "  node %q {\n    kind = \"condition\"\n    test = \"input.%s\"\n  }\n", branch, branch)
	}
	fmt.Fprintf(&src, `  node "merge" {
    kind = "join"
    %s
    value = "literal:ready"
  }
  node "end" {
    kind = "output"
    value = "nodes.merge.value"
  }
`, modeLine)
	for _, branch := range []string{"a", "b", "c"} {
		fmt.Fprintf(&src, `  edge { from = "start" to = %q }
  edge { from = %q to = "merge" when = "true" }
`, branch, branch)
	}
	src.WriteString(`  edge { from = "merge" to = "end" }
}
surface "three" {
  path = "/mcp/three"
  tool "vote" {
    pipeline = "three"
    read_only = true
  }
}
`)
	return src.String()
}

func TestThreeBranchJoinThresholdRuntimeModel(t *testing.T) {
	modes := []struct {
		name      string
		mode      string
		quorum    int
		threshold int
	}{
		{"all", "all", 0, 3},
		{"default-all", "", 0, 3},
		{"any", "any", 0, 1},
		{"quorum-one", "quorum", 1, 1},
		{"quorum-two", "quorum", 2, 2},
		{"quorum-three", "quorum", 3, 3},
	}
	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Decode(threeBranchJoinFixture(tc.mode, tc.quorum))
			if err != nil {
				t.Fatal(err)
			}
			for mask := 0; mask < 8; mask++ {
				args := map[string]any{
					"a": mask&1 != 0,
					"b": mask&2 != 0,
					"c": mask&4 != 0,
				}
				expected := bits.OnesCount(uint(mask)) >= tc.threshold
				for attempt := 0; attempt < 5; attempt++ {
					out, runErr := New(nil).Run(context.Background(), "revision", cfg,
						"three", "vote", args, true)
					if expected {
						if runErr != nil || out.Status != "success" ||
							!out.OutputProduced || out.Value != "ready" {
							t.Fatalf("mode %s, mask %03b, attempt %d: expected joined result: %+v %v",
								tc.name, mask, attempt, out, runErr)
						}
					} else {
						if runErr == nil || !strings.Contains(runErr.Error(), "produced no output") ||
							out.Status != "failure" || out.OutputProduced || out.Value != nil || out.MCP != nil {
							t.Fatalf("mode %s, mask %03b, attempt %d: incomplete join published: %+v %v",
								tc.name, mask, attempt, out, runErr)
						}
					}
				}
			}
		})
	}
}
