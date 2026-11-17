package config

import (
	"fmt"
	"math/bits"
	"strings"
	"testing"
)

func TestGuaranteedPredecessorsAllAnyQuorumExhaustive(t *testing.T) {
	// For every choice of source-bearing paths and every join threshold,
	// compare the fixed-point calculation to all possible winning subsets.
	// This detects false positive guarantees that could release a protected
	// consumer without its declared input being complete.
	for n := 1; n <= 5; n++ {
		for sourceMask := 1; sourceMask < 1<<n; sourceMask++ {
			p := Pipeline{
				Name: "model", Entry: "start",
				Nodes: map[string]Node{
					"start": {Name: "start", Kind: "input"},
					"shared": {Name: "shared", Kind: "transform"},
				},
				Edges: []Edge{{From: "start", To: "shared"}},
			}
			order := []string{"start", "shared"}
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("branch_%d", i)
				p.Nodes[id] = Node{Name: id, Kind: "transform"}
				from := "start"
				if sourceMask&(1<<i) != 0 { from = "shared" }
				p.Edges = append(p.Edges, Edge{From: from, To: id}, Edge{From: id, To: "merge"})
				order = append(order, id)
			}
			order = append(order, "merge")
			for threshold := 1; threshold <= n; threshold++ {
				p.Nodes["merge"] = Node{
					Name: "merge", Kind: "join",
					Attrs: map[string]any{"mode": "quorum", "quorum": int64(threshold)},
				}
				got := guaranteedCompleted(p, order)["merge"]["shared"]
				want := true
				for winning := 1; winning < 1<<n; winning++ {
					if bits.OnesCount(uint(winning)) >= threshold &&
						winning&sourceMask == 0 {
						want = false
						break
					}
				}
				if got != want {
					t.Fatalf("n=%d sourceMask=%b threshold=%d got=%t want=%t",
						n, sourceMask, threshold, got, want)
				}
			}
		}
	}
}

func guardedJoinFixture(mode string, quorum int, expression string) string {
	joinAttr := fmt.Sprintf("mode = %q", mode)
	if mode == "quorum" { joinAttr += fmt.Sprintf("\n    quorum = %d", quorum) }
	input := `    input_ports = {
      left = {type = "string"}
      right = {type = "string"}
    }`
	dataEdges := `  edge {
    kind = "data"
    from = "left"
    to = "merge"
    to_port = "left"
  }
  edge {
    kind = "data"
    from = "right"
    to = "merge"
    to_port = "right"
  }`
	if !strings.Contains(expression,"ports.") {
		input = ""
		dataEdges = ""
	}
	return fmt.Sprintf(`pipeline "two" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "left" {
    kind = "transform"
    value = "literal:L"
  }
  node "right" {
    kind = "transform"
    value = "literal:R"
  }
  node "merge" {
    kind = "join"
    %s
%s
    value = %q
  }
  node "done" {
    kind = "output"
    value = "nodes.merge.value"
  }
  edge {
    from = "start"
    to = "left"
  }
  edge {
    from = "start"
    to = "right"
  }
  edge {
    from = "left"
    to = "merge"
  }
  edge {
    from = "right"
    to = "merge"
  }
  edge {
    from = "merge"
    to = "done"
  }
%s
}
surface "two" {
  path = "/mcp/two"
  tool "combine" {
    pipeline = "two"
    read_only = true
  }
}
`, joinAttr, input, expression, dataEdges)
}

func TestJoinGuaranteesForPortsAndNodeReferences(t *testing.T) {
	for _, tc := range []struct{
		name, mode string
		quorum int
		accept bool
	}{
		{"all", "all", 0, true},
		{"quorum-two", "quorum", 2, true},
		{"any", "any", 0, false},
		{"quorum-one", "quorum", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, expression := range []string{
				"ports.left + ports.right",
				"nodes.left.value + nodes.right.value",
			} {
				_, err := Decode(guardedJoinFixture(tc.mode, tc.quorum, expression))
				if tc.accept && err != nil {
					t.Fatalf("guaranteed source blocked (%s): %v", expression, err)
				}
				if !tc.accept && (err == nil || !strings.Contains(err.Error(), "not guaranteed")) {
					t.Fatalf("optional data admitted (%s): %v", expression, err)
				}
			}
		})
	}
}

func TestOrdinaryMergeCannotReadOnlyOneOptionalParent(t *testing.T) {
	source := guardedJoinFixture("any", 0, "nodes.left.value")
	// An ordinary transform merge also activates on a single fired input.
	source = strings.Replace(source, `kind = "join"
    mode = "any"`, `kind = "transform"`, 1)
	if _, err := Decode(source); err == nil || !strings.Contains(err.Error(), "not guaranteed") {
		t.Fatalf("unsafe optional dependency admitted: %v", err)
	}
}
