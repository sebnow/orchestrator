package claude

import "testing"

func TestLoginURLTakesTheHyperlinksTargetOrElseThePlainURL(t *testing.T) {
	const url = "https://claude.com/cai/oauth/authorize?code=true&state=abc"
	for name, tc := range map[string]struct {
		line, want string
	}{
		"hyperlink, BEL":         {"If the browser didn't open, visit: \x1b]8;;" + url + "\x07" + url + "\x1b]8;;\x07", url},
		"hyperlink, ST":          {"visit: \x1b]8;;" + url + "\x1b\\click here\x1b]8;;\x1b\\", url},
		"plain":                  {"If the browser didn't open, visit: " + url, url},
		"other escapes stripped": {"\x1b]0;title\x07visit " + url + " now", url},
		"no URL":                 {"Opening browser to sign in…", ""},
	} {
		got, ok := loginURL(tc.line)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: loginURL = %q, %t; want %q", name, got, ok, tc.want)
		}
	}
}
