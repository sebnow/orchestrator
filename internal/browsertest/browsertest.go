// Package browsertest drives a headless Chromium-engine browser for the
// GUI's browser tests. It speaks the Chrome DevTools Protocol over
// --remote-debugging-pipe: the browser reads commands from its fd 3 and
// writes replies and events to its fd 4, each a JSON message ended by a
// NUL byte. Only the methods the tests use are wrapped.
package browsertest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// EnvBrowser names the browser executable, overriding the search of PATH.
const EnvBrowser = "ORCHESTRATOR_BROWSER"

// candidates are the browser executables looked for on PATH, in order.
var candidates = []string{"chromium", "google-chrome-stable", "google-chrome"}

// Find returns the browser to run: $ORCHESTRATOR_BROWSER when set, or
// else the first of chromium, google-chrome-stable and google-chrome on
// PATH.
func Find() (string, error) {
	if path := os.Getenv(EnvBrowser); path != "" {
		return path, nil
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no browser: set $%s or put one of %s on PATH", EnvBrowser, strings.Join(candidates, ", "))
}

// Timeout bounds every wait: a command's reply, a page load, a condition.
const Timeout = 10 * time.Second

// Browser is one headless browser process with a fresh profile.
type Browser struct {
	t       testing.TB
	cmd     *exec.Cmd
	toPipe  *os.File
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	replies map[int64]chan message
	// events are every event received, in order, for Page to wait on.
	events  []message
	arrived chan struct{}
	closed  chan struct{}
	readErr error
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Launch starts the browser headless, with extra flags appended, and
// stops it when the test ends. It fails the test when no browser is found
// or the browser does not answer.
func Launch(t testing.TB, extra ...string) *Browser {
	t.Helper()
	path, err := Find()
	if err != nil {
		t.Fatal(err)
	}
	// The browser reads fd 3 and writes fd 4.
	commandsRead, commandsWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	repliesRead, repliesWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{
		"--headless",
		"--remote-debugging-pipe",
		"--user-data-dir=" + filepath.Join(t.TempDir(), "profile"),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--use-mock-keychain",
		"--password-store=basic",
		"--window-size=1280,1024",
	}, extra...)
	args = append(args, "about:blank")
	cmd := exec.Command(path, args...)
	cmd.ExtraFiles = []*os.File{commandsRead, repliesWrite}
	cmd.Stderr = &lockedWriter{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", path, err)
	}
	commandsRead.Close()
	repliesWrite.Close()
	b := &Browser{
		t: t, cmd: cmd, toPipe: commandsWrite,
		replies: make(map[int64]chan message),
		arrived: make(chan struct{}), closed: make(chan struct{}),
	}
	go b.read(repliesRead)
	t.Cleanup(b.close)
	var version struct {
		Product string `json:"product"`
	}
	if err := b.call(context.Background(), "", "Browser.getVersion", nil, &version); err != nil {
		t.Fatalf("%s did not answer over the pipe: %v\nstderr:\n%s", path, err, b.Stderr())
	}
	t.Logf("browser: %s (%s)", version.Product, path)
	return b
}

type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Stderr is what the browser has written to its standard error.
func (b *Browser) Stderr() string {
	writer := b.cmd.Stderr.(*lockedWriter)
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.buf.String()
}

func (b *Browser) close() {
	b.writeMu.Lock()
	// Browser.close asks for a clean exit; closing the pipe ends it too.
	data, _ := json.Marshal(message{ID: -1, Method: "Browser.close"})
	b.toPipe.Write(append(data, 0))
	b.toPipe.Close()
	b.writeMu.Unlock()
	exited := make(chan struct{})
	go func() {
		b.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		b.cmd.Process.Kill()
		<-exited
	}
}

func (b *Browser) read(pipe *os.File) {
	defer pipe.Close()
	reader := bufio.NewReader(pipe)
	var err error
	for {
		var raw []byte
		if raw, err = reader.ReadBytes(0); err != nil {
			break
		}
		var msg message
		if err = json.Unmarshal(raw[:len(raw)-1], &msg); err != nil {
			break
		}
		b.mu.Lock()
		if msg.Method != "" {
			b.events = append(b.events, msg)
			close(b.arrived)
			b.arrived = make(chan struct{})
		} else if reply, ok := b.replies[msg.ID]; ok {
			delete(b.replies, msg.ID)
			reply <- msg
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	b.readErr = err
	b.mu.Unlock()
	close(b.closed)
}

// call sends method with params to session, or to the browser when
// session is empty, and decodes the result into result unless it is nil.
func (b *Browser) call(ctx context.Context, session, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var rawParams json.RawMessage
	if params != nil {
		var err error
		if rawParams, err = json.Marshal(params); err != nil {
			return err
		}
	}
	reply := make(chan message, 1)
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.replies[id] = reply
	b.mu.Unlock()
	data, err := json.Marshal(message{ID: id, SessionID: session, Method: method, Params: rawParams})
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	_, err = b.toPipe.Write(append(data, 0))
	b.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("%s: write: %w", method, err)
	}
	select {
	case msg := <-reply:
		if msg.Error != nil {
			return fmt.Errorf("%s: %s (%d)", method, msg.Error.Message, msg.Error.Code)
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	case <-b.closed:
		return fmt.Errorf("%s: the browser closed the pipe: %v", method, b.readErr)
	case <-ctx.Done():
		return fmt.Errorf("%s: no reply: %w", method, ctx.Err())
	}
}

// eventCount is how many events have arrived so far.
func (b *Browser) eventCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

// waitEvent waits for an event called method on session among those
// arriving after the first from.
func (b *Browser) waitEvent(session, method string, from int) error {
	deadline := time.After(Timeout)
	for {
		b.mu.Lock()
		for _, event := range b.events[from:] {
			if event.Method == method && event.SessionID == session {
				b.mu.Unlock()
				return nil
			}
		}
		from = len(b.events)
		arrived := b.arrived
		b.mu.Unlock()
		select {
		case <-arrived:
		case <-b.closed:
			return fmt.Errorf("waiting for %s: the browser closed the pipe", method)
		case <-deadline:
			return fmt.Errorf("no %s within %s", method, Timeout)
		}
	}
}

// Page is one tab, attached to with its own session.
type Page struct {
	t       testing.TB
	browser *Browser
	session string
}

// NewPage opens a blank tab.
func (b *Browser) NewPage() *Page {
	b.t.Helper()
	var created struct {
		TargetID string `json:"targetId"`
	}
	b.must(b.call(context.Background(), "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created))
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	b.must(b.call(context.Background(), "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached))
	page := &Page{t: b.t, browser: b, session: attached.SessionID}
	page.must(page.call("Page.enable", nil, nil))
	return page
}

func (b *Browser) must(err error) {
	b.t.Helper()
	if err != nil {
		b.t.Fatalf("%v\nbrowser stderr:\n%s", err, b.Stderr())
	}
}

func (p *Page) call(method string, params, result any) error {
	return p.browser.call(context.Background(), p.session, method, params, result)
}

// must fails the test with err, after saving a screenshot of the page.
func (p *Page) must(err error) {
	p.t.Helper()
	if err != nil {
		p.Screenshot("failure")
		p.t.Fatal(err)
	}
}

// Navigate loads url as if typed into the address bar, and waits for its
// load event.
func (p *Page) Navigate(url string) {
	p.t.Helper()
	from := p.browser.eventCount()
	var navigated struct {
		ErrorText string `json:"errorText"`
	}
	p.must(p.call("Page.navigate", map[string]any{"url": url, "transitionType": "typed"}, &navigated))
	if navigated.ErrorText != "" {
		p.must(fmt.Errorf("navigate to %s: %s", url, navigated.ErrorText))
	}
	p.must(p.browser.waitEvent(p.session, "Page.loadEventFired", from))
}

// LoadsAfter runs action, which is to make the page load a new document,
// and waits for that document's load event.
func (p *Page) LoadsAfter(action func()) {
	p.t.Helper()
	from := p.browser.eventCount()
	action()
	p.must(p.browser.waitEvent(p.session, "Page.loadEventFired", from))
}

// SetScriptsDisabled turns the page's JavaScript off or on, from the next
// document loaded.
func (p *Page) SetScriptsDisabled(disabled bool) {
	p.t.Helper()
	p.must(p.call("Emulation.setScriptExecutionDisabled", map[string]any{"value": disabled}, nil))
}

// Eval evaluates expression in the page, awaiting it if it is a promise,
// and decodes its value into result unless that is nil.
func (p *Page) Eval(expression string, result any) error {
	var evaluated struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := p.call("Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &evaluated)
	if err != nil {
		return err
	}
	if evaluated.ExceptionDetails != nil {
		return fmt.Errorf("evaluate %q: %s %s", expression, evaluated.ExceptionDetails.Text, evaluated.ExceptionDetails.Exception.Description)
	}
	if result == nil || evaluated.Result.Value == nil {
		return nil
	}
	return json.Unmarshal(evaluated.Result.Value, result)
}

// MustEval is Eval that fails the test on an error.
func (p *Page) MustEval(expression string, result any) {
	p.t.Helper()
	p.must(p.Eval(expression, result))
}

// WaitTrue waits until expression, evaluated in the page, is true.
func (p *Page) WaitTrue(expression string) {
	p.t.Helper()
	deadline := time.Now().Add(Timeout)
	var last error
	for time.Now().Before(deadline) {
		var ok bool
		if last = p.Eval(expression, &ok); last == nil && ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.must(fmt.Errorf("not true within %s: %s (last error: %v)", Timeout, expression, last))
}

// node finds the first element matching selector through the DOM domain,
// which works with the page's JavaScript off.
func (p *Page) node(selector string) (int64, error) {
	var document struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if err := p.call("DOM.getDocument", map[string]any{"depth": 0}, &document); err != nil {
		return 0, err
	}
	var found struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := p.call("DOM.querySelector", map[string]any{"nodeId": document.Root.NodeID, "selector": selector}, &found); err != nil {
		return 0, err
	}
	if found.NodeID == 0 {
		return 0, fmt.Errorf("no element matches %q", selector)
	}
	return found.NodeID, nil
}

// HTML returns the outer HTML of the first element matching selector,
// through the DOM domain.
func (p *Page) HTML(selector string) string {
	p.t.Helper()
	node, err := p.node(selector)
	p.must(err)
	var outer struct {
		OuterHTML string `json:"outerHTML"`
	}
	p.must(p.call("DOM.getOuterHTML", map[string]any{"nodeId": node}, &outer))
	return outer.OuterHTML
}

// Click clicks the middle of the first element matching selector with
// the mouse, as the owner would.
func (p *Page) Click(selector string) {
	p.t.Helper()
	node, err := p.node(selector)
	p.must(err)
	p.must(p.call("DOM.scrollIntoViewIfNeeded", map[string]any{"nodeId": node}, nil))
	var quads struct {
		Quads [][]float64 `json:"quads"`
	}
	p.must(p.call("DOM.getContentQuads", map[string]any{"nodeId": node}, &quads))
	if len(quads.Quads) == 0 || len(quads.Quads[0]) != 8 {
		p.must(fmt.Errorf("%q is not visible", selector))
	}
	quad := quads.Quads[0]
	x := (quad[0] + quad[2] + quad[4] + quad[6]) / 4
	y := (quad[1] + quad[3] + quad[5] + quad[7]) / 4
	for _, kind := range []string{"mousePressed", "mouseReleased"} {
		p.must(p.call("Input.dispatchMouseEvent", map[string]any{"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1}, nil))
	}
}

// Type focuses the first element matching selector and types text into
// it.
func (p *Page) Type(selector, text string) {
	p.t.Helper()
	node, err := p.node(selector)
	p.must(err)
	p.must(p.call("DOM.focus", map[string]any{"nodeId": node}, nil))
	p.must(p.call("Input.insertText", map[string]any{"text": text}, nil))
}

// Cookie is a cookie the browser holds.
type Cookie struct {
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"httpOnly"`
	SameSite string `json:"sameSite"`
}

// Cookies returns the cookies the browser would send to url.
func (p *Page) Cookies(url string) []Cookie {
	p.t.Helper()
	var got struct {
		Cookies []Cookie `json:"cookies"`
	}
	p.must(p.call("Network.getCookies", map[string]any{"urls": []string{url}}, &got))
	return got.Cookies
}

// Screenshot saves a PNG of the page as name.png in the test's artifact
// directory, kept when go test runs with -artifacts, and logs its path.
func (p *Page) Screenshot(name string) {
	p.t.Helper()
	var shot struct {
		Data string `json:"data"`
	}
	if err := p.call("Page.captureScreenshot", map[string]any{"format": "png"}, &shot); err != nil {
		p.t.Logf("screenshot: %v", err)
		return
	}
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		p.t.Logf("screenshot: %v", err)
		return
	}
	path := filepath.Join(p.t.ArtifactDir(), name+".png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		p.t.Logf("screenshot: %v", err)
		return
	}
	p.t.Logf("screenshot: %s", path)
}
