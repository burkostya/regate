package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

func joinFixture(mode string, quorum int) string {
	attr := fmt.Sprintf("mode = %q",mode)
	if mode=="quorum"{attr+=fmt.Sprintf("\n    quorum = %d",quorum)}
	return fmt.Sprintf(`
pipeline "votes" {
	entry = "start"
	node "start" {
		kind = "input"
	}
	node "left" {
		kind = "condition"
		test = "input.left"
	}
	node "right" {
		kind = "condition"
		test = "input.right"
	}
	node "merge" {
		kind = "join"
		%s
		value = "literal:approved"
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
		when = "true"
	}
	edge {
		from = "right"
		to = "merge"
		when = "true"
	}
	edge {
		from = "merge"
		to = "done"
	}
}
surface "voting" {
	path = "/mcp/voting"
	tool "vote" {
		pipeline = "votes"
		read_only = true
	}
}
`, attr)
}

func TestJoinThresholdRequiresDistinctFiredBranches(t *testing.T) {
	tests:=[]struct{mode string; quorum, threshold int}{
		{mode:"all",threshold:2},
		{mode:"any",threshold:1},
		{mode:"quorum",quorum:2,threshold:2},
		{mode:"quorum",quorum:1,threshold:1},
		{mode:"",threshold:2}, // Unspecified mode is fail-closed all.
	}
	for _,tc:=range tests {
		t.Run(fmt.Sprintf("%s-%d",tc.mode,tc.quorum),func(t *testing.T){
			cfg,err:=config.Decode(joinFixture(tc.mode,tc.quorum))
			if err!=nil{t.Fatal(err)}
			for mask:=0;mask<4;mask++{
				a,b:=mask&1!=0,mask&2!=0
				out,err:=New(nil).Run(context.Background(),"r1",cfg,
					"voting","vote",map[string]any{"left":a,"right":b},true)
				count:=0
				if a{count++}
				if b{count++}
				want:=count>=tc.threshold
				if want {
					if err!=nil || out.Status!="success" || out.Value!="approved" {
						t.Fatalf("mode=%s quorum=%d fired=%d: valid join failed: %#v %v",
							tc.mode,tc.quorum,count,out,err)
						}
				} else if err==nil || !strings.Contains(err.Error(),"produced no output") ||
					out.Status!="failure" || out.Value!=nil || out.OutputProduced {
					t.Fatalf("mode=%s quorum=%d fired=%d: inactive join published: %#v %v",
						tc.mode,tc.quorum,count,out,err)
				}
			}
		})
	}
}

func TestJoinCannotSpeculatePastPendingSuspension(t *testing.T) {
	source:=`
pipeline "fan" {
	entry = "start"
	node "start" {
		kind = "input"
	}
	node "a" {
		kind = "transform"
		value = "literal:fast"
	}
	node "middle" {
		kind = "transform"
		value = "literal:slow"
	}
	node "zz_wait" {
		kind = "suspend"
	}
	node "join" {
		kind = "join"
		mode = "any"
		value = "literal:passed"
	}
	node "done" {
		kind = "output"
		value = "nodes.join.value"
	}
	edge {
		from = "start"
		to = "a"
	}
	edge {
		from = "start"
		to = "middle"
	}
	edge {
		from = "a"
		to = "join"
	}
	edge {
		from = "middle"
		to = "zz_wait"
	}
	edge {
		from = "zz_wait"
		to = "join"
	}
	edge {
		from = "join"
		to = "done"
	}
}
surface "fan" {
	path = "/mcp/fan"
	tool "fan" {
		pipeline = "fan"
		read_only = true
	}
}
`
	cfg,err:=config.Decode(source)
	if err!=nil{t.Fatal(err)}
	s,err:=storage.New(t.TempDir(),source)
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	if err:=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
	runtime:=New(s)
	runID:=storage.ID()
	out,err:=runtime.execute(context.Background(),"revision",cfg,
		cfg.Pipelines["fan"],runID,"fan","fan",map[string]any{},
		false,nil)
	if err==nil || out.Status!="suspended"{
		t.Fatalf("branch did not suspend: %#v %v",out,err)
	}
	var cp Checkpoint
	if err:=s.GetSuspended(runID,&cp);err!=nil{t.Fatal(err)}
	if cp.States["a"]!="completed" || cp.States["join"]!="pending"{
		t.Fatalf("join executed before other branch settled: %#v",cp.States)
	}
}

func TestJoinValidationRejectsDuplicatedVotesAndImpossibleQuorums(t *testing.T){
	fixture:=joinFixture("quorum",2)
	duplicate:=strings.Replace(fixture,`
	edge {
		from = "right"
		to = "merge"
		when = "true"
	}
`,`
	edge {
		from = "left"
		to = "merge"
		when = "false"
	}
`,1)
	if duplicate==fixture{t.Fatal("duplicate test fixture not modified")}
	cases:=[]struct{source,marker string}{
		{strings.Replace(fixture,"quorum = 2","quorum = 3",1),"quorum"},
		{strings.Replace(joinFixture("all",0),
			"mode = \"all\"","mode = \"all\"\n    quorum = 1",1),"quorum"},
		{duplicate,"duplicate predecessor"},
	}
	for _,tc:=range cases{
		if _,err:=config.Decode(tc.source);err==nil ||
			!strings.Contains(err.Error(),tc.marker){
			t.Fatalf("invalid join accepted: %v",err)
		}
	}
}

func TestThreeBranchJoinAllAnyQuorumAndConcurrency(t *testing.T) {
	// The 2- and 3-branch TLC models cover threshold activation after
	// every predecessor settles. Exercise the corresponding real Go
	// scheduler with every possible branch-vote combination.
	for _, tc := range []struct {
		mode string
		quorum int
		threshold int
	}{
		{mode:"all", threshold:3},
		{mode:"any", threshold:1},
		{mode:"quorum", quorum:2, threshold:2},
		{mode:"quorum", quorum:1, threshold:1},
		{mode:"quorum", quorum:3, threshold:3},
	} {
		for _, concurrency := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s-%d-concurrency-%d",tc.mode,tc.quorum,concurrency),func(t *testing.T){
				attrs := fmt.Sprintf("mode = %q",tc.mode)
				if tc.mode=="quorum" {attrs+=fmt.Sprintf("\n    quorum = %d",tc.quorum)}
				source := fmt.Sprintf(`pipeline "votes" {
  entry = "start"
  concurrency = %d
  node "start" {
    kind = "input"
  }
  node "a" {
    kind = "condition"
    test = "input.a"
  }
  node "b" {
    kind = "condition"
    test = "input.b"
  }
  node "c" {
    kind = "condition"
    test = "input.c"
  }
  node "merge" {
    kind = "join"
    %s
    value = "literal:joined"
  }
  node "done" {
    kind = "output"
    value = "nodes.merge.value"
  }
  edge {from = "start" to = "a"}
  edge {from = "start" to = "b"}
  edge {from = "start" to = "c"}
  edge {from = "a" to = "merge" when = "true"}
  edge {from = "b" to = "merge" when = "true"}
  edge {from = "c" to = "merge" when = "true"}
  edge {from = "merge" to = "done"}
}
surface "voting" {
  path = "/mcp/voting"
  tool "vote" {
    pipeline = "votes"
    read_only = true
  }
}
`,concurrency,attrs)
				cfg,err := config.Decode(source)
				if err!=nil {t.Fatal(err)}
				for mask:=0;mask<8;mask++ {
					bits := 0
					for i:=0;i<3;i++ {bits+=(mask>>i)&1}
					input:=map[string]any{"a":mask&1!=0,"b":mask&2!=0,"c":mask&4!=0}
					out,err:=New(nil).Run(context.Background(),"rev",cfg,"voting","vote",input,true)
					if bits>=tc.threshold {
						if err!=nil || out.Status!="success" || out.Value!="joined" {
							t.Fatalf("mask=%03b expected join: %#v %v",mask,out,err)
						}
					} else if err==nil || !strings.Contains(err.Error(),"produced no output") ||
						out.Status!="failure" || out.Value!=nil || out.OutputProduced {
						t.Fatalf("mask=%03b should skip join without output: %#v %v",mask,out,err)
					}
				}
			})
		}
	}
}
