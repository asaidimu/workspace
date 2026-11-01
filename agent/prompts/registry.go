package prompts

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed definitions/*.json
var definitions embed.FS

// Set is a loaded collection of prompt definitions. Safe for concurrent reads.
type Set struct {
	byName map[string]Prompt
}

func (s *Set) Get(name string) (Prompt, bool) {
	p, ok := s.byName[name]
	return p, ok
}

func (s *Set) List() []Prompt {
	result := make([]Prompt, 0, len(s.byName))
	for _, p := range s.byName {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Registry is the process-wide registry, loaded once at package init.
var Registry = load()

func load() *Set {
	entries, err := fs.ReadDir(definitions, "definitions")
	if err != nil {
		panic(fmt.Sprintf("prompts: read definitions: %v", err))
	}

	s := &Set{byName: make(map[string]Prompt, len(entries))}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := definitions.ReadFile("definitions/" + entry.Name())
		if err != nil {
			panic(fmt.Sprintf("prompts: read %s: %v", entry.Name(), err))
		}

		var p Prompt
		if err := json.Unmarshal(data, &p); err != nil {
			panic(fmt.Sprintf("prompts: decode %s: %v", entry.Name(), err))
		}

		if _, dup := s.byName[p.Name]; dup {
			panic(fmt.Sprintf("prompts: duplicate prompt name: %s", p.Name))
		}

		s.byName[p.Name] = p
	}
	return s
}
