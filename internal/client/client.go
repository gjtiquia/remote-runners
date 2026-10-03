// Package client provides the local coordinator management API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gjtiquia/remote-runners/internal/protocol"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{}}
}
func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("coordinator %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}
func (c *Client) json(ctx context.Context, method, path string, body, result any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if result == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(result)
}
func (c *Client) Submit(ctx context.Context, s protocol.Submission) (protocol.Job, error) {
	var j protocol.Job
	err := c.json(ctx, "POST", "/jobs", s, &j)
	return j, err
}
func (c *Client) Jobs(ctx context.Context) ([]protocol.Job, error) {
	var j []protocol.Job
	err := c.json(ctx, "GET", "/jobs", nil, &j)
	return j, err
}
func (c *Client) Runners(ctx context.Context) ([]protocol.RunnerInfo, error) {
	var r []protocol.RunnerInfo
	err := c.json(ctx, "GET", "/runners", nil, &r)
	return r, err
}
func (c *Client) Job(ctx context.Context, id string) (protocol.Job, error) {
	var j protocol.Job
	err := c.json(ctx, "GET", "/jobs/"+url.PathEscape(id), nil, &j)
	return j, err
}
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.json(ctx, "POST", "/jobs/"+url.PathEscape(id)+"/cancel", nil, nil)
}
func (c *Client) Output(ctx context.Context, id string, w io.Writer) error {
	resp, err := c.request(ctx, "GET", "/jobs/"+url.PathEscape(id)+"/output", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}
