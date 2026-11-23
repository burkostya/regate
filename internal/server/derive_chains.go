package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

const (
	minChainSessions     = 3
	maxChainSteps        = 5
	chainPageSize        = 100
	heuristicSessionIdle = 15 * time.Minute
)

type chainStep struct {
	Tool         string `json:"tool"`
	AllowEffects bool   `json:"allow_effects"`
}

type callChain struct {
	ID               string      `json:"id"`
	Steps            []chainStep `json:"steps"`
	Sessions         int         `json:"sessions"`
	Explicit         int         `json:"explicit_sessions"`
	Heuristic        int         `json:"heuristic_sessions"`
	ExampleRuns      [][]string  `json:"example_runs"`
	DraftUnavailable string      `json:"draft_unavailable,omitempty"`
}

type chainSearchResult struct {
	Surface  string      `json:"surface"`
	Revision string      `json:"revision"`
	Chains   []callChain `json:"chains"`
	Next     string      `json:"next_cursor,omitempty"`
}

type chainEvidence struct {
	callChain
	lastSession int
}

func observationEnd(call storage.ToolCallObservation) int64 {
	if call.FinishedNS == 0 {
		return call.StartedNS + int64(publicToolTimeout)
	}
	return call.FinishedNS
}

// Calls are sorted by group and start time. Treat overlapping intervals as a
// single break, including any calls transitively overlapping that interval.
func countChains(calls []storage.ToolCallObservation) map[[maxChainSteps]string]*chainEvidence {
	counts := make(map[[maxChainSteps]string]*chainEvidence)
	session := 0
	var priorEnd int64
	var window []storage.ToolCallObservation
	for i := 0; i < len(calls); {
		call := calls[i]
		if i == 0 || call.Group != calls[i-1].Group ||
			(!call.Explicit && call.StartedNS-priorEnd >= int64(heuristicSessionIdle)) {
			session++
			window = window[:0]
		}
		end := observationEnd(call)
		j := i + 1
		for j < len(calls) && calls[j].Group == call.Group &&
			(calls[j].StartedNS < end || calls[j].StartedNS == call.StartedNS) {
			end = max(end, observationEnd(calls[j]))
			j++
		}
		overlap := j-i > 1
		priorEnd, i = end, j
		if overlap || !call.Successful || call.FinishedNS == 0 || call.Tool == "" {
			window = window[:0]
			continue
		}
		if len(window) == maxChainSteps {
			copy(window, window[1:])
			window = window[:maxChainSteps-1]
		}
		window = append(window, call)
		for offset := 0; offset+2 <= len(window); offset++ {
			ordered := window[offset:]
			var key [maxChainSteps]string
			for k, item := range ordered {
				key[k] = item.Tool
			}
			evidence := counts[key]
			if evidence == nil {
				evidence = &chainEvidence{}
				counts[key] = evidence
			}
			if evidence.lastSession == session {
				continue
			}
			evidence.lastSession = session
			evidence.Sessions++
			if call.Explicit {
				evidence.Explicit++
			} else {
				evidence.Heuristic++
			}
			if len(evidence.ExampleRuns) < minChainSessions {
				runs := make([]string, len(ordered))
				for k, item := range ordered {
					runs[k] = item.RunID
				}
				evidence.ExampleRuns = append(evidence.ExampleRuns, runs)
			}
		}
	}
	return counts
}

func (s *Service) findCallChains(ctx context.Context, cfg *config.Config, surfaceName, revision, after, selected string) (chainSearchResult, error) {
	surface, exists := cfg.Surfaces[surfaceName]
	if !exists || !surface.DeriveRules {
		return chainSearchResult{}, fmt.Errorf("surface does not collect tool chains")
	}
	calls, err := s.Store.ToolCalls(ctx, surfaceName, revision)
	if err != nil {
		return chainSearchResult{}, fmt.Errorf("tool call observations unavailable")
	}
	prefix, err := s.Store.StaticConfigPrefix()
	if err != nil {
		return chainSearchResult{}, err
	}
	at, err := surfaceInsertion(cfg.Source, surfaceName)
	if err != nil {
		return chainSearchResult{}, err
	}
	out := chainSearchResult{Surface: surfaceName, Revision: revision, Chains: []callChain{}}
	for key, evidence := range countChains(calls) {
		if evidence.Sessions < minChainSessions {
			continue
		}
		encoded, _ := json.Marshal(key)
		evidence.ID = storage.SourceDigest(surfaceName + "\x00" + revision + "\x00" + string(encoded))
		if selected != "" && evidence.ID != selected {
			continue
		}
		for _, name := range key {
			if name == "" {
				break
			}
			evidence.Steps = append(evidence.Steps, chainStep{Tool: name})
		}
		out.Chains = append(out.Chains, evidence.callChain)
	}
	slices.SortFunc(out.Chains, func(a, b callChain) int {
		if a.Sessions != b.Sessions {
			return b.Sessions - a.Sessions
		}
		if len(a.Steps) != len(b.Steps) {
			return len(b.Steps) - len(a.Steps)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if after != "" {
		index := slices.IndexFunc(out.Chains, func(chain callChain) bool { return chain.ID == after })
		if index == -1 {
			return chainSearchResult{}, fmt.Errorf("chain cursor is no longer available; refresh the search")
		}
		out.Chains = out.Chains[index+1:]
	}
	if len(out.Chains) > chainPageSize {
		out.Chains = out.Chains[:chainPageSize]
		out.Next = out.Chains[chainPageSize-1].ID
	}
	for i := range out.Chains {
		chain := &out.Chains[i]
		for j, step := range chain.Steps {
			tool, ok := surface.FindTool(step.Tool)
			if !ok {
				chain.DraftUnavailable = "A source tool is unavailable."
				continue
			}
			effectful, suspendable := cfg.ToolTraits(surfaceName, tool)
			chain.Steps[j].AllowEffects = effectful
			if suspendable {
				chain.DraftUnavailable = "A source tool can suspend."
			}
		}
		if at < len(prefix) {
			chain.DraftUnavailable = "This surface is managed by deployment."
		}
	}
	return out, nil
}
