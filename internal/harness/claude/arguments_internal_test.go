package claude

import (
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// flagValues returns the value after each occurrence of flag in args.
func flagValues(args []string, flag string) []string {
	var values []string
	for idx, arg := range args {
		if arg == flag && idx+1 < len(args) {
			values = append(values, args[idx+1])
		}
	}
	return values
}

func TestGivenEachNeutralEffortWhenBuildingTheArgumentsThenClaudeGetsItsOwnLevel(t *testing.T) {
	for effort, want := range map[string]string{
		protocol.EffortLow: "low", protocol.EffortMedium: "medium", protocol.EffortHigh: "high", protocol.EffortMax: "max",
	} {
		spec := commandSpec
		spec.Effort = effort

		args, err := arguments(spec, "")

		if err != nil || !slices.Equal(flagValues(args, "--effort"), []string{want}) {
			t.Errorf("effort %s: args = %q, %v; want --effort %s once", effort, args, err, want)
		}
	}
}

func TestGivenNoEffortWhenBuildingTheArgumentsThenClaudeKeepsItsDefault(t *testing.T) {
	args, err := arguments(commandSpec, "")

	if err != nil || slices.Contains(args, "--effort") {
		t.Errorf("args = %q, %v; want no --effort", args, err)
	}
}

func TestGivenEffortOutsideTheNeutralScaleWhenBuildingTheArgumentsThenItIsRefused(t *testing.T) {
	spec := commandSpec
	spec.Effort = "xhigh"

	if args, err := arguments(spec, ""); err == nil {
		t.Errorf("args = %q, want an error", args)
	}
}
