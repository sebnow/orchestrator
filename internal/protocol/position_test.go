package protocol_test

import (
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenAPositionWhenWrittenAndParsedThenItIsTheSame(t *testing.T) {
	for _, position := range []protocol.CommandPosition{
		{Epoch: "6ca6c64cdf8e8f0e4554ab8bd473e063", ID: 42},
		{Epoch: "E1", ID: 0},
		{ID: 7},
	} {
		got, err := protocol.ParseCommandPosition(position.String())
		if err != nil || got != position {
			t.Errorf("%q parsed as %+v, %v; want %+v", position.String(), got, err, position)
		}
	}
}

func TestGivenABareIDWhenParsedThenThePositionHasNoEpoch(t *testing.T) {
	got, err := protocol.ParseCommandPosition("12")

	if err != nil || got != (protocol.CommandPosition{ID: 12}) {
		t.Errorf("parsed %+v, %v; want id 12 without an epoch", got, err)
	}
}

func TestGivenAMalformedPositionWhenParsedThenItIsRefused(t *testing.T) {
	for _, raw := range []string{"", "seven", ":5", "e1:", "e1:-1", "e:1:2", "e-1:3", "e1:9223372036854775808"} {
		if got, err := protocol.ParseCommandPosition(raw); err == nil {
			t.Errorf("%q parsed as %+v, want an error", raw, got)
		}
	}
}
