package server

import (
    "context"
    "errors"
    "maps"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/mcp"
    "github.com/burkostya/regate/internal/storage"
)

// End-to-end local provider contracts (both separate OS processes).
// Actual provider state, not a mock gateway response, is fsynced after each
// write, fence takeover, idempotent replay and compensating tombstone.
func TestTwoProcessMCPDurableProviderContracts(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","two-mcp-safety-admin")
    root:=t.TempDir()
    dirs:=map[string]string{}
    providers:=map[string]*standProcess{}
    for _,name:=range []string{"alpha","beta"} {
        dirs[name]=filepath.Join(root,name)
        providers[name]=startStandProcess(t,name,dirs[name])
    }
    data,err:=os.ReadFile("../../examples/two-mcp-stand.hcl")
    if err!=nil {t.Fatal(err)}
    src:=string(data)
    src=strings.ReplaceAll(src,"http://127.0.0.1:9201",
        strings.TrimSuffix(providers["alpha"].url,"/mcp"))
    src=strings.ReplaceAll(src,"http://127.0.0.1:9202",
        strings.TrimSuffix(providers["beta"].url,"/mcp"))
    svc,err:=New(filepath.Join(root,"gateway"),src)
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    gateway:=httptest.NewServer(svc.Handler())
    defer gateway.Close()
    base:=gateway.URL
    ctx,cancel:=context.WithTimeout(context.Background(),35*time.Second)
    defer cancel()

    for index,name:=range []string{"alpha","beta"} {
        t.Run(name,func(t *testing.T){
            // A real provider fsyncs original value + operation token BEFORE
            // the transport is lost, for two wholly separate local processes.
            lost:=map[string]any{
                "record_id":"inverse_"+name,
                "value":"synthetic_"+name,
                "fault":"drop_after_commit",
            }
            received:=standCall(t,base,name,name+"_put",lost)
            if received["isError"]!=true {
                t.Fatalf("lost write response was reported confirmed: %v",received)
            }
            var source storage.Effect
            for _,effect:=range svc.Store.Effects() {
                if effect.Operation==name+".put_record" && effect.Status=="unknown" {
                    source=effect
                }
            }
            if source.ID=="" || source.IntentDigest=="" ||
                source.TargetDigest!=storage.TargetDigest("mcp",providers[name].url) {
                t.Fatalf("unknown write has no durable provenance: %+v",source)
            }
            // Compensation cannot be dispatched on a merely UNKNOWN result.
            inverseArgs:=map[string]any{
                "effect_id":source.ID,"contract":name+"_undo",
                "intent":lost,"actor":"test-operator",
                "reason":"verified remote record and approved compensating tombstone",
            }
            blocked:=jsonPost(t,base+"/api/effect-compensate",
                "two-mcp-safety-admin",inverseArgs)
            _=blocked.Body.Close()
            if blocked.StatusCode!=http.StatusBadRequest {
                t.Fatalf("unknown original was compensated: HTTP %d",blocked.StatusCode)
            }
            unauth:=jsonPost(t,base+"/api/effect-compensate","",inverseArgs)
            _=unauth.Body.Close()
            if unauth.StatusCode!=http.StatusUnauthorized {
                t.Fatalf("anonymous inverse accepted: HTTP %d",unauth.StatusCode)
            }
            read:=standContent(t,standCall(t,base,name,name+"_get",
                map[string]any{"record_id":"inverse_"+name}))
            assertRecord(t,read,name,"inverse_"+name,"synthetic_"+name)
            originalToken:=read["record"].(map[string]any)["operation_token"]
            if token,ok:=originalToken.(string);!ok || len(token)!=64 {
                t.Fatalf("provider did not fsync injected operation identity: %v",originalToken)
            }
            // Regate never retries UNKNOWN writes automatically. For the
            // provider-native contract, explicitly replay the identical
            // operation token after the lost network response: the provider
            // must return its prior result without a second commit.
            direct:=mcp.NewConn(providers[name].url,"",5*time.Second)
            nativeReplay,err:=direct.CallTool(ctx,"put_record",map[string]any{
                "record_id":"inverse_"+name,"value":"synthetic_"+name,
                "operation_token":originalToken,
            })
            if err!=nil || nativeReplay["isError"]==true ||
                nativeReplay["structuredContent"].(map[string]any)["idempotent_replay"]!=true {
                t.Fatalf("provider repeated a lost-response write: %#v %v",nativeReplay,err)
            }
            assertStandCount(t,standStats(t,base,name),"writes_committed",1)
            wrongIntent:=map[string]any{"record_id":"inverse_"+name,
                "value":"tampered","fault":"drop_after_commit"}
            rejected:=jsonPost(t,base+"/api/effect-readback","two-mcp-safety-admin",
                map[string]any{"effect_id":source.ID,
                    "adapter":name+"_record","intent":wrongIntent})
            _=rejected.Body.Close()
            if rejected.StatusCode!=http.StatusBadRequest {
                t.Fatalf("changed original intent accepted: HTTP %d",rejected.StatusCode)
            }
            if other:=[]string{"alpha","beta"}[1-index];other!=name {
                cross:=jsonPost(t,base+"/api/effect-compensate",
                    "two-mcp-safety-admin",map[string]any{
                        "effect_id":source.ID,"contract":other+"_undo",
                        "intent":lost,"actor":"test-operator",
                        "reason":"intentionally wrong endpoint"})
                _=cross.Body.Close()
                if cross.StatusCode!=http.StatusBadRequest {
                    t.Fatalf("cross-provider inverse accepted: HTTP %d",cross.StatusCode)
                }
            }
            liveCfg,_:=svc.Current()
			guardedCfg := *liveCfg
			guardedCfg.Upstreams = maps.Clone(liveCfg.Upstreams)
			upstream := guardedCfg.Upstreams[name]
			tools, err := direct.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			pins := mcp.ToolSchemaFingerprints(tools)
			if len(pins["get_record"]) != 64 || len(pins["revoke_record"]) != 64 {
				t.Fatal("fixture did not advertise readback and compensation schemas")
			}
			wrongPin := strings.Repeat("0", 64)
			upstream.ToolSchemaPins = map[string]string{"get_record": wrongPin, "revoke_record": wrongPin}
			guardedCfg.Upstreams[name] = upstream
			priorReads := standStats(t, base, name)["reads"].(float64)
			if _, err := svc.Engine.ReconcileMCP(ctx, &guardedCfg, source.ID, name+"_record", lost); err == nil {
				t.Fatal("mismatched pin admitted a provider readback")
			}
			assertStandCount(t, standStats(t, base, name), "reads", priorReads)
			upstream.ToolSchemaPins["get_record"] = pins["get_record"]
			applied, err := svc.Engine.ReconcileMCP(ctx, &guardedCfg, source.ID, name+"_record", lost)
			if err != nil || applied.Status != "resolved_applied" {
				t.Fatalf("readback under the reviewed schema failed: %v", err)
			}
			if _, err := svc.Engine.CompensateMCP(ctx, &guardedCfg, source.ID, name+"_undo", lost, "test-operator", "schema mismatch test"); err == nil {
				t.Fatal("mismatched pin admitted compensation")
			}
			if _, err := svc.Store.EffectGet(storage.CompensationID(source.ID)); err == nil {
				t.Fatal("mismatched pin created a compensation effect")
			}
            // The provider record is authentic, but the current HCL
            // could be maliciously changed before compensation. Deriving
            // the write token with a drifted adapter must NOT authorize
            // the old effect under new semantics or dispatch any inverse.
            drifted:=*liveCfg
            drifted.Readbacks=make(map[string]config.Readback,len(liveCfg.Readbacks))
            for key,definition:=range liveCfg.Readbacks {drifted.Readbacks[key]=definition}
            driftedAdapter:=drifted.Readbacks[name+"_record"]
            driftedAdapter.ReadTool="stats"
            drifted.Readbacks[name+"_record"]=driftedAdapter
            if _,err:=svc.Engine.CompensateMCP(ctx,&drifted,source.ID,
                name+"_undo",lost,"test-operator","drifted adapter must fail");
                !errors.Is(err,storage.ErrInvalidEvidence) {
                t.Fatalf("readback contract drift allowed inverse: %v",err)
            }
            spoofed:=map[string]any{}
            for key,value:=range lost {spoofed[key]=value}
            spoofed["operation_token"]=originalToken
            if _,err:=svc.Engine.CompensateMCP(ctx,liveCfg,source.ID,
                name+"_undo",spoofed,"test-operator","caller token must fail");
                !errors.Is(err,storage.ErrInvalidEvidence) {
                t.Fatalf("caller-supplied provider write token authorized inverse: %v",err)
            }
            assertStandCount(t,standStats(t,base,name),"compensations_received",0)

			upstream.ToolSchemaPins["revoke_record"] = pins["revoke_record"]
			inverse, err := svc.Engine.CompensateMCP(ctx, &guardedCfg, source.ID, name+"_undo", lost, "test-operator", "verified remote record and approved compensating tombstone")
			if err != nil || inverse.Status != "confirmed" || inverse.Compensates != source.ID {
				t.Fatalf("approved inverse under the reviewed schema failed: %v", err)
			}
            persisted,err:=svc.Store.EffectGet(storage.CompensationID(source.ID))
            if err!=nil || persisted.Status!="confirmed" ||
                persisted.Compensates!=source.ID || persisted.TargetDigest!=source.TargetDigest ||
                persisted.Approval==nil || persisted.Approval.Actor!="test-operator" {
                t.Fatalf("inverse lacks immutable audit/provenance: %+v %v",persisted,err)
            }
            prior,err:=svc.Store.EffectGet(source.ID)
            if err!=nil || prior.Status!="resolved_applied" {
                t.Fatalf("inverse silently rewrote original proof: %+v %v",prior,err)
            }
            after:=standContent(t,standCall(t,base,name,name+"_get",
                map[string]any{"record_id":"inverse_"+name}))
            record:=after["record"].(map[string]any)
            if record["compensated"]!=true || record["operation_token"]!=originalToken ||
                record["value"]!="synthetic_"+name {
                t.Fatalf("provider tombstone destroyed original evidence: %v",record)
            }
            repeated:=jsonPost(t,base+"/api/effect-compensate",
                "two-mcp-safety-admin",inverseArgs)
            _=repeated.Body.Close()
            if repeated.StatusCode!=http.StatusBadRequest {
                t.Fatalf("Regate admitted duplicate inverse: HTTP %d",repeated.StatusCode)
            }
            duplicate,err:=direct.CallTool(ctx,"revoke_record",
                map[string]any{"record_id":"inverse_"+name})
            if err!=nil || duplicate["isError"]!=true {
                t.Fatalf("provider itself admitted a duplicate inverse: %#v %v",duplicate,err)
            }
            stats:=standStats(t,base,name)
            assertStandCount(t,stats,"writes_committed",1)
            assertStandCount(t,stats,"compensations_received",2)
            assertStandCount(t,stats,"compensations_committed",1)

            // The INVERSE can itself be committed by the provider before
            // either an ordinary MCP application error or a lost TCP reply.
            // No retry follows an unknown inverse, even though an independent
            // get_record proves that the tombstone survived.
            failureID:="inverse_error_"+name
            if index==1 {failureID="inverse_drop_"+name}
            failureIntent:=map[string]any{
                "record_id":failureID,"value":"second_synthetic_"+name,
                "fault":"error_after_commit",
            }
            secondWrite:=standCall(t,base,name,name+"_put",failureIntent)
            if secondWrite["isError"]!=true {
                t.Fatalf("test failed to create second UNKNOWN write: %#v",secondWrite)
            }
            var secondEffect storage.Effect
            for _,candidate:=range svc.Store.Effects() {
                if candidate.Operation==name+".put_record" && candidate.Status=="unknown" {
                    secondEffect=candidate
                }
            }
            if secondEffect.ID=="" || secondEffect.ID==source.ID {
                t.Fatalf("second independent unknown effect missing: %+v",secondEffect)
            }
            secondResolved:=decode(t,jsonPost(t,base+"/api/effect-readback",
                "two-mcp-safety-admin",map[string]any{
                    "effect_id":secondEffect.ID,"adapter":name+"_record",
                    "intent":failureIntent,
                }))
            if secondResolved["status"]!="resolved_applied" {
                t.Fatalf("second provider write did not reconcile: %#v",secondResolved)
            }
            failureInverseArgs:=map[string]any{
                "effect_id":secondEffect.ID,"contract":name+"_undo",
                "intent":failureIntent,"actor":"test-operator",
                "reason":"post-commit compensation ambiguity test",
            }
            inverseResponse:=jsonPost(t,base+"/api/effect-compensate",
                "two-mcp-safety-admin",failureInverseArgs)
            _=inverseResponse.Body.Close()
            if inverseResponse.StatusCode==http.StatusOK {
                t.Fatal("post-commit inverse error or lost response was incorrectly confirmed")
            }
            unknownInverse,err:=svc.Store.EffectGet(storage.CompensationID(secondEffect.ID))
            if err!=nil || unknownInverse.Status!="unknown" ||
                unknownInverse.Compensates!=secondEffect.ID {
                t.Fatalf("ambiguous inverse is not a durable unknown effect: %+v %v",unknownInverse,err)
            }
            tombstone:=standContent(t,standCall(t,base,name,name+"_get",
                map[string]any{"record_id":failureID}))
            if tombstone["record"].(map[string]any)["compensated"]!=true {
                t.Fatalf("provider committed inverse not visible after fault: %#v",tombstone)
            }
            noRetry:=jsonPost(t,base+"/api/effect-compensate",
                "two-mcp-safety-admin",failureInverseArgs)
            _=noRetry.Body.Close()
            if noRetry.StatusCode!=http.StatusBadRequest {
                t.Fatalf("unknown inverse was blindly replayed: HTTP %d",noRetry.StatusCode)
            }
            // A human may make a durable resolution only after a separate
            // provider read. This is NOT an automatic absence inference.
            explicit:=decode(t,jsonPost(t,base+"/api/effect-decide",
                "two-mcp-safety-admin",map[string]any{
                    "effect_id":unknownInverse.ID,"actor":"test-operator",
                    "verdict":"applied",
                    "reason":"verified fixture tombstone by fresh provider read",
                }))
            if explicit["status"]!="resolved_applied" {
                t.Fatalf("operator did not resolve verified inverse: %#v",explicit)
            }
            stats=standStats(t,base,name)
            assertStandCount(t,stats,"compensations_received",3)
            assertStandCount(t,stats,"compensations_committed",2)
            assertStandCount(t,stats,"writes_committed",2)

            // Provider-native high-watermark: an epoch installed by the new
            // leader is persisted independently on each provider. Stale
            // generations cannot commit even if they carry a valid token.
            epoch:=float64(index+4)
            current:=int(index+4)
            fence,err:=direct.CallTool(ctx,"advance_fence",
                map[string]any{"fence_epoch":current})
            if err!=nil || fence["isError"]==true {
                t.Fatalf("new provider fence rejected: %#v %v",fence,err)
            }
            oldFence,err:=direct.CallTool(ctx,"advance_fence",
                map[string]any{"fence_epoch":current-1})
            if err!=nil || oldFence["isError"]!=true {
                t.Fatalf("stale fence promoted provider: %#v %v",oldFence,err)
            }
            newRecord:=map[string]any{
                "record_id":"fenced_"+name,"value":"fenced_synthetic_"+name,
                "fence_epoch":current,
            }
            success:=standContent(t,standCall(t,base,name,name+"_put",newRecord))
            assertRecord(t,success,name,"fenced_"+name,"fenced_synthetic_"+name)
            token,ok:=success["record"].(map[string]any)["operation_token"].(string)
            if !ok || len(token)!=64 {t.Fatal("fenced write lost provider operation token")}
            remoteArgs:=map[string]any{
                "record_id":"fenced_"+name,"value":"fenced_synthetic_"+name,
                "operation_token":token,"fence_epoch":current,
            }
            same,err:=direct.CallTool(ctx,"put_record",remoteArgs)
            if err!=nil || same["isError"]==true ||
                same["structuredContent"].(map[string]any)["idempotent_replay"]!=true {
                t.Fatalf("native same-token replay not idempotent: %#v %v",same,err)
            }
            changed:=map[string]any{
                "record_id":"fenced_"+name,"value":"different",
                "operation_token":token,"fence_epoch":current,
            }
            mismatch,err:=direct.CallTool(ctx,"put_record",changed)
            if err!=nil || mismatch["isError"]!=true {
                t.Fatalf("reused idempotency key changed payload: %#v %v",mismatch,err)
            }
            reuse,err:=direct.CallTool(ctx,"put_record",map[string]any{
                "record_id":"other_"+name,"value":"fenced_synthetic_"+name,
                "operation_token":token,"fence_epoch":current,
            })
            if err!=nil || reuse["isError"]!=true {
                t.Fatalf("operation key reused across records: %#v %v",reuse,err)
            }
            stale,err:=direct.CallTool(ctx,"put_record",map[string]any{
                "record_id":"stale_"+name,"value":"should_never_commit",
                "fence_epoch":current-1,
            })
            if err!=nil || stale["isError"]!=true {
                t.Fatalf("old leader wrote after provider takeover: %#v %v",stale,err)
            }
            stats=standStats(t,base,name)
            assertStandCount(t,stats,"writes_committed",3)
            assertStandCount(t,stats,"idempotent_replays",2)
            assertStandCount(t,stats,"fenced_rejections",1)
            assertStandCount(t,stats,"fence_epoch",epoch)

            // Kill the provider, reopen its fsynced directory under a
            // distinct process and verify idempotency + fence + tombstone.
            oldPID:=providers[name].cmd.Process.Pid
            providers[name].stop()
            fresh:=startStandProcess(t,name,dirs[name])
            providers[name]=fresh
            if fresh.cmd.Process.Pid==oldPID {t.Fatal("provider process was not replaced")}
            restarted:=mcp.NewConn(fresh.url,"",5*time.Second)
            oldRecord,err:=restarted.CallTool(ctx,"get_record",
                map[string]any{"record_id":"inverse_"+name})
            if err!=nil || oldRecord["isError"]==true {t.Fatalf("restarted provider lost tombstone: %v",err)}
            old:=standContent(t,oldRecord)["record"].(map[string]any)
            if old["compensated"]!=true || old["operation_token"]!=originalToken {
                t.Fatalf("provider restart lost compensation provenance: %#v",old)
            }
            secondAfterRestart,err:=restarted.CallTool(ctx,"get_record",
                map[string]any{"record_id":failureID})
            if err!=nil || secondAfterRestart["isError"]==true ||
                standContent(t,secondAfterRestart)["record"].(map[string]any)["compensated"]!=true {
                t.Fatalf("provider restart lost uncertain inverse tombstone: %#v %v",secondAfterRestart,err)
            }
            persistedStats,err:=restarted.CallTool(ctx,"stats",map[string]any{})
            if err!=nil {t.Fatal(err)}
            recovered:=standContent(t,persistedStats)
            assertStandCount(t,recovered,"writes_committed",3)
            assertStandCount(t,recovered,"compensations_committed",2)
            assertStandCount(t,recovered,"idempotent_replays",2)
            assertStandCount(t,recovered,"fence_epoch",epoch)
            afterRestart,err:=restarted.CallTool(ctx,"put_record",
                map[string]any{"record_id":"stale_after_restart_"+name,
                    "value":"forbidden","fence_epoch":current-1})
            if err!=nil || afterRestart["isError"]!=true {
                t.Fatalf("process restart reset provider fence: %#v %v",afterRestart,err)
            }
            // A *fresh* leader generation fences the old generation even
            // for an otherwise correct operation key.
            promoted,err:=restarted.CallTool(ctx,"advance_fence",
                map[string]any{"fence_epoch":current+1})
            if err!=nil || promoted["isError"]==true {t.Fatalf("second promotion failed: %v",err)}
            oldReplay,err:=restarted.CallTool(ctx,"put_record",remoteArgs)
            if err!=nil || oldReplay["isError"]!=true {
                t.Fatalf("stale token replay bypassed new epoch: %#v %v",oldReplay,err)
            }
            t.Logf("%s: durable readback/inverse, idempotence and fencing across independent process restart",name)
        })
    }
}
