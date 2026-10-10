package claude

import (
	"reflect"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenToolClassesWhenBuildingTheArgumentsThenToolsNamesOnlyTheirBuiltInTools(t *testing.T) {
	for name, tc := range map[string]struct {
		classes []string
		want    []string
	}{
		"read and web":     {[]string{protocol.ToolClassWeb, protocol.ToolClassRead}, []string{"Read,Grep,Glob,WebSearch,WebFetch"}},
		"every class":      {protocol.ToolClasses, []string{"Read,Grep,Glob,Edit,Write,NotebookEdit,Bash,WebSearch,WebFetch,Agent,Task"}},
		"only mcp":         {[]string{protocol.ToolClassMCP}, []string{""}},
		"no restriction":   {nil, nil},
		"empty is no list": {[]string{}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			spec := commandSpec
			spec.ToolClasses = tc.classes

			args, err := arguments(spec, "")

			if err != nil || !slices.Equal(flagValues(args, "--tools"), tc.want) {
				t.Errorf("--tools = %q (%v), want %q", flagValues(args, "--tools"), err, tc.want)
			}
		})
	}
}

func TestGivenToolsTheHarnessOffersWhenCheckedThenUnclassifiedAndExcessToolsAreNamedAndTheGatewaysAreAlwaysAllowed(t *testing.T) {
	offered := []string{"Read", "Task", "Monitor", "mcp__orchestrator__spawn_task", "mcp__other__lookup", "WebFetch", "ToolSearch"}

	restricted := checkTools(offered, []string{protocol.ToolClassRead, protocol.ToolClassWeb})
	unrestricted := checkTools(offered, nil)
	held := checkTools([]string{"Glob", "Grep", "Read", "WebFetch", "WebSearch", "mcp__orchestrator__permission"}, []string{protocol.ToolClassRead, protocol.ToolClassWeb})

	want := harness.ToolCheck{Restricted: true, Unclassified: []string{"Monitor", "ToolSearch"}, Excess: []string{"Task", "mcp__other__lookup"}}
	if !reflect.DeepEqual(restricted, want) || restricted.Enforceable() {
		t.Errorf("restricted check = %+v, want %+v, not enforceable", restricted, want)
	}
	if want := (harness.ToolCheck{Unclassified: []string{"Monitor", "ToolSearch"}}); !reflect.DeepEqual(unrestricted, want) || !unrestricted.Enforceable() {
		t.Errorf("unrestricted check = %+v, want %+v, enforceable", unrestricted, want)
	}
	if !held.Enforceable() || len(held.Unclassified) != 0 || len(held.Excess) != 0 {
		t.Errorf("check of exactly the allowed tools = %+v, want it to hold", held)
	}
}

func TestGivenProcessOutputWhenItsFirstInitArrivesThenOnlyThatLineCarriesTheToolCheck(t *testing.T) {
	tr := turn{classes: []string{protocol.ToolClassRead}}
	init := []byte(`{"type":"system","subtype":"init","session_id":"s1","tools":["Read","Bash"]}`)

	first := tr.classify(init)
	second := tr.classify(init)

	if first.Tools == nil || !reflect.DeepEqual(first.Tools.Excess, []string{"Bash"}) {
		t.Errorf("first init tools = %+v, want Bash in excess", first.Tools)
	}
	if second.Tools != nil {
		t.Errorf("second init tools = %+v, want none", second.Tools)
	}
}
