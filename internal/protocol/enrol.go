package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// EnrolPath is the server's enrolment route: the one route a daemon
// calls without a client certificate, to get its first one
// (docs/adr/2026-10-10-vps-provisioning.md).
const EnrolPath = "/v1/enrol"

// EnrolmentToken lets the holder enrol once as Daemon, before it
// expires. It is written "<daemon>:<secret>": a daemon id never holds a
// ':', so the first one separates the two.
type EnrolmentToken struct {
	Daemon DaemonID
	Secret string
}

var ErrInvalidEnrolmentToken = errors.New(`invalid enrolment token, want "<daemon id>:<secret>"`)

func ParseEnrolmentToken(raw string) (EnrolmentToken, error) {
	id, secret, found := strings.Cut(strings.TrimSpace(raw), ":")
	if !found || secret == "" {
		return EnrolmentToken{}, ErrInvalidEnrolmentToken
	}
	daemon, err := ParseDaemonID(id)
	if err != nil {
		return EnrolmentToken{}, fmt.Errorf("%w: %w", ErrInvalidEnrolmentToken, err)
	}
	return EnrolmentToken{Daemon: daemon, Secret: secret}, nil
}

func (t EnrolmentToken) String() string {
	return string(t.Daemon) + ":" + t.Secret
}

// Enrolment is what a daemon POSTs to EnrolPath: its token and a
// PEM-encoded certificate signing request whose Common Name is the
// token's daemon id.
type Enrolment struct {
	Token string `json:"token"`
	CSR   string `json:"csr"`
}

// Enrolled is the server's answer to an Enrolment: the daemon's
// certificate and the CA certificate, both PEM-encoded.
type Enrolled struct {
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
}
