// Package client provides the QuotaGate decision client and net/http middleware.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Decision struct {
	Allowed       bool   `json:"allowed"`
	Reason        string `json:"reason"`
	UTCDay        string `json:"utc_day"`
	DailyUsed     int64  `json:"daily_used"`
	DailyLimit    int64  `json:"daily_limit"`
	PolicyVersion int64  `json:"policy_version"`
}

// HTTPError contains only the status; upstream bodies and credentials are omitted.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return "QuotaGate returned a non-success status" }

type Client struct {
	endpoint  string
	http      *http.Client
	transport *http.Transport
}

// New creates a client with a two-second timeout and no redirects or retries.
// A single client may be shared by concurrent requests from different customers.
func New(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("QuotaGate URL must be an HTTP(S) origin without credentials, query or path")
	}
	u.Path = "/v1/decisions"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 20
	transport.MaxIdleConnsPerHost = 10
	return &Client{
		endpoint:  u.String(),
		transport: transport,
		http: &http.Client{
			Transport: transport,
			Timeout:   2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Close releases idle connections. Active requests retain their own deadlines.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// Decide makes one request. The caller owns the decision ID and any future retry.
func (c *Client) Decide(ctx context.Context, key, decisionID, operation string) (Decision, error) {
	body, err := json.Marshal(struct {
		DecisionID string `json:"decision_id"`
		Operation  string `json:"operation"`
	}{decisionID, operation})
	if err != nil {
		return Decision{}, errors.New("cannot encode decision request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, errors.New("cannot create decision request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Decision{}, errors.New("QuotaGate request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Decision{}, &HTTPError{StatusCode: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return Decision{}, errors.New("cannot read QuotaGate decision")
	}
	// Pointers distinguish missing fields from an explicit false or zero value.
	var wire struct {
		Allowed       *bool  `json:"allowed"`
		Reason        string `json:"reason"`
		UTCDay        string `json:"utc_day"`
		DailyUsed     *int64 `json:"daily_used"`
		DailyLimit    *int64 `json:"daily_limit"`
		PolicyVersion *int64 `json:"policy_version"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Allowed == nil ||
		wire.DailyUsed == nil || wire.DailyLimit == nil || wire.PolicyVersion == nil {
		return Decision{}, errors.New("invalid QuotaGate decision")
	}
	if _, err := time.Parse(time.DateOnly, wire.UTCDay); err != nil ||
		*wire.DailyUsed < 0 || *wire.DailyLimit < 1 || *wire.PolicyVersion < 1 {
		return Decision{}, errors.New("invalid QuotaGate decision")
	}
	if *wire.Allowed {
		if wire.Reason != "allowed" || *wire.DailyUsed < 1 || *wire.DailyUsed > *wire.DailyLimit {
			return Decision{}, errors.New("inconsistent QuotaGate permission")
		}
	} else if wire.Reason != "daily_quota" && wire.Reason != "rate_limited" {
		return Decision{}, errors.New("unknown QuotaGate rejection")
	}
	return Decision{*wire.Allowed, wire.Reason, wire.UTCDay, *wire.DailyUsed, *wire.DailyLimit, *wire.PolicyVersion}, nil
}

func bearerKey(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") ||
		len(parts[1]) != 67 || !strings.HasPrefix(parts[1], "qg_") {
		return "", false
	}
	return parts[1], true
}
