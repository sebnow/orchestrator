// Package hetzner is a client for the parts of the Hetzner Cloud API
// that daemon provisioning uses: creating, reading, listing and deleting
// servers, and the price list
// (https://docs.hetzner.cloud/reference/cloud,
// docs/adr/2026-10-10-vps-provisioning.md).
//
// The API token is a secret: it is sent only as the Authorization header
// to the endpoint, and never appears in an error.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Endpoint is the Hetzner Cloud API's base URL.
const Endpoint = "https://api.hetzner.cloud/v1"

// maxResponseBytes bounds a response body; the largest, the price list,
// is tens of kilobytes.
const maxResponseBytes = 4 << 20

// perPage is the most servers the API returns per page.
const perPage = 50

var (
	// ErrNotFound reports a server that does not exist, or no longer does.
	ErrNotFound = errors.New("not found")
	// ErrTransient reports a request the API could not serve now: rate
	// limited, locked by a running action, in conflict, or failed on the
	// API's side. The same request may succeed later.
	ErrTransient = errors.New("temporarily refused")
)

// APIError is an error the API answered with. It wraps ErrNotFound or
// ErrTransient when its status is one of theirs.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hetzner: %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *APIError) Unwrap() error {
	switch {
	case e.Status == http.StatusNotFound:
		return ErrNotFound
	case e.Status == http.StatusConflict, e.Status == http.StatusLocked, e.Status == http.StatusTooManyRequests, e.Status >= 500:
		return ErrTransient
	}
	return nil
}

// LoadToken reads the API token from path, a file only its owner may
// read or write, without surrounding whitespace.
func LoadToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("load Hetzner token: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("load Hetzner token: %s has mode %v; only its owner may read it (chmod 600)", path, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("load Hetzner token: %s: %w", path, fs.ErrInvalid)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("load Hetzner token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("load Hetzner token: %s is empty", path)
	}
	return token, nil
}

// Client calls the API with one project's token.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewClient calls the API at endpoint, such as Endpoint, with token,
// through httpClient, or http.DefaultClient when it is nil.
func NewClient(endpoint, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{endpoint: strings.TrimSuffix(endpoint, "/"), token: token, http: httpClient}
}

// ServerID identifies a server within the API.
type ServerID int64

// ServerStatus is a server's state, such as StatusRunning.
type ServerStatus string

const (
	StatusInitializing ServerStatus = "initializing"
	StatusStarting     ServerStatus = "starting"
	StatusRunning      ServerStatus = "running"
	StatusStopping     ServerStatus = "stopping"
	StatusOff          ServerStatus = "off"
	StatusDeleting     ServerStatus = "deleting"
)

// Server is what this package reads of a server.
type Server struct {
	ID         ServerID          `json:"id"`
	Name       string            `json:"name"`
	Status     ServerStatus      `json:"status"`
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels"`
	ServerType struct {
		Name         string `json:"name"`
		Architecture string `json:"architecture"`
	} `json:"server_type"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

// CreateServer is a server to create: Name is a hostname unique within
// the project; ServerType, Image and Location are names, such as
// "cx23", "debian-13" and "fsn1"; UserData is cloud-init's, at most
// 32 KiB; SSHKeys names or ids of the project's ssh keys, none when
// empty.
type CreateServer struct {
	Name       string            `json:"name"`
	ServerType string            `json:"server_type"`
	Image      string            `json:"image"`
	Location   string            `json:"location,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	UserData   string            `json:"user_data,omitempty"`
	SSHKeys    []string          `json:"ssh_keys,omitempty"`
}

// CreateServer creates a server, which starts once created, and returns
// it as the API first reports it, usually still initializing. The root
// password the API returns for a server created without ssh keys is
// discarded.
func (c *Client) CreateServer(ctx context.Context, server CreateServer) (Server, error) {
	var created struct {
		Server Server `json:"server"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", nil, server, &created); err != nil {
		return Server{}, fmt.Errorf("create server %q: %w", server.Name, err)
	}
	return created.Server, nil
}

// Server returns the server id, or ErrNotFound.
func (c *Client) Server(ctx context.Context, id ServerID) (Server, error) {
	var got struct {
		Server Server `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(int64(id), 10), nil, nil, &got); err != nil {
		return Server{}, fmt.Errorf("get server %d: %w", id, err)
	}
	return got.Server, nil
}

// DeleteServer deletes the server id, or returns ErrNotFound when there
// is none. The server is removed from the account at once; the API
// finishes deleting it in the background.
func (c *Client) DeleteServer(ctx context.Context, id ServerID) error {
	if err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(int64(id), 10), nil, nil, nil); err != nil {
		return fmt.Errorf("delete server %d: %w", id, err)
	}
	return nil
}

// Servers returns every server whose labels match selector, in the
// API's label selector syntax, such as "orchestrator=1,daemon=vps-1".
func (c *Client) Servers(ctx context.Context, selector string) ([]Server, error) {
	var servers []Server
	for page := 1; ; {
		query := url.Values{"label_selector": {selector}, "page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}
		var list struct {
			Servers []Server `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, "/servers", query, nil, &list); err != nil {
			return nil, fmt.Errorf("list servers %q: %w", selector, err)
		}
		servers = append(servers, list.Servers...)
		next := list.Meta.Pagination.NextPage
		if next == nil || *next <= page {
			return servers, nil
		}
		page = *next
	}
}

// Price is an amount in the price list's currency, a decimal number,
// without and with VAT.
type Price struct {
	Net   string `json:"net"`
	Gross string `json:"gross"`
}

// Pricing is what this package reads of the price list: each server
// type's price in each location.
type Pricing struct {
	Currency    string `json:"currency"`
	VATRate     string `json:"vat_rate"`
	ServerTypes []struct {
		Name   string `json:"name"`
		Prices []struct {
			Location     string `json:"location"`
			PriceHourly  Price  `json:"price_hourly"`
			PriceMonthly Price  `json:"price_monthly"`
		} `json:"prices"`
	} `json:"server_types"`
}

// Pricing returns the price list.
func (c *Client) Pricing(ctx context.Context) (Pricing, error) {
	var got struct {
		Pricing Pricing `json:"pricing"`
	}
	if err := c.do(ctx, http.MethodGet, "/pricing", nil, nil, &got); err != nil {
		return Pricing{}, fmt.Errorf("get pricing: %w", err)
	}
	return got.Pricing, nil
}

// do sends a request to path with query and, unless nil, in as its JSON
// body, and decodes a 2xx response's body into out unless it is nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	target := c.endpoint + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode, Code: "unknown", Message: http.StatusText(resp.StatusCode)}
		var answer struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &answer) == nil && answer.Error != nil {
			apiErr.Code, apiErr.Message = answer.Error.Code, answer.Error.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
