package component

import (
	"maps"
	"net/url"
	"slices"

	"github.com/sebnow/orchestrator/internal/html"
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
