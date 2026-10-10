package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// The server distributes the forges' ssh host keys to its daemons, so
// that a daemon on a fresh machine can verify the forge it pushes to
// without anything done on the machine: the owner's keys, kept as a
// setting, and GitHub's, which the server fetches from GitHub's meta API.
// It sends their union to each daemon as a host_keys command when the
// daemon connects and whenever it changes.

const (
	// GitHubMetaURL is GitHub's meta API, whose ssh_keys field lists the
	// host keys of github.com.
	GitHubMetaURL = "https://api.github.com/meta"
	// githubHost is the host GitHub's ssh_keys are for.
	githubHost = "github.com"
	// githubRefresh is how often GitHub's keys are fetched, and
	// githubRetry how soon a failed fetch is tried again.
	githubRefresh = 24 * time.Hour
	githubRetry   = time.Hour
	// githubTimeout bounds one fetch.
	githubTimeout = 30 * time.Second
	// maxGitHubMetaBytes bounds the meta API's answer, which lists
	// GitHub's address ranges besides its keys.
	maxGitHubMetaBytes = 16 << 20
	// maxHostKeysBytes bounds the owner's host keys.
	maxHostKeysBytes = 1 << 20

	forgeHostKeysSetting  = "forge_host_keys"
	githubHostKeysSetting = "github_host_keys"
)

// errInvalidHostKeys refuses host keys that are not known_hosts lines.
var errInvalidHostKeys = errors.New("invalid host keys")

// githubHostKeys are GitHub's host keys as last fetched, kept so that a
// server that restarts without reaching GitHub still sends them.
type githubHostKeys struct {
	Lines     []string  `json:"lines"`
	FetchedAt time.Time `json:"fetched_at"`
}

// hostKeysView is what the owner API reports of the host keys: the
// owner's text, GitHub's lines and when they were fetched, and Lines,
// the union the daemons get.
type hostKeysView struct {
	Forge           string     `json:"forge"`
	GitHub          []string   `json:"github"`
	GitHubFetchedAt *time.Time `json:"github_fetched_at,omitempty"`
	Lines           []string   `json:"lines"`
}

// parseHostKeys returns the known_hosts lines of text, one per line that
// is neither blank nor a comment, with their fields separated by single
// spaces. Each is "[marker] hosts keytype key [comment]", where marker
// is @cert-authority or @revoked and key is the base64 of a public key
// of type keytype.
func parseHostKeys(text string) ([]string, error) {
	lines := []string{}
	for number, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if err := checkHostKey(fields); err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", errInvalidHostKeys, number+1, err)
		}
		lines = append(lines, strings.Join(fields, " "))
	}
	return lines, nil
}

// checkHostKey checks the fields of one known_hosts line.
func checkHostKey(fields []string) error {
	if fields[0] == "@cert-authority" || fields[0] == "@revoked" {
		fields = fields[1:]
	} else if strings.HasPrefix(fields[0], "@") {
		return fmt.Errorf("unknown marker %q", fields[0])
	}
	if len(fields) < 3 {
		return errors.New(`want "hosts keytype key", as ssh-keyscan prints`)
	}
	return checkPublicKey(fields[1], fields[2])
}

// checkPublicKey checks that key is the base64 of an ssh public key of
// keytype, whose wire form starts with its type.
func checkPublicKey(keytype, key string) error {
	blob, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return fmt.Errorf("the key is not base64: %v", err)
	}
	if len(blob) < 4 {
		return errors.New("the key is too short")
	}
	size := binary.BigEndian.Uint32(blob)
	if uint64(size) > uint64(len(blob)-4) || string(blob[4:4+size]) != keytype {
		return fmt.Errorf("the key is not of type %q", keytype)
	}
	return nil
}

// parseGitHubMeta returns the known_hosts lines for github.com of the
// ssh_keys in body, an answer of GitHub's meta API.
func parseGitHubMeta(body []byte) ([]string, error) {
	var meta struct {
		SSHKeys []string `json:"ssh_keys"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("decode GitHub's meta: %w", err)
	}
	if len(meta.SSHKeys) == 0 {
		return nil, errors.New("GitHub's meta lists no ssh_keys")
	}
	lines := make([]string, 0, len(meta.SSHKeys))
	for _, key := range meta.SSHKeys {
		fields := strings.Fields(key)
		if len(fields) != 2 {
			return nil, fmt.Errorf("GitHub's ssh key %q is not \"keytype key\"", key)
		}
		if err := checkPublicKey(fields[0], fields[1]); err != nil {
			return nil, fmt.Errorf("GitHub's ssh key %q: %w", key, err)
		}
		lines = append(lines, githubHost+" "+fields[0]+" "+fields[1])
	}
	return lines, nil
}

// fetchGitHubHostKeys asks GitHub's meta API at metaURL for its host
// keys and returns them as known_hosts lines.
func fetchGitHubHostKeys(ctx context.Context, client *http.Client, metaURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch GitHub's meta: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubMetaBytes))
	if err != nil {
		return nil, fmt.Errorf("read GitHub's meta: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch GitHub's meta: %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 512)])))
	}
	return parseGitHubMeta(body)
}

// mergeHostKeys is the union of the owner's lines and GitHub's, the
// owner's first, each once.
func mergeHostKeys(forge, github []string) []string {
	lines := make([]string, 0, len(forge)+len(github))
	for _, line := range slices.Concat(forge, github) {
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	return lines
}

// setting returns the value of the setting name, "" when it is unset.
func (s *Store) setting(ctx context.Context, name string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", name, err)
	}
	return value, nil
}

// putSetting sets the setting name to value.
func (s *Store) putSetting(ctx context.Context, name, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, value)
	if err != nil {
		return fmt.Errorf("write setting %s: %w", name, err)
	}
	return nil
}

// hostKeys returns the host keys the server holds.
func (s *Store) hostKeys(ctx context.Context) (hostKeysView, error) {
	forge, err := s.setting(ctx, forgeHostKeysSetting)
	if err != nil {
		return hostKeysView{}, err
	}
	raw, err := s.setting(ctx, githubHostKeysSetting)
	if err != nil {
		return hostKeysView{}, err
	}
	view := hostKeysView{Forge: forge, GitHub: []string{}}
	if raw != "" {
		var github githubHostKeys
		if err := json.Unmarshal([]byte(raw), &github); err != nil {
			return hostKeysView{}, fmt.Errorf("read GitHub's host keys: %w", err)
		}
		view.GitHub, view.GitHubFetchedAt = github.Lines, &github.FetchedAt
	}
	forgeLines, err := parseHostKeys(forge)
	if err != nil {
		return hostKeysView{}, fmt.Errorf("read the forge host keys: %w", err)
	}
	view.Lines = mergeHostKeys(forgeLines, view.GitHub)
	return view, nil
}

// hasCommandOf reports whether daemon's command log holds a command of
// kind.
func (s *Store) hasCommandOf(ctx context.Context, daemon protocol.DaemonID, kind protocol.CommandKind) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM commands WHERE daemon_id = ? AND kind = ?)`,
		string(daemon), string(kind)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("look up the %s commands of daemon %q: %w", kind, daemon, err)
	}
	return found, nil
}

// changeHostKeys applies change to the stored host keys, and sends every
// connected daemon the union when it changed. A daemon that is not
// connected gets it when it connects.
func (s *Server) changeHostKeys(ctx context.Context, change func(context.Context) error) error {
	s.hostKeysMu.Lock()
	defer s.hostKeysMu.Unlock()
	before, err := s.store.hostKeys(ctx)
	if err != nil {
		return err
	}
	if err := change(ctx); err != nil {
		return err
	}
	after, err := s.store.hostKeys(ctx)
	if err != nil {
		return err
	}
	if slices.Equal(before.Lines, after.Lines) {
		return nil
	}
	s.log.Info("host keys changed", "lines", len(after.Lines))
	var errs []error
	for _, daemon := range s.connectedDaemons() {
		errs = append(errs, s.sendHostKeys(ctx, daemon, after.Lines))
	}
	return errors.Join(errs...)
}

// sendHostKeysAtConnect sends daemon, which is connecting, the host
// keys, unless there are none and it was never sent any. The caller
// holds hostKeysMu.
func (s *Server) sendHostKeysAtConnect(ctx context.Context, daemon protocol.DaemonID) error {
	view, err := s.store.hostKeys(ctx)
	if err != nil {
		return err
	}
	if len(view.Lines) == 0 {
		sent, err := s.store.hasCommandOf(ctx, daemon, protocol.CommandHostKeys)
		if err != nil || !sent {
			return err
		}
	}
	return s.sendHostKeys(ctx, daemon, view.Lines)
}

// sendHostKeys issues daemon a host_keys command carrying lines.
func (s *Server) sendHostKeys(ctx context.Context, daemon protocol.DaemonID, lines []string) error {
	payload, err := json.Marshal(protocol.HostKeys{Lines: lines})
	if err != nil {
		return err
	}
	_, err = s.store.issueDaemonCommand(ctx, daemon, protocol.CommandHostKeys, payload)
	return err
}

// setForgeHostKeys replaces the owner's host keys with text, which must
// be known_hosts lines, comments and blank lines allowed.
func (s *Server) setForgeHostKeys(ctx context.Context, text string) error {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if _, err := parseHostKeys(text); err != nil {
		return err
	}
	return s.changeHostKeys(ctx, func(ctx context.Context) error {
		return s.store.putSetting(ctx, forgeHostKeysSetting, text)
	})
}

// refreshGitHubHostKeys fetches GitHub's host keys and keeps them.
func (s *Server) refreshGitHubHostKeys(ctx context.Context) error {
	lines, err := fetchGitHubHostKeys(ctx, s.github.client, s.github.metaURL)
	if err != nil {
		return err
	}
	value, err := json.Marshal(githubHostKeys{Lines: lines, FetchedAt: s.sched.now().UTC()})
	if err != nil {
		return err
	}
	return s.changeHostKeys(ctx, func(ctx context.Context) error {
		return s.store.putSetting(ctx, githubHostKeysSetting, string(value))
	})
}

// FetchGitHubHostKeys fetches GitHub's host keys when ctx starts and
// every 24 hours until it ends, an hour after a fetch that failed. It
// returns at once when the server was given no meta URL.
func (s *Server) FetchGitHubHostKeys(ctx context.Context) {
	if s.github.metaURL == "" {
		return
	}
	for {
		wait := githubRefresh
		if err := s.refreshGitHubHostKeys(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("fetch GitHub's host keys; the last ones fetched stay", "error", err, "retry_in", githubRetry)
			wait = githubRetry
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// hostKeysStatus maps a host keys error to the owner API's status for
// it.
func hostKeysStatus(err error) int {
	switch {
	case errors.Is(err, errInvalidHostKeys):
		return http.StatusBadRequest
	case err != nil:
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

// getHostKeys answers with the host keys.
func (s *Server) getHostKeys(w http.ResponseWriter, r *http.Request) {
	view, err := s.store.hostKeys(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putHostKeys replaces the owner's host keys with the body's,
// {"forge": "<known_hosts lines>"}, and answers with the host keys.
func (s *Server) putHostKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Forge string `json:"forge"`
	}
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxHostKeysBytes), &body); err != nil {
		http.Error(w, "decode host keys: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.setForgeHostKeys(r.Context(), body.Forge); err != nil {
		if status := hostKeysStatus(err); status != http.StatusInternalServerError {
			http.Error(w, err.Error(), status)
			return
		}
		s.internalError(w, err)
		return
	}
	s.getHostKeys(w, r)
}

// getSettingsPage shows the settings page.
func (s *Server) getSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.writeSettingsPage(w, r, http.StatusOK, nil, "")
}

// writeSettingsPage writes the settings page with forge, when not nil,
// in place of the stored host keys, and problem, when set, saying why
// they were refused.
func (s *Server) writeSettingsPage(w http.ResponseWriter, r *http.Request, status int, forge *string, problem string) {
	view, err := s.store.hostKeys(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	keys := component.HostKeys{Forge: view.Forge, GitHub: view.GitHub, GitHubOff: s.github.metaURL == "", Problem: problem}
	if forge != nil {
		keys.Forge = *forge
	}
	if view.GitHubFetchedAt != nil {
		keys.GitHubFetchedAt = *view.GitHubFetchedAt
	}
	s.writeHTML(w, status, component.SettingsPage(keys))
}

// postHostKeysForm replaces the owner's host keys with the form's and
// returns the owner to the settings page.
func (s *Server) postHostKeysForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHostKeysBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	forge := r.PostForm.Get("forge")
	if err := s.setForgeHostKeys(r.Context(), forge); err != nil {
		status := hostKeysStatus(err)
		if status == http.StatusInternalServerError {
			s.internalError(w, err)
			return
		}
		s.writeSettingsPage(w, r, status, &forge, "Not saved: "+err.Error()+".")
		return
	}
	redirect(w, r, component.SettingsURL)
}

// hostKeysClient is how the server fetches GitHub's host keys: from
// metaURL, "" for never, with client.
type hostKeysClient struct {
	metaURL string
	client  *http.Client
}
