# The GUI in a real browser: findings (2026-10-08)

The orchestrator's server serves a web GUI where the owner starts,
watches and steers Claude Code tasks. Its pages use htmx and htmx's SSE
extension. Before these tests, every GUI test drove the handlers
directly or followed the no-JS path, so nothing showed what htmx does
with the pages. Seven tests now load the GUI in a headless browser,
behind the `browser` build tag. This note records how they run, what
they established, the GUI bug they found and its fix, and what they
leave open.

Markers: **UNVERIFIED** flags a claim that no test or run observed and
that no primary source confirmed, including inferred causes. Everything
else was observed in the runs below, or read from the source file
named where it is used.

Terms:

- The "owner" is the person who uses the GUI. They sign in with the
  "owner token", a secret the server issues; the "owner-only routes"
  are every route but login, logout, the static files and the daemon
  API, and they need the owner's session.
- A "daemon" runs tasks with the `claude` CLI on some machine and talks
  to the server: it posts each task's events to the daemon API and
  reads the commands the server issues from a command stream.
- A "turn" is one run of a task's agent, started by the task's start, a
  follow-up prompt, a resume or a message delivery. The "scheduler"
  decides when a waiting turn is admitted. Each daemon has a fixed
  number of "slots", and a task holds one from the admission of its
  turn until its process exits.
- The "task page" is `GET /tasks/{task}` and the "dashboard" is `GET /`.
  The dashboard's "attention list" is its list of tasks that need the
  owner, such as failed ones.
- The "stream" is `GET /tasks/{task}/stream`, the server-sent events
  that keep a task page current. The "poller" is the element that
  fetches `GET /tasks/{task}/updates` every five seconds once the stream
  has failed.
- A "region" is a part of a page that a response replaces by an htmx
  out-of-band swap: the task header, the permission prompt, the prompt
  form and its Send button, among others (`internal/component`).
- A "badge" is the task state shown in the header, such as `running`
  or `paused`, with `queued #N` beside it while a turn of the task
  waits for the scheduler.
- `yuqnvoyx`, `rssryxkk`, `ultvuxvw` and `nmuutxnm` are jujutsu change
  ids in this repository.

## Method

Environment: the project owner's macOS workstation (Darwin 25.4.0,
arm64), Go 1.26.8 and Google Chrome 154.0.8037.98, both from the Nix
dev shell.

The dev shell's browser differs by platform. nixpkgs' `chromium`
(154.0.8037.97) lists only Linux systems in `meta.platforms`, and
`nix develop` refused to evaluate it on `aarch64-darwin`. On macOS the
shell therefore has nixpkgs' `google-chrome`, the only unfree package
the flake allows, while Linux keeps `chromium`. The darwin package puts
`google-chrome-stable` and `google-chrome` on `PATH`. Its download from
`dl.google.com` was slow and needed one curl retry, after "HTTP/2
stream 1 reset by server (error 0x2 INTERNAL_ERROR)".

`internal/browsertest` (`yuqnvoyx`) starts the browser with
`--headless --remote-debugging-pipe` and a fresh profile per test, and
speaks the DevTools protocol as NUL-terminated JSON over the browser's
fds 3 and 4, using only the standard library. This pipe transport
worked on this machine. The client clicks with
`Input.dispatchMouseEvent` at the middle of the element, types with
`Input.insertText`, and reads elements through the `DOM` domain, so all
three work with the page's JavaScript off.

The tests are in `internal/server/browser_internal_test.go`. Each
starts the server in process on a loopback listener and logs every
request it receives, with its `HX-Request` header, its session cookie
and its status. No real daemon runs, and the tests do not start the
`claude` CLI. A fake daemon built from the server package's test
fixtures posts events to the daemon API and reads the command stream
as a daemon would. Six tests use `Options{Insecure: true}`, the mode
the server's `-insecure-loopback` flag selects. The authentication test
uses the TLS server that `internal/server/auth_internal_test.go`
builds: a certificate from an `internal/pki` CA that the browser does
not trust, and Chrome started with `--ignore-certificate-errors`.

Run with:

    nix develop -c go test -tags browser -count=1 -v ./internal/server/

Each test took 0.4 to 0.9 seconds, except the polling test, which took
about 5.6 seconds because it waits for a five-second poll. Once the fix
and the settle wait (findings 1 and 2) were in, the forms and dismiss
tests passed 30 runs in a row, and the first six tests passed 5 runs
each.

## What the tests established

1. **htmx and its SSE extension run on the dashboard.**
   `htmx.version` is `2.0.11`, and `htmx.createEventSource` is a
   function. The vendored `htmx.min.js` calls an extension's `init` when
   the extension is defined, and the extension's `init` adds
   `createEventSource`.
2. **The stream updates a task page in place.** The extension's
   `EventSource`, which htmx keeps in the element's internal data, was
   open (`readyState` 1). An event posted through the daemon API showed
   as the transcript's last entry. Meanwhile the page was loaded once,
   the stream was opened once, `/updates` was never requested, and a
   marker set on `window` after loading survived, so the page did not
   reload.
3. **The task page's forms work through htmx without a navigation.**
   With the fake daemon connected, so that the scheduler admits turns,
   the test clicked Allow on a permission request, then Pause, Resume,
   Send with a follow-up prompt, and Stop. After each, the badge showed
   `running`, `pausing`, `running`, `running` and `stopped`, and the
   fake daemon received each command with the expected payload. Pause
   disabled Send, and after the prompt the prompt form's text box was
   empty. All five POSTs carried `HX-Request` and were answered 200. The
   page was loaded once and the marker survived.
4. **Dismissing a failed task from the dashboard** removed it from the
   attention list in place, leaving "Nothing needs attention.", through
   one htmx POST. `/?dismissed=show` then listed it, marked dismissed.
5. **The session cookie survives a typed navigation.** With
   `-insecure-loopback` the server serves plain HTTP and skips
   authentication. `getLogin` and `postLogin` in
   `internal/server/auth.go` redirect to `/` without setting a cookie.
   So `__Host-session`, which is `Secure`, exists only over TLS, and the
   test uses the TLS server. In the browser, `/` redirected to `/login`.
   Signing in with the owner token led to the dashboard, and the
   browser held the cookie as `Secure`, `HttpOnly`, `SameSite=Strict`,
   `Path=/`. A further `Page.navigate` to `/`, with `transitionType`
   `typed`, sent the cookie and was answered 200 with the dashboard.
   **UNVERIFIED:** that Chrome treats `Page.navigate` with
   `transitionType` `typed` exactly as it treats a URL typed by a
   person.
6. **The task page works with JavaScript off**
   (`Emulation.setScriptExecutionDisabled`). It showed its transcript.
   The browser fetched `style.css` but neither htmx nor its SSE
   extension, and never opened the stream. A follow-up prompt and Pause
   were plain POSTs answered 303, each followed by a fresh load of the
   task page. That page showed the prompt in the transcript and then
   the `pausing` badge.
7. **A form makes a polling page poll at once.** With every stream
   answered 503, the page fell back to the poller. The test waited for
   the first five-second poll, then clicked Pause. A poll followed
   within a second of the POST and showed `pausing`. Without the
   `HX-Trigger` header (finding 1), the same test failed: the change
   waited for the next five-second poll.

## Findings

1. **Fixed: a form response could undo a newer update from the
   stream** (`rssryxkk`). A task page's htmx form response swapped the
   header, the permission prompt and the Send button, which the stream
   also swaps. The response was rendered when the command was queued.
   When the scheduler admitted the turn at once, the stream's newer
   header sometimes arrived first, and the response's older one then
   replaced it. The page showed `paused` and `queued #1` for a resume
   the daemon had already received, and kept showing it until the task
   next changed. Test 3 failed this way in 1 of 10 runs, and in 3 of 15
   in a second series, in which each failure logged the stale header
   after the fake daemon had received the resume. The stream, or the
   poller once the page polls, is now the only writer of the header and
   the permission prompt. Every successful form response sets
   `HX-Trigger: task-changed`, which also makes the poller poll
   (test 7). After a prompt, the response also swaps in an emptied
   prompt form, which still carries a Send button (see Open).
2. **Not fixed: a click in htmx's settle window bypasses htmx.** htmx
   2.0.11 attaches its handlers to swapped-in content (`processNode`)
   in a settle step that runs after `htmx.config.defaultSettleDelay`,
   20 ms (read from the vendored `htmx.min.js`). Test 3 sometimes
   clicked Resume within that window after the stream swapped the
   header in. The browser then submitted the form itself, without
   `HX-Request`; the server answered 303 and the page reloaded. That
   failed the test in 3 of 20 runs, and the outcome was still correct
   through the no-JS path. The tests now wait for htmx to settle (no
   element with the class `htmx-added` or `htmx-settling`) before
   clicking, and no test failed this way in the 30 runs since.
   **UNVERIFIED:** how often a person clicks within 20 ms of a swap.
3. **Not fixed: `/favicon.ico` without a session redirects to the login
   form.** The browser requests it on every page, and the owner-only
   routes answer it before sign-in with 303 to `/login`. That doubles
   the count of login form loads that test 5 observes. With a session
   it is answered 404. **UNVERIFIED:** that it has no other effect.
4. **Observed: a prompt to a running task is admitted at once.** It
   does not wait in the queue, because the task already holds a slot
   (`holdsSlot` in `internal/server/scheduler.go`), so the header does
   not show `queued #N`. Test 6 therefore checks the transcript for the
   prompt rather than the badge for a queued turn.
5. **Observed: the TLS server logs "TLS handshake error ... EOF" three
   times per browser start** in test 5. **UNVERIFIED:** that these are
   the browser closing connections it opened in advance.

## Open

- **UNVERIFIED:** that the tests pass with nixpkgs' `chromium` on
  Linux. They ran only on macOS with Google Chrome.
- **UNVERIFIED:** that no similar race remains. After a prompt, the
  response's emptied prompt form carries a Send button rendered from
  the response's own reading of the task. The dashboard's dismiss
  response and its five-second refresh both replace the dashboard, so a
  refresh rendered before a dismissal could show the task again until
  the next refresh. Dismissing from the task page still swaps the
  header in the response. None of these was observed.
