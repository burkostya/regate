package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/storage"
)

func twoSuspensionsSource() string {
	return `regate {
  listen = "127.0.0.1:8080"
}
pipeline "twice" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "first" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  node "second" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  node "done" {
    kind = "output"
    value = "nodes.second.value"
  }
  edge {
    from = "start"
    to = "first"
  }
  edge {
    from = "first"
    to = "second"
  }
  edge {
    from = "second"
    to = "done"
  }
}
surface "tests" {
  path = "/mcp/tests"
  tool "twice" {
    pipeline = "twice"
    read_only = true
  }
}
`
}

func pollTask(t *testing.T,svc *Service,id string,done func(storage.TaskRecord)bool) storage.TaskRecord {
	t.Helper()
	deadline:=time.Now().Add(6*time.Second)
	for time.Now().Before(deadline) {
		task,err:=svc.Store.TaskGet(id)
		if err!=nil {t.Fatal(err)}
		if done(task) {return task}
		time.Sleep(10*time.Millisecond)
	}
	task,err:=svc.Store.TaskGet(id)
	t.Fatalf("task condition timed out: %#v %v",task,err)
	return storage.TaskRecord{}
}

func TestSequentialTaskInputsRejectStaleResponseAndAllowCorrection(t *testing.T) {
	svc,err:=New(t.TempDir(),twoSuspensionsSource())
	if err!=nil {t.Fatal(err)}
	defer svc.Store.Close()
	created,err:=svc.CreateTask(context.Background(),"tests","twice","owner",map[string]any{})
	if err!=nil {t.Fatal(err)}
	id:=created["taskId"].(string)
	initial:=pollTask(t,svc,id,func(t storage.TaskRecord)bool{
		return t.Status=="input_required" && t.InputRequests["resume"]!=nil
	})
	if initial.RunID=="" {t.Fatal("root run ID not recorded")}
	// Invalid values are rejected before the suspended step advances and do
	// not consume the one-time input request or a durable external effect.
	_,err=svc.TaskUpdate(id,"owner",map[string]any{
		"resume":map[string]any{"action":"accept","content":42},
	})
	if err!=nil {t.Fatal(err)}
	correctable:=pollTask(t,svc,id,func(t storage.TaskRecord)bool{
		return t.Status=="input_required" &&
			strings.Contains(t.StatusMessage,"Invalid continuation input")
	})
	if len(correctable.InputRequests)!=1 || correctable.InputRequests["resume"]==nil {
		t.Fatalf("invalid continuation consumed request: %#v",correctable)
	}
	_,err=svc.TaskUpdate(id,"owner",map[string]any{
		"resume":map[string]any{"action":"accept","content":"first"},
	})
	if err!=nil {t.Fatal(err)}
	second:=pollTask(t,svc,id,func(task storage.TaskRecord)bool{
		if task.Status!="input_required"||len(task.InputRequests)!=1 {return false}
		return task.InputRequests["resume"]==nil
	})
	var secondKey string
	for key:=range second.InputRequests {secondKey=key}
	if !strings.HasPrefix(secondKey,"resume-") ||
		secondKey=="resume" || len(secondKey)<=len("resume-") {
		t.Fatalf("second suspension lacks unique request key: %q",secondKey)
	}
	// Old duplicate response cannot advance the new leaf.
	_,err=svc.TaskUpdate(id,"owner",map[string]any{
		"resume":map[string]any{"action":"accept","content":"stale"},
	})
	if err!=nil {t.Fatal(err)}
	unchanged,err:=svc.Store.TaskGet(id)
	if err!=nil {t.Fatal(err)}
	if unchanged.Status!="input_required" || unchanged.InputRequests[secondKey]==nil {
		t.Fatalf("stale reply advanced future checkpoint: %#v",unchanged)
	}
	_,err=svc.TaskUpdate(id,"owner",map[string]any{
		secondKey:map[string]any{"action":"accept","content":"second"},
	})
	if err!=nil {t.Fatal(err)}
	completed:=pollTask(t,svc,id,func(task storage.TaskRecord)bool{
		return task.Status=="completed"
	})
	if completed.Result["structuredContent"]!="second" {
		t.Fatalf("final continuation value mismatched: %#v",completed.Result)
	}
}

func TestResumeInputSelectionRejectsAmbiguousRequestMaps(t *testing.T) {
	original:=storage.TaskRecord{InputRequests:suspensionInputRequest("resume","first")}
	if got:=resumeResponseKey(original,false);got!="resume" {
		t.Fatalf("first request key: %q",got)
	}
	second:=storage.TaskRecord{InputRequests:suspensionInputRequest("resume-"+storage.ID(),"next")}
	var want string
	for k:=range second.InputRequests {want=k}
	if got:=resumeResponseKey(second,false);got!=want {
		t.Fatalf("second request key: %q, wanted %q",got,want)
	}
	second.InputRequests["resume"]=map[string]any{}
	if got:=resumeResponseKey(second,false);got!="" {
		t.Fatalf("ambiguous request map was accepted: %q",got)
	}
	// Grant-backed in-process refresh retains the historic request name.
	if got:=resumeResponseKey(second,true);got!="resume" {
		t.Fatalf("grant compatibility request key: %q",got)
	}
}
