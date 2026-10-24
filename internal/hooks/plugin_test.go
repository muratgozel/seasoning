package hooks

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/muratgozel/seasoning/internal/inject"
)

// The plugin's hooks.json and .mcp.json are a contract with this binary
// (SPEC §15.3, §16). This test keeps them in step.
func TestPluginContract(t *testing.T) {
	var file struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command       string   `json:"command"`
				Args          []string `json:"args"`
				AsyncRewake   bool     `json:"asyncRewake"`
				RewakeMessage string   `json:"rewakeMessage"`
				Timeout       int      `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	raw, err := os.ReadFile("../../plugin/hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	proto := strconv.Itoa(1)
	seen := map[string]bool{}
	waiters := 0
	for name, groups := range file.Hooks {
		if _, ok := eventNames[toCLI(name)]; !ok {
			t.Errorf("hooks.json wires unknown event %s", name)
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command != "${CLAUDE_PLUGIN_ROOT}/bin/seasoning-run" || len(h.Args) != 4 || h.Args[0] != "hook" || h.Args[2] != "--plugin-proto" || h.Args[3] != proto {
					t.Errorf("%s: unexpected hook %+v", name, h)
					continue
				}
				if !slices.Contains(Events, h.Args[1]) {
					t.Errorf("%s calls unknown event %s", name, h.Args[1])
				}
				seen[h.Args[1]] = true
				if h.Args[1] == Wait {
					waiters++
					if !h.AsyncRewake || h.RewakeMessage != inject.RewakePrefix || h.Timeout < 86400 {
						t.Errorf("%s waiter must be asyncRewake with rewakeMessage %q and a long timeout: %+v", name, inject.RewakePrefix, h)
					}
				}
			}
		}
	}
	for _, e := range Events {
		if !seen[e] {
			t.Errorf("hooks.json never calls %s", e)
		}
	}
	if waiters != 2 {
		t.Errorf("want the waiter armed at SessionStart and Stop, found %d", waiters)
	}

	var mcp struct {
		McpServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	raw, err = os.ReadFile("../../plugin/.mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &mcp)
	s, ok := mcp.McpServers["seasoning"]
	if !ok || !slices.Equal(s.Args, []string{"mcp", "--plugin-proto", proto}) {
		t.Errorf(".mcp.json: %+v", mcp)
	}
	if info, err := os.Stat("../../plugin/bin/seasoning-run"); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("plugin/bin/seasoning-run must exist and be executable: %v", err)
	}
}

func toCLI(event string) string {
	for k, v := range eventNames {
		if v == event {
			return k
		}
	}
	return ""
}
