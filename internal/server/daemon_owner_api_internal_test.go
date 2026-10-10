package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"
)

const testSSHKey = "AAAAC3NzaC1lZDI1NTE5AAAAINUZukIJ+vKsP7bTdxRjE93dbMEuScRvX+q1pD16tdex"

func TestGivenDaemonsWhenTheOwnerListsAndGetsThemThenEachHasItsLabelsFactsKeySeenLostSlotsAndRunningCount(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"darwin","slots":"3","ssh_public_key":"`+testSSHKey+`"}`)
	if err := srv.store.setLabels(t.Context(), "laptop", Labels{"tier": "dev", "slots": "2"}); err != nil {
		t.Fatal(err)
	}
	openCommandStream(t, srv, "laptop", "")
	startedTurn(t, srv)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/vps/acks", "")
	lostAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	if _, err := srv.store.db.ExecContext(t.Context(), `UPDATE daemons SET lost_at = ? WHERE id = 'vps'`, formatTime(lostAt)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(srv.connectedDaemons(), "laptop") {
		t.Fatal("laptop's command stream is not open")
	}

	var list []map[string]any
	getJSON(t, srv.url+"/v1/daemons", &list)
	var laptop map[string]any
	getJSON(t, srv.url+"/v1/daemons/laptop", &laptop)

	if len(list) != 2 || list[0]["id"] != "laptop" || list[1]["id"] != "vps" {
		t.Fatalf("list = %v, want laptop and vps, by id", list)
	}
	seen, _ := time.Parse(time.RFC3339Nano, laptop["last_seen"].(string))
	if time.Since(seen) > time.Minute {
		t.Errorf("last_seen = %v, want just now", laptop["last_seen"])
	}
	delete(laptop, "last_seen")
	requireJSONEqual(t, laptop, map[string]any{
		"id": "laptop", "labels": map[string]any{"tier": "dev", "slots": "2"},
		"facts":               map[string]any{"os": "darwin", "slots": "3", "ssh_public_key": testSSHKey},
		"ssh_public_key":      "ssh-ed25519 " + testSSHKey + " orchestrator@laptop",
		"ssh_public_key_blob": testSSHKey,
		"connected":           true, "lost": false, "slots": float64(2), "running": float64(1),
	})
	vps := list[1]
	delete(vps, "last_seen")
	requireJSONEqual(t, vps, map[string]any{
		"id": "vps", "labels": map[string]any{}, "facts": map[string]any{},
		"connected": false, "lost": true, "lost_since": lostAt.Format(time.RFC3339Nano), "slots": float64(1), "running": float64(0),
	})
}

func TestGivenUnknownOrMalformedDaemonWhenTheOwnerGetsItThenNotFoundOrBadRequest(t *testing.T) {
	srv := startTestServer(t)

	unknown, _ := doRequest(t, http.MethodGet, srv.url+"/v1/daemons/ghost", "")
	malformed, _ := doRequest(t, http.MethodGet, srv.url+"/v1/daemons/no%20space", "")
	var list []json.RawMessage
	getJSON(t, srv.url+"/v1/daemons", &list)

	if unknown != http.StatusNotFound || malformed != http.StatusBadRequest {
		t.Errorf("status = %d and %d, want 404 and 400", unknown, malformed)
	}
	if list == nil || len(list) != 0 {
		t.Errorf("list = %v, want an empty array", list)
	}
}
