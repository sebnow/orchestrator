package component

import (
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
)

// SettingsURL shows the server's settings and has their forms.
const SettingsURL = "/settings"

// HostKeys is what the settings page shows of the forges' ssh host keys
// the server sends its daemons: Forge, the owner's known_hosts lines as
// typed; GitHub, github.com's lines as last fetched at GitHubFetchedAt,
// zero before the first fetch; GitHubOff when the server does not fetch
// them. Problem, when set, says why the owner's keys were refused.
type HostKeys struct {
	Forge           string
	GitHub          []string
	GitHubFetchedAt time.Time
	GitHubOff       bool
	Problem         string
}

// SettingsPage is the settings page.
func SettingsPage(keys HostKeys) html.Node {
	var github html.Node
	switch {
	case len(keys.GitHub) > 0:
		fetched := html.Fragment(html.Text("Fetched from GitHub's meta API "), timestamp(keys.GitHubFetchedAt), html.Text("."))
		if keys.GitHubOff {
			fetched = html.Fragment(fetched, html.Text(" The server no longer fetches them."))
		}
		github = html.Fragment(html.El("p", nil, fetched), preformatted(strings.Join(keys.GitHub, "\n")))
	case keys.GitHubOff:
		github = html.El("p", attrs("class", "empty"), html.Text("The server does not fetch GitHub's keys: it was started with an empty -github-meta-url."))
	default:
		github = html.El("p", attrs("class", "empty"), html.Text("Not fetched yet; the server fetches them when it starts and every 24 hours."))
	}
	return Page("Settings",
		Section("Forge host keys",
			html.El("p", nil, html.Text("The ssh host keys of the forges the daemons push to, one known_hosts line each, as ssh-keyscan prints them. "+
				"Each daemon gets these and GitHub's below when it connects and whenever they change, and its ssh checks a forge's host key against them; "+
				"with a harness user it reads no other known_hosts but the system's. Compare each key's fingerprint with the one its forge publishes before saving.")),
			PlainForm(SettingsURL+"/host-keys", keys.Problem,
				Field(FieldSpec{Kind: FieldTextarea, Name: "forge", Label: "Host keys", Value: keys.Forge, Rows: 8,
					Placeholder: "git.example.com ssh-ed25519 AAAA..."}),
				Button("Save", VariantPrimary, "", ""))),
		Section("GitHub's host keys", github),
	)
}
