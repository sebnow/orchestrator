package server

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// denyAll denies every request, saying why.
type denyAll struct{}

func (denyAll) Decide(protocol.TaskID, protocol.PermissionRequested) Decision {
	return Decision{Verdict: VerdictDeny, Message: "not on this server"}
}

func lastCommand(t *testing.T, srv testServer, daemon protocol.DaemonID) protocol.Command {
	t.Helper()
	commands, err := srv.store.commandsAfter(t.Context(), daemon, 0)
	if err != nil {
		t.Fatal(err)
	}
	return commands[len(commands)-1]
}

func TestGivenAllowAllWhenAPermissionRequestIsStoredThenTheServerAllowsItAndTheTranscriptSaysByPolicy(t *testing.T) {
	srv := startTestServerWith(t, Options{Permissions: AllowAll{}})
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)

	events.ingest(t, srv, "laptop")

	last := lastCommand(t, srv, "laptop")
	if last.Kind != protocol.CommandAnswerPermission || string(last.Payload) != `{"request_id":"toolu_01KYWDtRqQK6PRLSQzHm7bag","allow":true}` {
		t.Errorf("last command = %s %s, want the policy's allowance", last.Kind, last.Payload)
	}
	if p := readProgress(t, srv.store, task); p.State != TaskRunning {
		t.Errorf("state = %s, want running", p.State)
	}
	page := getPage(t, srv.url+"/tasks/"+string(task))
	requireContains(t, page, "Agent asked to run Bash; request toolu_01KYWDtRqQK6PRLSQzHm7bag allowed by policy")
	requireLacks(t, page, `<h2>Permission requested</h2>`, "Owner allowed")
}

func TestGivenAllowAllWhenARequestIsPostedAgainThenItIsAnsweredOnce(t *testing.T) {
	srv := startTestServerWith(t, Options{Permissions: AllowAll{}})
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)
	events.ingest(t, srv, "laptop")

	postEvents(t, srv, "laptop", events.events...)

	commands, err := srv.store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	answers := 0
	for _, command := range commands {
		if command.Kind == protocol.CommandAnswerPermission {
			answers++
		}
	}
	if answers != 1 {
		t.Errorf("answers = %d, want 1", answers)
	}
}

func TestGivenAllowAllWhenTheBatchAlsoEndsTheTaskThenNothingIsAnswered(t *testing.T) {
	srv := startTestServerWith(t, Options{Permissions: AllowAll{}})
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)
	events.add(protocol.KindHarnessExited, `{"exit_code":1,"error":"crashed"}`)

	events.ingest(t, srv, "laptop")

	if last := lastCommand(t, srv, "laptop"); last.Kind != protocol.CommandStartTask {
		t.Errorf("last command = %s %s, want only the start", last.Kind, last.Payload)
	}
	if p := readProgress(t, srv.store, task); p.State != TaskFailed {
		t.Errorf("state = %s, want failed", p.State)
	}
}

func TestGivenADenyingPolicyWhenAPermissionRequestIsStoredThenTheServerDeniesItWithTheMessage(t *testing.T) {
	srv := startTestServerWith(t, Options{Permissions: denyAll{}})
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)

	events.ingest(t, srv, "laptop")

	last := lastCommand(t, srv, "laptop")
	if last.Kind != protocol.CommandAnswerPermission || string(last.Payload) != `{"request_id":"toolu_01KYWDtRqQK6PRLSQzHm7bag","allow":false,"message":"not on this server"}` {
		t.Errorf("last command = %s %s, want the policy's denial", last.Kind, last.Payload)
	}
	page := getPage(t, srv.url+"/tasks/"+string(task))
	requireContains(t, page, "Agent asked to run Bash; request toolu_01KYWDtRqQK6PRLSQzHm7bag denied by policy", "not on this server")
}

func TestGivenAskOwnerWhenAPermissionRequestIsStoredThenItWaitsForTheOwnerWhoseAnswerSaysOwner(t *testing.T) {
	srv := startTestServerWith(t, Options{Permissions: AskOwner{}})
	task := startTaskViaForm(t, srv, "laptop", "Touch two files")
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindPermissionRequested, bashRequest)

	events.ingest(t, srv, "laptop")

	if last := lastCommand(t, srv, "laptop"); last.Kind != protocol.CommandStartTask {
		t.Errorf("last command = %s %s, want only the start", last.Kind, last.Payload)
	}
	if p := readProgress(t, srv.store, task); p.State != TaskAwaitingPermission {
		t.Errorf("state = %s, want awaiting_permission", p.State)
	}
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(task)), `<h2>Permission requested</h2>`)

	postForm(t, srv, task, url.Values{"kind": {"answer_permission"}, "request_id": {"toolu_01KYWDtRqQK6PRLSQzHm7bag"}, "decision": {"allow"}})

	page := getPage(t, srv.url+"/tasks/"+string(task))
	requireContains(t, page, "Owner allowed request toolu_01KYWDtRqQK6PRLSQzHm7bag")
	requireLacks(t, page, "by policy", `<h2>Permission requested</h2>`)
}

const remoteAgentRequest = `{"request_id":"toolu_01RemoteAgentRequest","tool":"Agent",` +
	`"input":{"description":"Count files","prompt":"Count the files in this directory.","isolation":"remote"}}`

func lastAnswer(t *testing.T, srv testServer) (protocol.Command, protocol.AnswerPermission) {
	t.Helper()
	last := lastCommand(t, srv, "laptop")
	var answer protocol.AnswerPermission
	if last.Kind == protocol.CommandAnswerPermission {
		if err := json.Unmarshal(last.Payload, &answer); err != nil {
			t.Fatal(err)
		}
	}
	return last, answer
}

func TestGivenAnyPolicyWhenTheAgentAsksToRunASubagentRemotelyThenTheServerDeniesItAndSaysToRunItLocally(t *testing.T) {
	for name, policy := range map[string]Policy{"allow all": AllowAll{}, "ask the owner": AskOwner{}, "no policy": nil} {
		t.Run(name, func(t *testing.T) {
			srv := startTestServerWith(t, Options{Permissions: policy})
			task := startTaskViaForm(t, srv, "laptop", "Count the files")
			events := &taskEvents{task: task}
			events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
			events.add(protocol.KindPermissionRequested, remoteAgentRequest)

			events.ingest(t, srv, "laptop")

			last, answer := lastAnswer(t, srv)
			want := protocol.AnswerPermission{RequestID: "toolu_01RemoteAgentRequest", Message: claude.RemoteSubagentDenial}
			if last.Kind != protocol.CommandAnswerPermission || answer != want {
				t.Errorf("last command = %s %s, want the denial %+v", last.Kind, last.Payload, want)
			}
			if p := readProgress(t, srv.store, task); p.State != TaskRunning {
				t.Errorf("state = %s, want running", p.State)
			}
			requireContains(t, getPage(t, srv.url+"/tasks/"+string(task)), "Agent asked to run Agent; request toolu_01RemoteAgentRequest denied by policy")
		})
	}
}

func TestGivenAllowAllWhenTheAgentAsksToRunASubagentLocallyThenTheServerAllowsIt(t *testing.T) {
	for name, input := range map[string]string{
		"no isolation": `{"description":"Count files","prompt":"Count the files in this directory."}`,
		"a worktree":   `{"description":"Count files","prompt":"Count the files in this directory.","isolation":"worktree"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := startTestServerWith(t, Options{Permissions: AllowAll{}})
			task := startTaskViaForm(t, srv, "laptop", "Count the files")
			events := &taskEvents{task: task}
			events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
			events.add(protocol.KindPermissionRequested, `{"request_id":"toolu_01LocalAgentRequest","tool":"Agent","input":`+input+`}`)

			events.ingest(t, srv, "laptop")

			last, answer := lastAnswer(t, srv)
			if want := (protocol.AnswerPermission{RequestID: "toolu_01LocalAgentRequest", Allow: true}); last.Kind != protocol.CommandAnswerPermission || answer != want {
				t.Errorf("last command = %s %s, want the allowance %+v", last.Kind, last.Payload, want)
			}
		})
	}
}
