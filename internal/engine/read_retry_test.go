package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/burkostya/regate/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func flakyReadUpstream(t *testing.T, drops int64, onDrop func()) (*httptest.Server,*atomic.Int64,*atomic.Int64) {
	t.Helper()
	seen := new(atomic.Int64)
	served := new(atomic.Int64)
	server := sdk.NewServer(&sdk.Implementation{Name:"retry-test",Version:"1"},nil)
	server.AddTool(&sdk.Tool{Name:"lookup",InputSchema:map[string]any{"type":"object"}},
		func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
			served.Add(1)
			return &sdk.CallToolResult{
				StructuredContent:map[string]any{"ok":true},
				Content:[]sdk.Content{&sdk.TextContent{Text:"ok"}},
			},nil
		})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return server},
		&sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		if r.Method==http.MethodPost {
			body,err:=io.ReadAll(io.LimitReader(r.Body,1<<20))
			if err!=nil {http.Error(w,"bad request",400);return}
			r.Body=io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body,[]byte(`"method":"tools/call"`)) {
				if seen.Add(1)<=drops {
					if onDrop!=nil {onDrop()}
					if hj,ok:=w.(http.Hijacker);ok {
						c,_,err:=hj.Hijack()
						if err==nil {_=c.Close();return}
					}
					http.Error(w,"connection failed",502)
					return
				}
			}
		}
		handler.ServeHTTP(w,r)
	})),seen,served
}

func readRetryPipeline(url string, attempts int, delayMS int) string {
	retry := ""
	if attempts > 0 {
		retry = fmt.Sprintf(`
    retry = {
      max_attempts = %d
      delay_ms = %d
      safe_read = true
    }`, attempts, delayMS)
	}
	return fmt.Sprintf(`upstream "crm" {
  url = %q
  timeout_ms = 5000
}
pipeline "reader" {
  entry = "lookup"
  node "lookup" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "lookup"
    effect = "read"
    arguments = "input"%s
  }
  node "done" {
    kind = "output"
    value = "nodes.lookup.value"
  }
  edge {
    from = "lookup"
    to = "done"
  }
}
surface "test" {
  path = "/mcp/test"
  tool "read" {
    pipeline = "reader"
    read_only = true
  }
}
`, url, retry)
}

func TestBoundedReadRetryAfterLostTransportResponses(t *testing.T) {
	upstream,calls,served:=flakyReadUpstream(t,2,nil)
	defer upstream.Close()
	cfg,err:=config.Decode(readRetryPipeline(upstream.URL,3,10))
	if err!=nil {t.Fatal(err)}
	out,err:=New(nil).Run(context.Background(),"rev",cfg,"test","read",map[string]any{"id":"C700"},false)
	if err!=nil || out.Status!="success" || calls.Load()!=3 || served.Load()!=1 {
		t.Fatalf("read retry failed: %#v %v attempted=%d served=%d",out,err,calls.Load(),served.Load())
	}
	value,ok:=out.Value.(map[string]any)
	if !ok||value["ok"]!=true {t.Fatalf("lost read result: %#v",out.Value)}
}

func TestBoundedReadRetryStopsAtDeclaredLimit(t *testing.T) {
	upstream,calls,served:=flakyReadUpstream(t,10,nil)
	defer upstream.Close()
	cfg,err:=config.Decode(readRetryPipeline(upstream.URL,2,10))
	if err!=nil {t.Fatal(err)}
	_,err=New(nil).Run(context.Background(),"rev",cfg,"test","read",nil,false)
	if err==nil || calls.Load()!=2 || served.Load()!=0 {
		t.Fatalf("retry limit bypassed: %v attempts=%d served=%d",err,calls.Load(),served.Load())
	}
}

func TestCancelledReadCannotRetryAfterCredentialBoundary(t *testing.T) {
	ctx,cancel:=context.WithCancel(context.Background())
	defer cancel()
	upstream,calls,served:=flakyReadUpstream(t,10,cancel)
	defer upstream.Close()
	cfg,err:=config.Decode(readRetryPipeline(upstream.URL,4,100))
	if err!=nil {t.Fatal(err)}
	_,err=New(nil).Run(ctx,"rev",cfg,"test","read",nil,false)
	if err==nil || !errors.Is(err,context.Canceled) || calls.Load()!=1 || served.Load()!=0 {
		t.Fatalf("cancelled read retried: %v attempts=%d served=%d",err,calls.Load(),served.Load())
	}
}

func TestExplicitReadRetryNeverDispatchesMCPBusinessErrorTwice(t *testing.T) {
	var calls atomic.Int64
	server:=sdk.NewServer(&sdk.Implementation{Name:"business-error",Version:"1"},nil)
	server.AddTool(&sdk.Tool{Name:"lookup",InputSchema:map[string]any{"type":"object"}},
		func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
			calls.Add(1)
			return &sdk.CallToolResult{IsError:true,Content:[]sdk.Content{
				&sdk.TextContent{Text:"invalid customer"},
			}},nil
		})
	endpoint:=httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request)*sdk.Server{return server},
		&sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true}))
	defer endpoint.Close()
	cfg,err:=config.Decode(readRetryPipeline(endpoint.URL,4,10))
	if err!=nil {t.Fatal(err)}
	got,err:=New(nil).Run(context.Background(),"rev",cfg,"test","read",nil,false)
	if err==nil || !strings.Contains(err.Error(),"isError") || calls.Load()!=1 {
		t.Fatalf("application-level MCP error retried or accepted: %#v %v calls=%d",got,err,calls.Load())
	}
}

func TestReadTransportClassifierRejectsNonTransportFailures(t *testing.T) {
	if retryableReadTransportError(errors.New("policy denied")) ||
		retryableReadTransportError(fmt.Errorf("MCP invalid arguments")) ||
		!retryableReadTransportError(io.EOF) ||
		!retryableReadTransportError(io.ErrUnexpectedEOF) {
		t.Fatal("transport error classifier widened beyond network faults")
	}
}
