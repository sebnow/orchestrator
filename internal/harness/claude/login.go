package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/runas"
)

// authStatus is the output of `claude auth status --json`. Claude Code
// 2.1.289 printed loggedIn, authMethod ("claude.ai" logged in, "none"
// logged out), apiProvider, analyticsDisabled, projectsDirectory and
// configDirectory, and, logged in, email, orgId, orgName and
// subscriptionType.
type authStatus struct {
	LoggedIn   *bool  `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	OrgID      string `json:"orgId"`
}

// LoginStatus runs `claude auth status --json` as user. Claude Code
// exits 1 when it is logged out, still printing the status, so the exit
// status is ignored when the output reads as a status.
func (h *Harness) LoginStatus(ctx context.Context, user runas.User) (harness.LoginStatus, error) {
	cmd := user.Command(ctx, "", h.path, []string{"auth", "status", "--json"}, childEnv(os.Environ()), nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	status, err := parseAuthStatus(stdout.Bytes())
	if err != nil {
		if runErr != nil {
			return harness.LoginStatus{}, fmt.Errorf("claude auth status: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return harness.LoginStatus{}, fmt.Errorf("claude auth status: %w", err)
	}
	return status, nil
}

// parseAuthStatus reads the output of `claude auth status --json`. The
// account is the email address and the organisation id, joined by '/':
// usage limits belong to one person's seat in one organisation, and the
// same address may hold seats in several.
func parseAuthStatus(out []byte) (harness.LoginStatus, error) {
	var raw authStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		return harness.LoginStatus{}, fmt.Errorf("read the status: %w", err)
	}
	if raw.LoggedIn == nil {
		return harness.LoginStatus{}, fmt.Errorf("read the status: no loggedIn in %q", out)
	}
	status := harness.LoginStatus{LoggedIn: *raw.LoggedIn, Method: raw.AuthMethod}
	if status.LoggedIn {
		var parts []string
		for _, part := range []string{raw.Email, raw.OrgID} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		status.Account = strings.Join(parts, "/")
	}
	return status, nil
}

const (
	// loginURLTimeout bounds the wait for `claude auth login` to print
	// the URL.
	loginURLTimeout = time.Minute
	// codeTimeout bounds the wait for the outcome of a code.
	codeTimeout = 2 * time.Minute
	// codeErrorGrace is how long the login may run on once it has
	// complained on stderr after a code. Claude Code 2.1.289 exited 1,
	// printing "Login failed: Request failed with status code 400", for a
	// well-formed code it did not accept, but for a malformed one printed
	// "Invalid code. Please make sure the full code was copied." and
	// waited for another; the login then counts as failed with that
	// complaint.
	codeErrorGrace = 5 * time.Second
	// loginTailBytes bounds what is kept of the login's output for its
	// error.
	loginTailBytes = 4 << 10
)

// hyperlink is an OSC 8 hyperlink's opening sequence, its target the
// first group; osc is any operating system command sequence. Claude
// Code 2.1.289 printed the URL as the target and the text of one.
var (
	hyperlink = regexp.MustCompile("\x1b\\]8;[^;\x07\x1b]*;([^\x07\x1b]+)(?:\x07|\x1b\\\\)")
	osc       = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	plainURL  = regexp.MustCompile(`https://[^\s]+`)
)

// loginURL returns the URL a line of `claude auth login` gives: the
// target of its first hyperlink, or else the first https URL in its
// text.
func loginURL(line string) (string, bool) {
	if match := hyperlink.FindStringSubmatch(line); match != nil {
		return match[1], true
	}
	url := plainURL.FindString(osc.ReplaceAllString(line, ""))
	return url, url != ""
}

// StartLogin runs `claude auth login` as user with its input and output
// piped. Claude Code 2.1.289 printed "Opening browser to sign in", then
// "If the browser didn't open, visit: " and the URL, and then waited on
// its input after "Paste code here if prompted > ". It also opened the
// browser that $BROWSER names, or the system's; in that browser the
// login completes without a code.
func (h *Harness) StartLogin(ctx context.Context, user runas.User) (harness.LoginSession, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := user.Command(ctx, "", h.path, []string{"auth", "login"}, childEnv(os.Environ()), nil)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	s := &loginSession{
		stdin:    stdin,
		cancel:   cancel,
		urls:     make(chan string, 1),
		problems: make(chan string, 16),
		done:     make(chan struct{}),
		output:   &tail{limit: loginTailBytes},
		stderr:   &tail{limit: loginTailBytes},
	}
	cmd.Stdout = &lineWriter{line: func(line string) {
		s.output.Write([]byte(line + "\n"))
		if url, ok := loginURL(line); ok {
			select {
			case s.urls <- url:
			default:
			}
		}
	}}
	cmd.Stderr = &lineWriter{line: func(line string) {
		s.stderr.Write([]byte(line + "\n"))
		select {
		case s.problems <- line:
		default:
		}
	}}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 5 * time.Second
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s auth login: %w", h.path, err)
	}
	go func() {
		err := cmd.Wait()
		s.finish(err)
	}()
	timer := time.NewTimer(loginURLTimeout)
	defer timer.Stop()
	select {
	case s.url = <-s.urls:
		return s, nil
	case <-s.done:
		select {
		case s.url = <-s.urls:
			return s, nil
		default:
		}
		return nil, fmt.Errorf("claude auth login ended before giving a URL: %w", s.Err())
	case <-timer.C:
		s.Close()
		return nil, fmt.Errorf("claude auth login gave no URL within %s: %s", loginURLTimeout, strings.TrimSpace(s.output.String()))
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	}
}

// loginSession is `claude auth login` under way.
type loginSession struct {
	url    string
	stdin  io.WriteCloser
	cancel context.CancelFunc
	// urls carries the first URL the login printed, and problems each
	// line it wrote to stderr.
	urls     chan string
	problems chan string
	output   *tail
	stderr   *tail

	done chan struct{}
	mu   sync.Mutex
	err  error
	// failure, when set, is why Submit gave the login up; it is the
	// outcome rather than how the process ended.
	failure error
}

func (s *loginSession) URL() string { return s.url }

func (s *loginSession) Done() <-chan struct{} { return s.done }

func (s *loginSession) Err() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return s.failure
	}
	return s.err
}

// finish records how the process ended: nil for exit 0, and otherwise
// what it wrote to stderr, or how it ended when it wrote nothing.
func (s *loginSession) finish(err error) {
	if err != nil {
		if text := strings.TrimSpace(s.stderr.String()); text != "" {
			err = errors.New(text)
		} else {
			err = fmt.Errorf("claude auth login: %w", err)
		}
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	close(s.done)
}

func (s *loginSession) Submit(code string) error {
	for drained := false; !drained; {
		select {
		case <-s.problems:
		default:
			drained = true
		}
	}
	if _, err := io.WriteString(s.stdin, code+"\n"); err != nil {
		select {
		case <-s.done:
			return s.Err()
		default:
		}
		s.giveUp(fmt.Errorf("give claude auth login the code: %w", err))
		return s.Err()
	}
	timeout := time.NewTimer(codeTimeout)
	defer timeout.Stop()
	var grace <-chan time.Time
	var complaint []string
	for {
		select {
		case <-s.done:
			return s.Err()
		case line := <-s.problems:
			complaint = append(complaint, line)
			if grace == nil {
				grace = time.After(codeErrorGrace)
			}
		case <-grace:
			s.giveUp(errors.New(strings.TrimSpace(strings.Join(complaint, "\n"))))
			return s.Err()
		case <-timeout.C:
			s.giveUp(fmt.Errorf("claude auth login did not finish within %s of the code", codeTimeout))
			return s.Err()
		}
	}
}

// giveUp ends the login with reason as its outcome, unless it has ended
// already.
func (s *loginSession) giveUp(reason error) {
	s.mu.Lock()
	select {
	case <-s.done:
	default:
		s.failure = reason
	}
	s.mu.Unlock()
	s.Close()
}

func (s *loginSession) Close() {
	s.stdin.Close()
	s.cancel()
	<-s.done
}

// lineWriter calls line with each line written to it, without its line
// ending; a last line without one is not seen.
type lineWriter struct {
	partial []byte
	line    func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		idx := bytes.IndexByte(w.partial, '\n')
		if idx < 0 {
			return len(p), nil
		}
		w.line(strings.TrimSuffix(string(w.partial[:idx]), "\r"))
		w.partial = w.partial[idx+1:]
	}
}
