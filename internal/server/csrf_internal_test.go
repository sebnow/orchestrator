package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestGivenOwnersSessionWhenAnotherOriginPostsAFormThenItIsRejected(t *testing.T) {
	srv := startTLSTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	client := srv.client(t, "")
	cookie := logIn(t, srv, client, issueOwnerToken(t, srv))

	for name, headers := range map[string]map[string]string{
		"cross-site":             {"Sec-Fetch-Site": "cross-site"},
		"same-site, other host":  {"Sec-Fetch-Site": "same-site"},
		"origin of another host": {"Origin": "https://attacker.example"},
	} {
		req := withCookie(formRequest(t, srv.url+"/tasks", startForm("laptop", "Run this.")), cookie)
		for header, value := range headers {
			req.Header.Set(header, value)
		}
		if resp, _ := do(t, client, req); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
		}
	}
	if tasks, err := srv.store.tasks(t.Context()); err != nil || len(tasks) != 1 {
		t.Errorf("tasks = %d, %v; want only the seeded one", len(tasks), err)
	}
}

func TestGivenOwnersSessionWhenTheSameOriginPostsAFormThenItIsServed(t *testing.T) {
	srv := startTLSTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	client := srv.client(t, "")
	cookie := logIn(t, srv, client, issueOwnerToken(t, srv))
	origin := srv.url

	for name, headers := range map[string]map[string]string{
		"same-origin": {"Sec-Fetch-Site": "same-origin"},
		"own origin":  {"Origin": origin},
	} {
		req := withCookie(formRequest(t, srv.url+"/tasks", startForm("laptop", "Run this.")), cookie)
		for header, value := range headers {
			req.Header.Set(header, value)
		}
		resp, body := do(t, client, req)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/tasks/") {
			t.Errorf("%s: status %d, location %q: %s", name, resp.StatusCode, resp.Header.Get("Location"), body)
		}
	}
}

func TestGivenAnotherOriginWhenItPostsTheLoginFormThenItIsRejected(t *testing.T) {
	srv := startTLSTestServer(t)
	token := issueOwnerToken(t, srv)
	req := formRequest(t, srv.url+"/login", url.Values{"token": {token}})
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, _ := do(t, srv.client(t, ""), req)
	if resp.StatusCode != http.StatusForbidden || len(resp.Cookies()) != 0 {
		t.Errorf("status %d, cookies %v; want 403 and none", resp.StatusCode, resp.Cookies())
	}
}

func TestGivenInsecureServerWhenAnotherOriginPostsAFormThenItIsRejected(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	req := formRequest(t, srv.url+"/tasks", startForm("laptop", "Run this."))
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	if resp, _ := do(t, http.DefaultClient, req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}
