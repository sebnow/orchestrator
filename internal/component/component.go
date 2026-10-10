// Package component renders the owner's GUI from typed data: abstract
// building blocks (Page, Section, Card, Table, Form, ...) and the domain
// components made from them. It is the only package that calls html.El,
// and the only one, besides package html, that calls html.Raw.
//
// Forms POST to the server. Without JavaScript the browser follows the
// response, a redirect or a page. With htmx the response is a fragment of
// out-of-band swaps (OutOfBand), each replacing the content of one Region.
package component

import (
	"embed"
	"io/fs"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebnow/orchestrator/internal/html"
)

//go:embed static/*.js static/*.css
var staticFiles embed.FS

// Static holds the files the pages load from /static/.
var Static = mustSub(staticFiles, "static")

func mustSub(files fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// htmxConfig lets htmx swap 422 responses, which carry a form with its
// validation errors; other 4xx and 5xx responses are not swapped.
const htmxConfig = `{"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},` +
	`{"code":"422","swap":true},{"code":"[45]..","swap":false,"error":true}]}`

// attrs pairs up names and values.
func attrs(pairs ...string) []html.Attribute {
	list := make([]html.Attribute, 0, len(pairs)/2)
	for idx := 0; idx+1 < len(pairs); idx += 2 {
		list = append(list, html.Attr(pairs[idx], pairs[idx+1]))
	}
	return list
}

// Page is a whole document titled title, with children as its content,
// under a navigation bar that links to the dashboard, the projects, the
// agents and the settings, and logs out.
func Page(title string, children ...html.Node) html.Node {
	return document(title, html.El("nav", nil,
		html.El("div", attrs("class", "links"),
			html.El("a", attrs("href", "/"), html.Text("Orchestrator")),
			html.El("a", attrs("href", ProjectsURL), html.Text("Projects")),
			html.El("a", attrs("href", AgentsURL), html.Text("Agents")),
			html.El("a", attrs("href", SettingsURL), html.Text("Settings"))),
		html.El("form", attrs("method", "post", "action", "/logout"), Button("Log out", VariantPlain, "", "")),
	), children...)
}

// LoginPage is the page that asks for the owner's token, with problem,
// when set, saying why the last one was refused.
func LoginPage(problem string) html.Node {
	return document("Log in", html.El("nav", nil, html.Text("Orchestrator")), LoginForm(problem))
}

// LoginForm asks for the owner's token. problem, when set, is shown
// above it. It is a plain form, which htmx leaves to the browser.
func LoginForm(problem string) html.Node {
	return Section("Log in", html.El("form", attrs("method", "post", "action", "/login"),
		problemNote(problem),
		Field(FieldSpec{Kind: FieldPassword, Name: "token", Label: "Owner token", Required: true}),
		Button("Log in", VariantPrimary, "", ""),
	))
}

func document(title string, nav html.Node, children ...html.Node) html.Node {
	return html.Fragment(
		// A constant with no data in it.
		html.Raw("<!DOCTYPE html>\n"),
		html.El("html", attrs("lang", "en"),
			html.El("head", nil,
				html.El("meta", attrs("charset", "utf-8")),
				html.El("meta", attrs("name", "viewport", "content", "width=device-width, initial-scale=1")),
				html.El("meta", attrs("name", "htmx-config", "content", htmxConfig)),
				html.El("title", nil, html.Text(title+" · orchestrator")),
				html.El("link", attrs("rel", "stylesheet", "href", "/static/style.css")),
				html.El("script", attrs("src", "/static/htmx.min.js", "defer", "")),
				html.El("script", attrs("src", "/static/htmx-ext-sse.min.js", "defer", "")),
			),
			html.El("body", nil,
				nav,
				html.El("main", nil, children...),
			),
		),
	)
}

// Section is a titled part of a page.
func Section(heading string, children ...html.Node) html.Node {
	return html.El("section", nil, html.El("h2", nil, html.Text(heading)), html.Fragment(children...))
}

// Card sets its children apart from what surrounds them.
func Card(children ...html.Node) html.Node {
	return html.El("article", attrs("class", "card"), children...)
}

// Table is rows under headers, or empty when there are no rows.
func Table(headers []string, empty string, rows ...html.Node) html.Node {
	if len(rows) == 0 {
		return html.El("p", attrs("class", "empty"), html.Text(empty))
	}
	cells := make([]html.Node, len(headers))
	for idx, header := range headers {
		cells[idx] = html.El("th", attrs("scope", "col"), html.Text(header))
	}
	return html.El("table", nil,
		html.El("thead", nil, html.El("tr", nil, cells...)),
		html.El("tbody", nil, rows...))
}

// Variant is how a button stands out from the others in its form.
type Variant string

const (
	VariantPlain   Variant = "plain"
	VariantPrimary Variant = "primary"
	VariantDanger  Variant = "danger"
)

// Button submits its form, sending name=value when name is set.
func Button(label string, variant Variant, name, value string) html.Node {
	list := attrs("type", "submit", "class", string(variant))
	if name != "" {
		list = append(list, html.Attr("name", name), html.Attr("value", value))
	}
	return html.El("button", list, html.Text(label))
}

// badge is a short label; class picks its colour.
func badge(label, class string) html.Node {
	return html.El("span", attrs("class", "badge "+class), html.Text(label))
}

// PlainForm POSTs its fields to action and lets the browser follow the
// response, with or without JavaScript. problem, when set, is shown
// above them.
func PlainForm(action, problem string, children ...html.Node) html.Node {
	return html.El("form", attrs("method", "post", "action", action), problemNote(problem), html.Fragment(children...))
}

// Form POSTs its fields to action. problem, when set, is shown above
// them. With htmx the response is only out-of-band swaps, so the form
// swaps nothing itself.
func Form(action, problem string, children ...html.Node) html.Node {
	return html.El("form", attrs("method", "post", "action", action, "hx-post", action, "hx-swap", "none"),
		problemNote(problem), html.Fragment(children...))
}

// problemNote shows problem, or nothing when it is empty.
func problemNote(problem string) html.Node {
	if problem == "" {
		return nil
	}
	return html.El("p", attrs("class", "problem", "role", "alert"), html.Text(problem))
}

// FieldKind is the control a Field shows.
type FieldKind int

const (
	FieldText FieldKind = iota
	FieldTextarea
	FieldSelect
	FieldHidden
	// FieldPassword is a text input whose value the browser hides.
	FieldPassword
	// FieldCheckbox is ticked when its Value is not empty, and sends "on"
	// when ticked.
	FieldCheckbox
)

// FieldSpec describes one form field. Options are the choices of a
// FieldSelect, whose Value is the chosen one.
type FieldSpec struct {
	Kind        FieldKind
	Name        string
	Label       string
	Value       string
	Placeholder string
	Options     []Option
	Required    bool
	// Rows is a FieldTextarea's height in lines; zero is four.
	Rows int
}

// Option is one choice of a FieldSelect: Value is sent, Label shown.
type Option struct {
	Value, Label string
}

// Field is a labelled form control.
func Field(spec FieldSpec) html.Node {
	list := attrs("name", spec.Name)
	if spec.Placeholder != "" {
		list = append(list, html.Attr("placeholder", spec.Placeholder))
	}
	if spec.Required {
		list = append(list, html.Attr("required", ""))
	}
	var control html.Node
	switch spec.Kind {
	case FieldHidden:
		return html.El("input", append(list, html.Attr("type", "hidden"), html.Attr("value", spec.Value)))
	case FieldCheckbox:
		box := append(list, html.Attr("type", "checkbox"), html.Attr("value", "on"))
		if spec.Value != "" {
			box = append(box, html.Attr("checked", ""))
		}
		return html.El("label", attrs("class", "checkbox"), html.El("input", box), html.Text(" "+spec.Label))
	case FieldPassword:
		control = html.El("input", append(list, html.Attr("type", "password"), html.Attr("value", spec.Value)))
	case FieldTextarea:
		// A newline right after <textarea> is dropped by the parser, so
		// one is written to keep a value's own leading newline.
		rows := spec.Rows
		if rows == 0 {
			rows = 4
		}
		control = html.El("textarea", append(list, html.Attr("rows", strconv.Itoa(rows))), html.Text("\n"+spec.Value))
	case FieldSelect:
		options := make([]html.Node, len(spec.Options))
		for idx, option := range spec.Options {
			optionAttrs := attrs("value", option.Value)
			if option.Value == spec.Value {
				optionAttrs = append(optionAttrs, html.Attr("selected", ""))
			}
			options[idx] = html.El("option", optionAttrs, html.Text(option.Label))
		}
		control = html.El("select", list, options...)
	default:
		control = html.El("input", append(list, html.Attr("type", "text"), html.Attr("value", spec.Value)))
	}
	return html.El("label", nil, html.El("span", nil, html.Text(spec.Label)), control)
}

// Details folds children away under summary.
func Details(summary string, children ...html.Node) html.Node {
	return html.El("details", nil, html.El("summary", nil, html.Text(summary)), html.Fragment(children...))
}

// Region names a part of a page that a response may replace.
type Region string

const (
	RegionDashboard    Region = "dashboard"
	RegionNewTask      Region = "new-task"
	RegionTaskHeader   Region = "task-header"
	RegionPermission   Region = "permission"
	RegionPrompt       Region = "prompt"
	RegionPromptSubmit Region = "prompt-submit"
	// RegionLive holds what keeps a task page up to date.
	RegionLive Region = "task-live"
	// RegionFallback holds what takes over when server-sent events fail.
	RegionFallback Region = "task-fallback"
	// RegionUnknown says how many unrecognised transcript entries a task
	// page hides or shows.
	RegionUnknown Region = "task-unknown"
	// RegionChildren lists the tasks a task spawned.
	RegionChildren Region = "task-children"
	// RegionTree shows the tree of tasks rooted at a task.
	RegionTree Region = "task-tree"
	// RegionQueued lists a task's prompts that have not reached its
	// harness yet.
	RegionQueued Region = "task-queued"
	// RegionDaemonLogin is a daemon page's login section.
	RegionDaemonLogin Region = "daemon-login"
)

// RegionOf is region with children as its content.
func RegionOf(region Region, children ...html.Node) html.Node {
	return html.El("div", attrs("id", string(region)), children...)
}

// Refreshing is region with children as its content, fetched again from
// url, a page containing the region, every five seconds.
func Refreshing(region Region, url string, children ...html.Node) html.Node {
	return html.El("div", attrs("id", string(region), "hx-get", url, "hx-trigger", "every 5s",
		"hx-select", "#"+string(region), "hx-swap", "outerHTML"), children...)
}

// OutOfBand replaces the content of region with children, wherever it
// is on the page.
func OutOfBand(region Region, children ...html.Node) html.Node {
	return html.El("div", attrs("hx-swap-oob", "innerHTML:#"+string(region)), children...)
}

// preformatted is text with its whitespace kept.
func preformatted(text string) html.Node {
	// A newline right after <pre> is dropped by the parser, so one is
	// written to keep the text's own leading newline.
	return html.El("pre", nil, html.Text("\n"+text))
}

// timestamp is t in the server's time zone, or never for the zero time.
func timestamp(t time.Time) html.Node {
	if t.IsZero() {
		return html.Text("never")
	}
	return html.El("time", attrs("datetime", t.Format(time.RFC3339Nano)), html.Text(t.Local().Format("2006-01-02 15:04:05")))
}

func cost(usd float64) string {
	return "$" + strconv.FormatFloat(usd, 'f', 4, 64)
}

// excerpt is the first line of text, cut to about 80 characters.
func excerpt(text string) string {
	const limit = 80
	line, _, cut := strings.Cut(strings.TrimSpace(text), "\n")
	if utf8.RuneCountInString(line) > limit {
		line, cut = string([]rune(line)[:limit]), true
	}
	if cut {
		return line + "…"
	}
	if line == "" {
		return "(no prompt)"
	}
	return line
}
