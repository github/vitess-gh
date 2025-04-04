/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workflow

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"vitess.io/vitess/go/vt/topo/memorytopo"
)

func TestLongPolling(t *testing.T) {
	ts := memorytopo.NewServer("cell1")
	m := NewManager(ts)

	// Register the manager to a web handler, start a web server.
	m.HandleHTTPLongPolling("/workflow")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Cannot listen: %v", err)
	}
	go http.Serve(listener, nil)

	// Run the manager in the background.
	wg, _, cancel := StartManager(m)

	// Get the original tree with a 'create'.
	u := url.URL{Scheme: "http", Host: listener.Addr().String(), Path: "/workflow/create"}
	resp, err := http.Get(u.String())
	if err != nil {
		t.Fatalf("/create failed: %v", err)
	}
	tree, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("/create reading failed: %v", err)
	}
	if string(tree) != `{"index":1,"fullUpdate":true}` {
		t.Errorf("unexpected first result: %v", string(tree))
	}

	// Add a node, make sure we get the update with the next poll
	tw := &testWorkflow{}
	n := &Node{
		Listener: tw,

		Name:        "name",
		PathName:    "uuid1",
		Children:    []*Node{},
		LastChanged: 143,
	}
	if err := m.NodeManager().AddRootNode(n); err != nil {
		t.Fatalf("adding root node failed: %v", err)
	}

	u.Path = "/workflow/poll/1"
	resp, err = http.Get(u.String())
	if err != nil {
		t.Fatalf("/poll/1 failed: %v", err)
	}
	tree, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("/poll/1 reading failed: %v", err)
	}
	if !strings.Contains(string(tree), `"name":"name"`) ||
		!strings.Contains(string(tree), `"path":"/uuid1"`) {
		t.Errorf("unexpected first result: %v", string(tree))
	}

	// Trigger an action, make sure it goes through.
	u.Path = "/workflow/action/1"
	message := `{"path":"/uuid1","name":"button1"}`
	buf := bytes.NewReader([]byte(message))
	pResp, err := http.Post(u.String(), "application/json; charset=utf-8", buf)
	if err != nil {
		t.Fatalf("/action/1 post failed: %v", err)
	}
	pResp.Body.Close()
	for timeout := 0; ; timeout++ {
		// This is an asynchronous action, need to take the lock.
		tw.mu.Lock()
		if len(tw.actions) == 1 && tw.actions[0].Path == n.Path && tw.actions[0].Name == "button1" {
			tw.mu.Unlock()
			break
		}
		tw.mu.Unlock()
		timeout++
		if timeout == 1000 {
			t.Fatalf("failed to wait for action")
		}
		time.Sleep(time.Millisecond)
	}

	// Send an update, make sure we see it.
	n.Name = "name2"
	n.BroadcastChanges(false /* updateChildren */)

	u.Path = "/workflow/poll/1"
	resp, err = http.Get(u.String())
	if err != nil {
		t.Fatalf("/poll/1 failed: %v", err)
	}
	tree, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("/poll/1 reading failed: %v", err)
	}
	if !strings.Contains(string(tree), `"name":"name2"`) {
		t.Errorf("unexpected update result: %v", string(tree))
	}

	// Stop the manager.
	cancel()
	wg.Wait()
}

func TestSanitizeRequestHeader(t *testing.T) {
	testCases := []struct {
		name                string
		sanitizeHTTPHeaders bool
		reqMethod           string
		reqURL              string
		reqHeader           map[string]string
		wantHeader          http.Header
	}{
		{
			name:                "withCookie-and-sanitize",
			sanitizeHTTPHeaders: true,
			reqMethod:           "GET",
			reqURL:              "https://vtctld-dev-xyz.company.com/api/workflow/poll/11",
			reqHeader: map[string]string{
				"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp",
				"Cookie":     "_ga=GA1.2.1234567899.1234567890; machine-cookie=username:123456789:1234f999ff99aa1af3ae9999d2a5d3968ac014a0947ae1418b10642ab2cbb7d3; session=7e5dc76807be39dd0886b9425883dabe3fd2432f7415e2a08fba7ca43ac4d0dc;",
				"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36",
			},
			wantHeader: map[string][]string{
				"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp"},
				"Cookie":     {HTTPHeaderRedactedMessage},
				"User-Agent": {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36"},
			},
		},
		{
			name:                "withCookie-not-sanitize",
			sanitizeHTTPHeaders: false,
			reqMethod:           "GET",
			reqURL:              "https://vtctld-dev-xyz.company.com/api/workflow/poll/11",
			reqHeader: map[string]string{
				"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp",
				"Cookie":     "_ga=GA1.2.1234567899.1234567890; machine-cookie=username:123456789:1234f999ff99aa1af3ae9999d2a5d3968ac014a0947ae1418b10642ab2cbb7d3; session=7e5dc76807be39dd0886b9425883dabe3fd2432f7415e2a08fba7ca43ac4d0dc;",
				"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36",
			},
			wantHeader: map[string][]string{
				"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp"},
				"Cookie":     {"_ga=GA1.2.1234567899.1234567890; machine-cookie=username:123456789:1234f999ff99aa1af3ae9999d2a5d3968ac014a0947ae1418b10642ab2cbb7d3; session=7e5dc76807be39dd0886b9425883dabe3fd2432f7415e2a08fba7ca43ac4d0dc;"},
				"User-Agent": {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36"},
			},
		},
		{
			name:                "noCookie-sanitize",
			sanitizeHTTPHeaders: true,
			reqMethod:           "GET",
			reqURL:              "https://vtctld-dev-xyz.company.com/api/workflow/poll/11",
			reqHeader: map[string]string{
				"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp",
				"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36",
			},
			wantHeader: map[string][]string{
				"Accept":     {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp"},
				"User-Agent": {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36"},
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			req, err := http.NewRequestWithContext(ctx, tc.reqMethod, tc.reqURL, nil)
			if err != nil {
				t.Fatalf("can't parse test http req: %v", err)
			}

			for k, v := range tc.reqHeader {
				req.Header.Set(k, v)
			}
			clone := req.Clone(ctx)

			if got := sanitizeRequestHeader(req, tc.sanitizeHTTPHeaders); !reflect.DeepEqual(got.Header, tc.wantHeader) {
				t.Errorf("sanitizeRequestHeader() failed\nwant: %#v\ngot: %#v", tc.wantHeader, got.Header)
			}

			if !reflect.DeepEqual(clone, req) {
				t.Errorf("sanitizeRequestHeader() corrupted original http request\nwant: %#v\ngot: %#v", clone, req)
			}
		})
	}
}
