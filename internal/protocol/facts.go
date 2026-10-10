package protocol

// Facts are what a daemon detects about its machine and reports with
// PUT /v1/daemons/{daemon}/facts each time it opens its command stream,
// and again whenever they change
// (docs/adr/2026-10-09-agents-and-placement.md). The body is a JSON
// object of string values, which replaces the facts the daemon reported
// before. The server matches the labels a task requires against them,
// and against the labels the owner sets, which win where both set a key.
type Facts map[string]string

// The facts a daemon reports. A fact the daemon cannot detect is left
// out.
const (
	// FactOS and FactArch are the operating system and architecture the
	// daemon was built for, as Go names them: "linux", "darwin"; "amd64",
	// "arm64".
	FactOS   = "os"
	FactArch = "arch"
	// FactCPUs is the number of logical CPUs, in decimal.
	FactCPUs = "cpus"
	// FactMemory is the machine's memory in bytes, in decimal.
	FactMemory = "memory"
	// FactHarness and FactHarnessVersion name the harness the daemon runs
	// and its version, as events carry them.
	FactHarness        = "harness"
	FactHarnessVersion = "harness_version"
	// FactGPU is "nvidia" when nvidia-smi is on the daemon's PATH, and
	// "apple" on darwin/arm64.
	FactGPU = "gpu"
	// FactSSHPublicKey is the public half of the ed25519 key the daemon
	// pushes with (docs/adr/2026-10-10-daemon-push-identity.md): the
	// base64 of its ssh wire encoding, the second field of its
	// authorized_keys line. Its line is "ssh-ed25519 <value>".
	FactSSHPublicKey = "ssh_public_key"
	// FactModels names the models the daemon's harness provides, each as
	// the harness accepts it, separated by ';'
	// (docs/adr/2026-10-10-agent-models-and-capacity.md). The owner's
	// label of the same key adds models to it rather than replacing it.
	// An adapter that cannot list its models, as Claude Code's cannot,
	// leaves it out.
	FactModels = "models"
	// FactSlots is how many tasks the daemon can run at once, in decimal,
	// as it derives it from the memory its machine has available and its
	// load average (docs/adr/2026-10-10-agent-models-and-capacity.md).
	// The daemon reports it again whenever it changes: on a timer, and
	// when a harness starts or exits. An owner's label of the same key
	// caps it.
	FactSlots = "slots"
	// FactLogin is "yes" when the harness reports itself logged in and
	// "no" when it does not; FactLoginMethod is how it is logged in, as
	// the harness names it, such as Claude Code's "claude.ai" or "none"
	// (docs/adr/2026-10-10-harness-login.md). The daemon reports them
	// when it connects, after each login, and every ten minutes. The
	// server places no turn on a daemon whose login is "no". A harness
	// that cannot report its login leaves both out.
	FactLogin       = "login"
	FactLoginMethod = "login_method"
	// FactAccount names the account the harness is logged in to, as the
	// harness adapter derives it from what the harness reports; for
	// Claude Code, the email address and the organisation id, joined by
	// '/'. Quota readings are kept per harness and account. It is left
	// out when the harness reports none, or one that is not a valid
	// label value.
	FactAccount = "account"
)

// Login values of FactLogin.
const (
	LoginYes = "yes"
	LoginNo  = "no"
)

const maxLabelValue = 255

// ValidLabelValue reports whether value can be a fact or a label's
// value: 1 to 255 printable ASCII characters other than space, ',' and
// '=', which separate labels when they are written as text.
func ValidLabelValue(value string) bool {
	if value == "" || len(value) > maxLabelValue {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r > '~' || r == ',' || r == '=' {
			return false
		}
	}
	return true
}
