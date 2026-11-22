package server

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/burkostya/regate/internal/config"
)

type ConfigFiles struct {
	Initial string
	Static  string
}

// OpenConfigFiles reads the bootstrap HCL only for a fresh store. Leaving
// Static unset preserves the last deployed layer. An empty file removes it.
func OpenConfigFiles(ctx context.Context, stateDir string, files ConfigFiles, wait bool) (*Service, error) {
	readInitial := func() (string, error) {
		if files.Initial == "" {
			return "\n", nil
		}
		source, err := os.ReadFile(files.Initial)
		return string(source), err
	}
	var readStatic func() (string, error)
	if files.Static != "" {
		readStatic = sync.OnceValues(func() (string, error) {
			source, err := os.ReadFile(files.Static)
			if err != nil {
				return "", errors.New("static HCL file could not be read")
			}
			return string(source), nil
		})
	}
	return newService(ctx, stateDir, readInitial, readStatic, wait)
}

func bootstrapSource(readInitial, readStatic func() (string, error)) (string, string, error) {
	dynamic, err := readInitial()
	if err != nil {
		return "", "", err
	}
	var static string
	if readStatic != nil {
		static, err = readStatic()
		if err != nil {
			return "", "", err
		}
	}
	source, prefix, err := config.MergeStaticConfig(dynamic, static)
	if err != nil {
		return "", "", err
	}
	if _, err := config.Decode(source); err != nil {
		if readStatic != nil {
			return "", "", errors.New("initial combined HCL did not validate")
		}
		return "", "", err
	}
	return source, prefix, nil
}

// applyStaticConfig runs at startup under the execution lease, before the
// HTTP listener and the queue dispatcher start.
func (s *Service) applyStaticConfig(readStatic func() (string, error)) error {
	static, err := readStatic()
	if err != nil {
		s.rejectStaticConfig(err.Error())
		return nil
	}
	if static == "" && s.staticPrefix == "" || static != "" && static+"\n\n" == s.staticPrefix {
		return nil
	}
	dynamic := strings.TrimPrefix(s.cfg.Source, s.staticPrefix)
	source, prefix, err := config.MergeStaticConfig(dynamic, static)
	if err != nil {
		s.rejectStaticConfig(err.Error())
		return nil
	}
	cfg, err := config.Decode(source)
	if err != nil {
		s.rejectStaticConfig("combined HCL did not validate")
		return nil
	}
	if cfg.MaxQueuedTasks > 0 && !s.Store.HasTaskQueueKeys() {
		s.rejectStaticConfig("queue keys required by combined HCL are unavailable")
		return nil
	}
	next, revision, err := s.inbound.prepare(s, cfg)
	if err != nil {
		s.rejectStaticConfig("combined HCL could not prepare the MCP catalog")
		return nil
	}
	rev, err := s.Store.ActivateStaticConfig(s.rev.ID, source, prefix)
	if err != nil {
		return err
	}
	*revision = rev.ID
	s.inbound.publish(next)
	s.cfg, s.rev, s.staticPrefix = cfg, rev, prefix
	return nil
}

func (s *Service) rejectStaticConfig(reason string) {
	s.staticError = reason
	log.Printf("Regate kept the active revision after rejecting static configuration: %s", reason)
}

func (s *Service) configView(cfg *config.Config, revision string) map[string]any {
	return map[string]any{
		"revision": revision, "source": cfg.Source, "static_prefix": s.staticPrefix,
		"dynamic_source": strings.TrimPrefix(cfg.Source, s.staticPrefix),
	}
}
