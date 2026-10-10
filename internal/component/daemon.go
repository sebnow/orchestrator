package component

import (
	"maps"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
)

func daemonURL(id string) string { return "/daemons/" + url.PathEscape(id) }

// LabelList shows labels as key=value pairs in key order, or says there
// are none.
func LabelList(labels map[string]string) html.Node {
	if len(labels) == 0 {
		return html.Text("none")
	}
	nodes := make([]html.Node, 0, 2*len(labels))
	for idx, key := range slices.Sorted(maps.Keys(labels)) {
		if idx > 0 {
			nodes = append(nodes, html.Text(" "))
		}
		nodes = append(nodes, html.El("code", nil, html.Text(key+"="+labels[key])))
	}
	return html.Fragment(nodes...)
}

// DaemonLabels shows what the daemon reported, what the owner set, and
// what placement matches against: both merged, the owner's winning.
func DaemonLabels(id string, facts, labels, merged map[string]string) html.Node {
	term := func(name string, value html.Node) html.Node {
		return html.Fragment(html.El("dt", nil, html.Text(name)), html.El("dd", nil, value))
	}
	return html.El("dl", attrs("class", "labels"),
		term("Facts it reported", LabelList(facts)),
		term("Labels the owner set", LabelList(labels)),
		term("Matched against what a task requires", LabelList(merged)),
	)
}

// LabelsForm sets the labels of daemon id to those written in text.
// problem, when set, says why the last submission was refused.
func LabelsForm(id, text, problem string) html.Node {
	return PlainForm(daemonURL(id)+"/labels", problem,
		Field(FieldSpec{Name: "labels", Label: "Labels (key=value, separated by commas); they win over facts with the same key", Value: text}),
		Button("Save labels", VariantPrimary, "", ""),
	)
}

// DaemonSSHKey shows line, the authorized_keys line of the key the
// daemon pushes with, for the owner to register at the forge
// (docs/adr/2026-10-10-daemon-push-identity.md), or says that the daemon
// has reported none.
func DaemonSSHKey(line string) html.Node {
	if line == "" {
		return html.El("p", attrs("class", "empty"), html.Text("The daemon has not reported a key."))
	}
	return html.Fragment(
		html.El("p", nil, html.Text("The daemon pushes task branches with this key. Register it at the forge: as a deploy key with write access on each repository its tasks use, or on a machine user with access to them.")),
		html.El("pre", attrs("class", "ssh-key"), html.El("code", nil, html.Text(line))),
	)
}

// DaemonLoginView is what a daemon page shows of its harness's login
// (docs/adr/2026-10-10-harness-login.md): the login facts it reported,
// empty when it reported none, and its latest login from the server:
// Phase is "", "requested", "started", "code_sent" or "finished". URL is
// where a started login is authorised; a finished one is OK or failed
// with Error. Problem, when set, says why the owner's last request was
// refused.
type DaemonLoginView struct {
	ID                     string
	Connected              bool
	Login, Method, Account string
	Phase                  string
	URL                    string
	OK                     bool
	Error                  string
	Problem                string
}

// DaemonLoginSection is a daemon page's login section, which server-sent
// events keep current.
func DaemonLoginSection(view DaemonLoginView) html.Node {
	return html.El("section", attrs("id", "login"),
		html.El("h2", nil, html.Text("Login")),
		RegionOf(RegionDaemonLogin, DaemonLogin(view)),
		html.El("div", attrs("hx-ext", "sse", "sse-connect", daemonURL(view.ID)+"/stream",
			"sse-swap", "message", "hx-target", "#"+string(RegionDaemonLogin), "hx-swap", "innerHTML")),
	)
}

// DaemonLogin is the content of a daemon page's login section: whether
// the harness is logged in, how the latest login goes, and the forms
// that start a login and give it its code.
func DaemonLogin(view DaemonLoginView) html.Node {
	var status html.Node
	switch view.Login {
	case protocol.LoginYes:
		as := html.Text("")
		if view.Account != "" {
			as = html.Fragment(html.Text(" as "), html.El("code", nil, html.Text(view.Account)))
		}
		status = html.El("p", attrs("class", "login-status"), html.Text("Logged in, by "+view.Method), as, html.Text("."))
	case protocol.LoginNo:
		status = html.El("p", attrs("class", "login-status"), badge("login needed", "login-needed"),
			html.Text(" The harness reports that it is not logged in; no turn is placed here until it is."))
	default:
		status = html.El("p", attrs("class", "login-status empty"), html.Text("The daemon has not reported its login."))
	}
	var progress html.Node
	switch view.Phase {
	case "requested":
		progress = html.El("p", attrs("class", "notice login-progress"), html.Text("Waiting for the daemon to start the login."))
	case "started":
		// Claude Code opens a browser itself when the daemon's machine
		// has one, and its local callback completes the login without a
		// code; the code is for a machine without one.
		progress = html.Fragment(
			html.El("p", attrs("class", "login-progress"),
				html.Text("If a browser opened on the daemon's machine, authorise there. Otherwise "),
				html.El("a", attrs("href", view.URL, "target", "_blank", "rel", "noopener noreferrer", "class", "login-url"), html.Text("open this link")),
				html.Text(", authorise, and paste the code shown.")),
			Form(daemonURL(view.ID)+"/login/code", "",
				Field(FieldSpec{Kind: FieldPassword, Name: "code", Label: "Code", Required: true}),
				Button("Submit code", VariantPrimary, "", "")),
		)
	case "code_sent":
		progress = html.El("p", attrs("class", "notice login-progress"), html.Text("Code sent; waiting for the harness to finish the login."))
	case "finished":
		switch {
		case view.OK && view.Account != "":
			progress = html.El("p", attrs("class", "notice login-progress login-ok"),
				html.Text("The login succeeded: logged in as "), html.El("code", nil, html.Text(view.Account)), html.Text("."))
		case view.OK:
			progress = html.El("p", attrs("class", "notice login-progress login-ok"),
				html.Text("The login succeeded; the harness reports no account yet."))
		default:
			progress = html.El("p", attrs("class", "problem login-progress login-failed"), html.Text("The login failed: "+view.Error))
		}
	}
	var start html.Node
	if view.Connected {
		label := "Log in"
		if view.Phase == "requested" || view.Phase == "started" || view.Phase == "code_sent" {
			label = "Start again"
		}
		start = Form(daemonURL(view.ID)+"/login", view.Problem, Button(label, VariantPlain, "", ""))
	} else {
		start = html.Fragment(problemNote(view.Problem),
			html.El("p", attrs("class", "empty"), html.Text("The daemon is not connected; log it in once it is.")))
	}
	return html.Fragment(status, progress, start)
}

// VPS is a VPS the server provisioned, as the GUI shows it: State is
// creating, enrolled, connected, destroying or destroyed, and ServerID
// is 0 until Hetzner answered.
type VPS struct {
	Daemon     string
	ServerID   int64
	ServerType string
	Location   string
	CreatedAt  time.Time
	State      string
}

// VPSColumns head a Table of VPSRows.
var VPSColumns = []string{"Daemon", "Hetzner server", "Type", "Location", "Created", "State", ""}

// VPSRow is a VPS in the VPS list, with the button that destroys it
// unless it is destroyed.
func VPSRow(vps VPS) html.Node {
	server := html.Text("not yet created")
	if vps.ServerID != 0 {
		server = html.Text(strconv.FormatInt(vps.ServerID, 10))
	}
	var destroy html.Node
	if vps.State != "destroyed" {
		destroy = PlainForm(daemonURL(vps.Daemon)+"/destroy", "", Button("Destroy", VariantDanger, "", ""))
	}
	return html.El("tr", attrs("class", "vps"),
		cell(html.Text(vps.Daemon)),
		cell(server),
		cell(html.Text(vps.ServerType)),
		cell(html.Text(vps.Location)),
		cell(timestamp(vps.CreatedAt)),
		cell(html.El("span", attrs("class", "vps-state"), html.Text(vps.State))),
		cell(destroy),
	)
}

// Provision is the button that provisions a VPS of serverType in
// location, or, when enabled is false, a note that provisioning is off.
func Provision(enabled bool, serverType, location string) html.Node {
	if !enabled {
		return html.El("p", attrs("class", "empty"), html.Text("Provisioning is off: the server was started without a Hetzner token."))
	}
	return PlainForm("/vpses", "",
		html.El("p", nil, html.Text("A new VPS is a "+serverType+" in "+location+", billed by the hour until it is destroyed. Its daemon enrols and connects by itself; log it in once it shows here.")),
		Button("Provision a VPS", VariantPrimary, "", ""))
}

// FailurePage says that what the owner asked for under heading failed,
// and why, with a way back to the dashboard.
func FailurePage(heading, problem string) html.Node {
	return Page(heading, Section(heading, problemNote(problem), html.El("p", nil, link("/", "Back to the dashboard"))))
}
