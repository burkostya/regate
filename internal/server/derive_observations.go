package server

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/identity"
	"github.com/burkostya/regate/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const publicToolTimeout = 90 * time.Second

type observedRunKey struct{}

type observedRun struct {
	id        string
	suspended bool
}

func (s *Service) observeTools(server *sdk.Server, surface config.Surface, revision *string) {
	server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, request sdk.Request) (sdk.Result, error) {
			call, ok := request.(*sdk.CallToolRequest)
			if method != "tools/call" || !ok || call.Params == nil {
				return next(ctx, method, request)
			}
			var headers http.Header
			if call.Extra != nil {
				headers = call.Extra.Header
			}
			runID := s.beginToolObservation(ctx, surface, *revision, call.Params.Name, headers, call.Params.GetMeta())
			if runID == "" {
				return next(ctx, method, request)
			}
			success := false
			run := &observedRun{id: runID}
			defer func() {
				if !run.suspended {
					s.finishToolObservation(runID, success)
				}
			}()
			result, err := next(context.WithValue(ctx, observedRunKey{}, run), method, request)
			if completed, ok := result.(*sdk.CallToolResult); ok && completed != nil {
				success = err == nil && !completed.IsError && ctx.Err() == nil
			}
			return result, err
		}
	})
}

func explicitObservationID(surface config.Surface, headers http.Header, meta map[string]any) (source, value string) {
	valid := func(value string) bool {
		return value != "" && len(value) <= 1024 && strings.TrimSpace(value) == value &&
			strings.IndexFunc(value, unicode.IsControl) < 0
	}
	for _, name := range surface.DeriveSessionHeaders {
		values := headers.Values(name)
		if len(values) == 1 && !strings.Contains(values[0], ",") && valid(values[0]) {
			return "header:" + strings.ToLower(name), values[0]
		}
	}
	for _, key := range surface.DeriveSessionMetaKeys {
		if value, ok := meta[key].(string); ok && valid(value) {
			return "meta:" + key, value
		}
	}
	return "", ""
}

func (s *Service) beginToolObservation(ctx context.Context, surface config.Surface, revision, name string, headers http.Header, meta map[string]any) string {
	if !surface.DeriveRules {
		return ""
	}
	source, value := explicitObservationID(surface, headers, meta)
	principal, verified := identity.FromContext(ctx)
	if source == "" && !verified {
		return ""
	}
	if _, exists := surface.FindTool(name); !exists {
		// An unrecognised request breaks adjacency without retaining a
		// caller-supplied tool name.
		name = ""
	}
	owner := []string{"anonymous"}
	if verified {
		owner = []string{"oauth", principal.Issuer, principal.Audience, principal.Subject}
	} else if token := os.Getenv("REGATE_PUBLIC_TOKEN"); token != "" {
		owner = []string{"bearer", token}
	}
	runID := storage.ID()
	group := s.Store.ToolCallGroup(append([]string{surface.Name, revision, source, value}, owner...)...)
	write, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := s.Store.StartToolCall(write, storage.ToolCallObservation{
		RunID: runID, Surface: surface.Name, Revision: revision, Tool: name,
		Group: group, Explicit: source != "", StartedNS: time.Now().UnixNano(),
	}); err != nil {
		return ""
	}
	return runID
}

func (s *Service) finishToolObservation(runID string, success bool) {
	if runID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = s.Store.FinishToolCall(ctx, runID, time.Now(), success)
}

func (s *Service) runTaskTool(ctx context.Context, cfg *config.Config, task storage.TaskRecord, args map[string]any) (engine.Outcome, error) {
	if task.ToolCallObserved {
		return s.Engine.RunObserved(ctx, task.Revision, cfg, task.Surface, task.Tool, args, task.RunID)
	}
	return s.Engine.Run(ctx, task.Revision, cfg, task.Surface, task.Tool, args, false)
}

func (s *Service) finishTaskObservation(task storage.TaskRecord) {
	if !task.ToolCallObserved {
		return
	}
	if task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled" {
		s.finishToolObservation(task.RunID, task.Status == "completed" && task.Result != nil && task.Result["isError"] != true)
	}
}
