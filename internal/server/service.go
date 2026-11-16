package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/storage"
)

type Service struct {
	Store         *storage.Store
	Engine        *engine.Runtime
	mu            sync.RWMutex
	cfg           *config.Config
	rev           storage.Revision
	catalog       map[string]catalogEntry
	tasksMu       sync.Mutex
    queueOnce sync.Once
    queueWake chan struct{}
	subscriptions subscriptionBus
	inbound       *inboundMCP
}
type catalogEntry struct {
    // Items retains the original management API tools-only contract.
	Items []map[string]any `json:"items"`
    Resources []map[string]any `json:"resources"`
    ResourceTemplates []map[string]any `json:"resourceTemplates"`
    Prompts []map[string]any `json:"prompts"`
    Revision string `json:"revision"`
	At time.Time `json:"at"`
	Error string `json:"error,omitempty"`
}

func New(stateDir, source string) (*Service, error) {
	if _, e := config.Decode(source); e != nil {
		return nil, e
	}
	store, e := storage.New(stateDir, source)
	if e != nil {
		return nil, e
	}
	rev, active, e := store.Active()
	if e != nil {
        _=store.Close()
		return nil, e
	}
	cfg, e := config.Decode(active)
	if e != nil {
        _=store.Close()
		return nil, fmt.Errorf("persisted active config: %w", e)
	}
    if cfg.MaxQueuedTasks>0 && !store.HasTaskQueueKeys() {
        _=store.Close()
        return nil,fmt.Errorf("start encrypted queue: %w",storage.ErrTaskQueueKeyUnavailable)
    }
    if e=store.AcquireExecutionLease();e!=nil {
        _=store.Close()
        return nil,fmt.Errorf("start execution leader: %w",e)
    }
	svc := &Service{Store: store, Engine: engine.New(store), cfg: cfg, rev: rev, catalog: map[string]catalogEntry{}}
    svc.inbound, e = newInboundMCP(svc, cfg, rev.ID)
    if e != nil {
        _=store.Close()
        return nil, fmt.Errorf("initialize MCP server: %w", e)
    }
    depth,err:=store.QueueDepth(context.Background())
    if err!=nil {_=store.Close();return nil,err}
    if depth>0 && !store.HasTaskQueueKeys() {
        _=store.Close()
        return nil,fmt.Errorf("queued inputs require keys: %w",storage.ErrTaskQueueKeyUnavailable)
    }
    if cfg.MaxQueuedTasks>0 || depth>0 {svc.startQueueDispatcher()}
    return svc, nil
}
func (s *Service) Current() (*config.Config, storage.Revision) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.rev
}
func (s *Service) Validate(source string) (*config.Config, error) { return config.Decode(source) }
func (s *Service) Activate(id string) (storage.Revision, error) {
	d, e := s.Store.DraftGet(id)
	if e != nil {
		return storage.Revision{}, e
	}
	cfg, e := s.Validate(d.Source)
	if e != nil {
		return storage.Revision{}, e
	}
    if cfg.MaxQueuedTasks>0 && !s.Store.HasTaskQueueKeys() {
        return storage.Revision{},storage.ErrTaskQueueKeyUnavailable
    }
	next, revision, e := s.inbound.prepare(s, cfg)
    if e != nil { return storage.Revision{}, fmt.Errorf("prepare MCP catalog: %w", e) }
    s.mu.Lock()
    rev, e := s.Store.Activate(id)
	if e != nil {
		s.mu.Unlock()
		return rev, e
	}
	*revision = rev.ID
    s.inbound.publish(next)
    s.cfg = cfg
    s.rev = rev
    s.catalog = map[string]catalogEntry{}
    s.mu.Unlock()
    if cfg.MaxQueuedTasks>0 {s.startQueueDispatcher()}
	s.subscriptions.emit("notifications/tools/list_changed")
	s.subscriptions.emit("notifications/resources/list_changed")
	s.subscriptions.emit("notifications/prompts/list_changed")
	return rev, nil
}
func (s *Service) Rollback(target string) (storage.Revision, error) {
	src, e := s.Store.Source(target)
	if e != nil {
		return storage.Revision{}, e
	}
	cfg, e := s.Validate(src)
	if e != nil {
		return storage.Revision{}, e
	}
    if cfg.MaxQueuedTasks>0 && !s.Store.HasTaskQueueKeys() {
        return storage.Revision{},storage.ErrTaskQueueKeyUnavailable
    }
	next, revision, e := s.inbound.prepare(s, cfg)
    if e != nil { return storage.Revision{}, fmt.Errorf("prepare MCP catalog: %w", e) }
    s.mu.Lock()
    rev, e := s.Store.Rollback(target)
	if e != nil {
		s.mu.Unlock()
		return rev, e
	}
	*revision = rev.ID
    s.inbound.publish(next)
    s.cfg = cfg
    s.rev = rev
    s.catalog = map[string]catalogEntry{}
    s.mu.Unlock()
    if cfg.MaxQueuedTasks>0 {s.startQueueDispatcher()}
	s.subscriptions.emit("notifications/tools/list_changed")
	s.subscriptions.emit("notifications/resources/list_changed")
	s.subscriptions.emit("notifications/prompts/list_changed")
	return rev, nil
}
func (s *Service) Discover(ctx context.Context, name string, refresh bool) (catalogEntry, error) {
    cfg, revision := s.Current()
    s.mu.RLock()
    e, ok := s.catalog[name]
    s.mu.RUnlock()
    // A catalog discovered under another config revision may represent an
    // entirely different upstream authority. Never reuse it.
    if !refresh && ok && e.Revision == revision.ID && time.Since(e.At) < time.Minute {
        if e.Error!="" {return e,fmt.Errorf("cached upstream discovery failed: %s",e.Error)}
        return e,nil
    }
    items, err := s.Engine.DiscoverCatalog(ctx, cfg, name)
    e = catalogEntry{
        Items:items.Tools, Resources:items.Resources,
        ResourceTemplates:items.ResourceTemplates, Prompts:items.Prompts,
        Revision:revision.ID, At:time.Now().UTC(),
    }
    if err!=nil {e.Error=err.Error()}
    s.mu.Lock()
    // If activation raced with discovery, return this historical result to
    // its caller but never publish it as the current catalog cache.
    if s.rev.ID==revision.ID {s.catalog[name]=e}
    s.mu.Unlock()
    return e,err
}
func (s *Service) Status() map[string]any {
    cfg,rev:=s.Current()
    state:=map[string]any{
        "name":"regate","version":"0.1.0","revision":rev.ID,"listen":cfg.Listen,
        "upstreams":len(cfg.Upstreams),"surfaces":len(cfg.Surfaces),
        "pipelines":len(cfg.Pipelines),"hooks":len(cfg.Hooks),
        "policies":len(cfg.Policies),"http_only":true,
        "max_active_tasks":cfg.MaxActiveTasks,"max_queued_tasks":cfg.MaxQueuedTasks,
    }
    if active,err:=s.Store.ActiveTaskCount(context.Background());err==nil {
        state["active_tasks"]=active
    }
    if queued,err:=s.Store.QueueDepth(context.Background());err==nil {
        state["queued_tasks"]=queued
    }
    return state
}
func (s *Service) Inspect(name string) map[string]any {
	cfg, _ := s.Current()
	if name == "" {
		return map[string]any{"upstreams": cfg.Upstreams, "hooks": cfg.Hooks, "policies": cfg.Policies}
	}
	u, ok := cfg.Upstreams[name]
	if ok {
		return map[string]any{"upstream": u}
	}
	return map[string]any{"error": "unknown upstream"}
}
func JsonPretty(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
func GenerateToolBlock(name, source, pipeline, description string) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "  tool %q {\n", name)
	if source != "" {
		fmt.Fprintf(&buf, "    source = %q\n", source)
	}
	if pipeline != "" {
		fmt.Fprintf(&buf, "    pipeline = %q\n", pipeline)
	}
	fmt.Fprintf(&buf, "    description = %q\n  }\n", description)
	return buf.String()
}
func AdminToken() string { return os.Getenv("REGATE_ADMIN_TOKEN") }
