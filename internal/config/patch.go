package config

import "fmt"

// ReplaceBlock is a loss-minimizing patch: all bytes outside the target block
// remain exactly as authored. It intentionally refuses ambiguous targets.
func ReplaceBlock(source, typ, label, replacement string) (string, error) {
	blocks, e := Parse(source)
	if e != nil {
		return "", e
	}
	found := 0
	start, end := 0, 0
	for _, b := range blocks {
		if b.Type == typ && len(b.Labels) == 1 && b.Labels[0] == label {
			found++
			start = b.Start
			end = b.End
		}
	}
	if found != 1 {
		return "", fmt.Errorf("expected one %s %q, found %d", typ, label, found)
	}
	next := source[:start] + replacement + source[end:]
	if _, e := Decode(next); e != nil {
		return "", e
	}
	return next, nil
}
