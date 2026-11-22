package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/storage"
)

func TestStaticConfigPromotionPreservesDynamicState(t *testing.T) {
	const upstream = `upstream "beta" {
  url = "http://127.0.0.1:9090/mcp"
}`
	const surface = `# Keep this comment with the dynamic surface.
surface "readers" {
  path = "/mcp/readers"
  tool "lookup" {
    source = "beta.lookup"
    read_only = true
  }
}
`
	dir := t.TempDir()
	files := ConfigFiles{
		Initial: filepath.Join(dir, "bootstrap.hcl"),
		Static:  filepath.Join(dir, "static.hcl"),
	}
	write := func(path, source string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	start := func() *Service {
		t.Helper()
		service, err := OpenConfigFiles(t.Context(), filepath.Join(dir, "state"), files, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.Store.Close() })
		return service
	}
	close := func(service *Service) {
		t.Helper()
		if err := service.Store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	draft := func(service *Service, source string) storage.Draft {
		t.Helper()
		created, err := service.Store.DraftCreate()
		if err != nil {
			t.Fatal(err)
		}
		updated, err := service.Store.DraftUpdate(created.ID, source)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	}

	write(files.Initial, upstream+"\n\n"+surface)
	write(files.Static, "")
	first := start()
	_, oldRevision := first.Current()
	retained := draft(first, upstream+"\n\n"+surface+"# unsaved deployment draft\n")
	if err := first.Store.Effect(storage.Effect{ID: "prior-write", Status: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	close(first)

	if err := os.Remove(files.Initial); err != nil {
		t.Fatal(err)
	}
	write(files.Static, upstream+"\n")
	promoted := start()
	cfg, revision := promoted.Current()
	view := promoted.configView(cfg, revision.ID)
	if revision.Parent != oldRevision.ID || view["dynamic_source"] != "\n\n"+surface {
		t.Fatalf("promotion did not retain dynamic HCL: %#v", view)
	}
	if got, err := promoted.Store.DraftGet(retained.ID); err != nil || got.Source != retained.Source {
		t.Fatalf("draft did not survive the deployment: %v", err)
	}
	if got, err := promoted.Store.EffectGet("prior-write"); err != nil || got.Status != "confirmed" {
		t.Fatalf("effect did not survive the deployment: %v", err)
	}
	forged := draft(promoted, upstream+"\n"+surface)
	if _, err := promoted.Activate(forged.ID); !errors.Is(err, storage.ErrStaticConfigConflict) {
		t.Fatalf("static prefix could be changed through activation: %v", err)
	}
	if _, err := promoted.Rollback(oldRevision.ID); !errors.Is(err, storage.ErrStaticConfigConflict) {
		t.Fatalf("rollback crossed the static deployment: %v", err)
	}
	edited := draft(promoted, strings.ReplaceAll(cfg.Source, "/mcp/readers", "/mcp/updated"))
	dynamicRevision, err := promoted.Activate(edited.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(promoted)

	unchanged := start()
	if _, current := unchanged.Current(); current.ID != dynamicRevision.ID {
		t.Fatal("restart with identical static content created a revision")
	}
	close(unchanged)

	write(files.Static, "")
	rejected := start()
	if _, current := rejected.Current(); current.ID != dynamicRevision.ID || rejected.staticError == "" {
		t.Fatal("removing a required static upstream did not retain the active configuration")
	}
	close(rejected)

	write(files.Static, strings.ReplaceAll(upstream, ":9090", ":9091"))
	updated := start()
	cfg, current := updated.Current()
	if current.Parent != dynamicRevision.ID || cfg.Upstreams["beta"].URL != "http://127.0.0.1:9091/mcp" ||
		cfg.Surfaces["readers"].Path != "/mcp/updated" || updated.staticError != "" {
		t.Fatal("static update lost the dynamic surface")
	}
}

func TestStaticConfigConflictingPromotionAndBootstrap(t *testing.T) {
	const dynamic = `regate { listen = "127.0.0.1:9090" }`
	const declared = `regate { listen = "127.0.0.1:9091" }`
	dir := t.TempDir()
	original, err := New(dir, dynamic)
	if err != nil {
		t.Fatal(err)
	}
	_, before := original.Current()
	if err := original.Store.Close(); err != nil {
		t.Fatal(err)
	}
	staticFile := filepath.Join(t.TempDir(), "static.hcl")
	if err := os.WriteFile(staticFile, []byte(declared), 0600); err != nil {
		t.Fatal(err)
	}
	files := ConfigFiles{Static: staticFile}
	conflict, err := OpenConfigFiles(context.Background(), dir, files, false)
	if err != nil {
		t.Fatal(err)
	}
	defer conflict.Store.Close()
	cfg, after := conflict.Current()
	if cfg.Source != dynamic || before != after || conflict.staticError == "" {
		t.Fatal("conflicting promotion replaced dynamic HCL")
	}
	fresh, err := OpenConfigFiles(t.Context(), t.TempDir(), files, false)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Store.Close()
	cfg, after = fresh.Current()
	if !strings.HasPrefix(cfg.Source, declared) || len(fresh.Store.History()) != 1 ||
		fresh.configView(cfg, after.ID)["dynamic_source"] != "\n" {
		t.Fatal("fresh store did not initialize from the static file")
	}
}
