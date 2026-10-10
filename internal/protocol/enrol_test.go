package protocol_test

import (
	"errors"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenTokenWhenParsedThenTheDaemonAndSecretSplitAtTheFirstColon(t *testing.T) {
	token, err := protocol.ParseEnrolmentToken("vps-ab12:c2VjcmV0:x\n")
	if err != nil {
		t.Fatal(err)
	}
	if token.Daemon != "vps-ab12" || token.Secret != "c2VjcmV0:x" {
		t.Errorf("token = %+v", token)
	}
	if token.String() != "vps-ab12:c2VjcmV0:x" {
		t.Errorf("String() = %q", token.String())
	}
}

func TestGivenMalformedTokenWhenParsedThenItIsRefused(t *testing.T) {
	for _, raw := range []string{"", "vps-ab12", "vps-ab12:", ":secret", "../x:secret"} {
		if _, err := protocol.ParseEnrolmentToken(raw); !errors.Is(err, protocol.ErrInvalidEnrolmentToken) {
			t.Errorf("%q: err = %v, want ErrInvalidEnrolmentToken", raw, err)
		}
	}
}
