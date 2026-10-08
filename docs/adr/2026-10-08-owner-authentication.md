---
status: accepted
date: 2026-10-08
source: >-
  owner decision, 2026-10-08, to add authentication before the
  server is exposed to the internet;
  [daemon authentication](2026-10-08-daemon-authentication.md)
---

# Owner authentication

## Context

The owner steers every agent through the server's web interface and its
owner API ([client connectivity](2026-10-07-client-connectivity.md)).
Both let their caller start tasks that run an agent with broad
permissions on a daemon's machine, so once the server is reachable from
the internet, an unauthenticated owner route hands those machines to
anyone who finds it. The orchestrator serves one person
([tenancy](2026-10-07-tenancy.md)), and the server must be reachable
without a VPN or private network.

The web interface is server-rendered HTML whose forms POST to the
server; scripts and tools use the JSON owner API.
[Daemon authentication](2026-10-08-daemon-authentication.md) has the
server terminate TLS on one port for both daemons and the owner's
browser.

## Decision

The owner authenticates with one token, using only the Go standard
library.

- A `cmd/server` subcommand makes the token from 256 random bits,
  prints it once, and stores only its SHA-256 hash. Issuing a new token
  replaces the old one and ends every session.
- Plain SHA-256 is adequate for this token. A slow key-derivation
  function protects low-entropy secrets that an attacker holding the
  hash could guess; guessing a uniformly random 256-bit token is
  infeasible whether the hash is fast or slow. For the same reason,
  failed logins are not rate-limited.
- The owner API takes the token in an `Authorization: Bearer` header.
- The web interface has a login form that takes the token and sets a
  session cookie that is `HttpOnly`, `Secure` and `SameSite=Strict`.
  The cookie holds a random session id. The server keeps the id's hash
  and its expiry, 30 days after login, so that logging out or issuing a
  new token ends sessions at once.
- The owner API answers a request without a valid token or session with
  401; the web interface redirects it to the login form.
- `net/http.CrossOriginProtection` (Go 1.25 and later) and the
  `SameSite` cookie protect against cross-site request forgery.
  `CrossOriginProtection` rejects browser requests other than GET, HEAD
  and OPTIONS whose `Sec-Fetch-Site` or `Origin` header shows another
  origin, so GET handlers must not change state. Forms do not need CSRF
  tokens.
- The `-insecure-loopback` mode of
  [daemon authentication](2026-10-08-daemon-authentication.md) turns off
  owner authentication too.

Alternatives rejected:

- Client certificates in the browser: each browser and device needs the
  certificate installed, and the browser asks the owner to pick one.
- A password: one chosen by a person can be guessed, so it would need a
  slow key-derivation function and limits on failed logins. The
  standard library offers only PBKDF2; bcrypt, scrypt and Argon2 would
  add `golang.org/x/crypto` as a dependency. A generated token needs
  neither.
- Signed session cookies: they need no lookup, but a single session
  cannot be ended early (rotating the key ends every session), and the
  key is one more secret to keep.
- Relying on a VPN alone:
  [client connectivity](2026-10-07-client-connectivity.md) requires the
  server to work without one.

## Consequences

- Scripts using the owner API must send the `Authorization` header; the
  API's messages are otherwise unchanged.
- Whoever holds the token controls every daemon. Losing it means
  issuing a new one, which also logs out every browser.
- The owner logs in to each browser with the token every 30 days, and
  after logging out or issuing a new token, so the token has to be kept
  somewhere retrievable, such as a password manager.
- Requests with neither `Sec-Fetch-Site` nor `Origin`, such as scripts
  using the owner API, pass the cross-origin check and rely on the
  token alone.
- With `SameSite=Strict`, a link to the web interface followed from
  another site, such as a chat message, arrives without the cookie
  (draft-ietf-httpbis-rfc6265bis-22, section 5.6.7.1), so it lands on
  the login form even when the owner is logged in. Opening the page
  again from the address bar sends the cookie: in the browser test
  `TestGivenOwnerSignedInWhenTheDashboardURLIsTypedIntoTheAddressBarThenTheSessionCookieIsSentAndTheDashboardShown`,
  Chrome sent it on a navigation of type `typed` and the server showed
  the dashboard
  ([browser check](../design/2026-10-08-browser-check.md), finding 5).

Revisit if anyone besides the owner needs access.
