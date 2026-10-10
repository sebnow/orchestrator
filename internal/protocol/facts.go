package protocol

// Facts are what a daemon detects about its machine and reports with
// PUT /v1/daemons/{daemon}/facts each time it opens its command stream
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
)
