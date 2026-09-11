package xunlei

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func taskFixture(id, typ, phase string) map[string]any {
	return map[string]any{"id": id, "name": "example.bin", "type": typ, "phase": phase, "space": "local-device", "params": map[string]string{"target": "local-device", "url": "https://example.org/example.bin", "parent_folder_id": "root", "total_file_count": "1"}}
}
func directoryFixture(id string) map[string]any {
	return map[string]any{"id": id, "name": "Downloads", "kind": "drive#folder", "space": "local-device", "params": map[string]string{"RealPath": "/downloads", "is_write": "true"}}
}

type testBackend struct {
	tokens  atomic.Int32
	watches atomic.Int32
	server  *httptest.Server
}

func newBackend(t *testing.T, handler http.HandlerFunc) (*Client, *testBackend) {
	t.Helper()
	backend := &testBackend{}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPrefix) {
			t.Errorf("unexpected prefix: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		if path == "/" {
			n := backend.tokens.Add(1)
			_, _ = fmt.Fprintf(w, `<script>function uiauth(value){ return "test-token-%d"; }</script>`, n)
			return
		}
		if r.Header.Get("pan-auth") == "" || r.URL.Query().Get("pan_auth") == "" {
			t.Error("missing upstream authentication")
			w.WriteHeader(401)
			return
		}
		switch path {
		case "/device/info/watch":
			backend.watches.Add(1)
			jsonReply(w, map[string]any{"target": "local-device", "is_connected": true, "user": map[string]string{"id": "private-user", "name": "private name"}, "downloads": []any{map[string]string{"path": "/downloads", "limit": "1000", "usage": "42"}}})
		case "/launcher/status":
			jsonReply(w, map[string]string{"running_version": "3.21.0"})
		default:
			handler(w, r)
		}
	}))
	t.Cleanup(backend.server.Close)
	c, err := New(Config{BaseURL: backend.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, backend
}
func rawTask(t *testing.T, v any) Task {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(v), &m); err != nil {
		t.Fatal(err)
	}
	return normalizeTask(m)
}
func errorKind(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Kind != want {
		t.Fatalf("error=%v, expected kind=%s", err, want)
	}
}

func TestNewConfiguration(t *testing.T) {
	for _, base := range []string{"", "ftp://example.org", "http://user:secret@example.org", "http://example.org/?token=x", "http://example.org/#x"} {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Errorf("accepted invalid base %q", base)
		}
	}
	c, err := New(Config{BaseURL: "https://example.org/proxy/", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if c.base.Path != "/proxy"+apiPrefix || c.http.Timeout <= 0 {
		t.Fatal("proxy path or default timeout not configured")
	}
	c, err = New(Config{BaseURL: "https://example.org" + apiPrefix + "/"})
	if err != nil || c.base.Path != apiPrefix {
		t.Fatal("full API prefix should not be duplicated")
	}
}
func TestAuthenticationConcurrentAndDevicePrivacy(t *testing.T) {
	c, backend := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, map[string]any{"tasks": []any{}, "expires_in": 5})
	})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ListTasks(ctx, ListOptions{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if backend.tokens.Load() != 1 || backend.watches.Load() != 1 {
		t.Fatalf("concurrent discovery was duplicated: tokens=%d watches=%d", backend.tokens.Load(), backend.watches.Load())
	}
	d, err := c.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Online || !d.LoggedIn || d.Version != "3.21.0" || len(d.Volumes) != 1 || *d.Volumes[0].UsedBytes != 42 {
		t.Fatalf("bad device: %+v", d)
	}
	data := string(mustJSON(d))
	for _, secret := range []string{"private-user", "private name", "test-token"} {
		if strings.Contains(data, secret) {
			t.Fatal("device response exposed private auth/account data")
		}
	}
}
func TestReadRefreshAndMutationNeverRetries(t *testing.T) {
	var reads, writes atomic.Int32
	c, b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, apiPrefix) {
		case "/drive/v1/tasks":
			if reads.Add(1) == 1 {
				jsonReply(w, map[string]string{"error": "invalid_token", "error_description": "test-token-secret"})
				return
			}
			jsonReply(w, map[string]any{"tasks": []any{}})
		case "/method/patch/drive/v1/task":
			writes.Add(1)
			w.WriteHeader(401)
		default:
			t.Error("unexpected request")
			http.NotFound(w, r)
		}
	})
	if _, err := c.ListTasks(ctx, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if b.tokens.Load() != 2 || reads.Load() != 2 {
		t.Fatal("read must refresh expired authentication once")
	}
	task := rawTask(t, taskFixture("task-a", "user#download-url", "PHASE_TYPE_RUNNING"))
	err := c.Control(ctx, task, "pause")
	errorKind(t, err, "unauthorized")
	if writes.Load() != 1 || b.tokens.Load() != 2 {
		t.Fatal("mutation was retried or eagerly refreshed")
	}
}
func TestTaskPaginationFilteringAndNormalization(t *testing.T) {
	calls := 0
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("space") != "local-device" || q.Get("limit") != "2" {
			t.Error("task query lost device/limit")
		}
		var filters map[string]map[string]string
		if json.Unmarshal([]byte(q.Get("filters")), &filters) != nil || filters["type"]["in"] != "user#download-url,user#download" {
			t.Error("missing download type filter")
		}
		if calls == 2 && q.Get("page_token") != "next +/?" {
			t.Error("cursor was not preserved")
		}
		task := taskFixture("local", "user#download-url", "PHASE_TYPE_RUNNING")
		task["file_size"] = "200"
		task["progress"] = 100
		task["future_field"] = map[string]int{"hello": 7}
		task["params"] = map[string]string{"target": "local-device", "speed": "12", "download_size": "200", "checked_size": "50"}
		foreign := taskFixture("foreign", "user#download-url", "PHASE_TYPE_RUNNING")
		foreign["space"] = "other-device"
		jsonReply(w, map[string]any{"tasks": []any{task, taskFixture("cloud", "user#runner", "PHASE_TYPE_COMPLETE"), foreign}, "next_page_token": "next +/?", "expires_in": "5"})
	})
	page, err := c.ListTasks(ctx, ListOptions{Status: "running", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.ExpiresIn != 5 || page.NextPageToken != "next +/?" {
		t.Fatalf("bad page %+v", page)
	}
	task := page.Tasks[0]
	if task.Status != "running" || task.Progress == nil || *task.Progress != 25 || task.SpeedBytesPerSecond != 12 || task.ObservedAt.IsZero() {
		t.Fatalf("bad normalized task %+v", task)
	}
	if len(task.Raw["future_field"]) == 0 || strings.Contains(string(mustJSON(task)), "future_field") {
		t.Fatal("unknown metadata must be retained internally only")
	}
	if _, err = c.ListTasks(ctx, ListOptions{Limit: 2, Cursor: page.NextPageToken}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ListTasks(ctx, ListOptions{Status: "bogus"}); err == nil {
		t.Fatal("invalid status accepted")
	}
}
func TestMissingTaskFieldsAndCompletion(t *testing.T) {
	task := rawTask(t, taskFixture("a", "user#download", "PHASE_TYPE_RUNNING"))
	if task.Progress != nil || task.SizeBytes != nil || task.DownloadedBytes != nil || task.SpeedBytesPerSecond != 0 {
		t.Fatal("missing fields were invented")
	}
	for _, phase := range []string{"PHASE_TYPE_RUNNING", "PHASE_TYPE_COMPLETE", "PHASE_TYPE_FUTURE"} {
		v := taskFixture("a", "user#download", phase)
		v["progress"] = 100
		task = rawTask(t, v)
		if phase == "PHASE_TYPE_RUNNING" && task.Status == "complete" {
			t.Fatal("progress 100 cannot imply completion")
		}
		if phase == "PHASE_TYPE_FUTURE" && task.Status != "unknown" {
			t.Fatal("unknown phase was not preserved")
		}
	}
	if floatValue(json.RawMessage(`"NaN"`)) != nil {
		t.Fatal("nonfinite progress accepted")
	}
}
func TestControlAndDeleteProtocols(t *testing.T) {
	var actions []string
	deleteCalls := 0
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		switch path {
		case "/drive/v1/tasks":
			jsonReply(w, map[string]any{"tasks": []any{taskFixture("task-a", "user#download", "PHASE_TYPE_RUNNING")}})
		case "/method/patch/drive/v1/task":
			if r.Method != "POST" {
				t.Error("logical PATCH was not tunneled through POST")
			}
			var body struct {
				Space, Type, ID string
				SetParams       map[string]string `json:"set_params"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid body")
			}
			if body.Space != "local-device" || body.Type != "user#download" || body.ID != "task-a" {
				t.Error("control lost real task type/ownership")
			}
			var spec map[string]string
			if json.Unmarshal([]byte(body.SetParams["spec"]), &spec) != nil {
				t.Error("spec must be a JSON string")
			}
			actions = append(actions, spec["phase"])
			jsonReply(w, map[string]int{"HttpStatus": 0})
		case "/method/delete/drive/v1/tasks":
			deleteCalls++
			if r.Method != "POST" || r.URL.Query().Get("space") != "local-device" || len(r.URL.Query()["task_ids"]) != 1 || r.URL.Query().Get("task_ids") != "task-a" {
				t.Error("wrong record-delete protocol")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != "{}" {
				t.Error("record delete body must be an empty object")
			}
			jsonReply(w, map[string]any{})
		default:
			t.Errorf("unexpected route %s", path)
			http.NotFound(w, r)
		}
	})
	task := rawTask(t, taskFixture("task-a", "user#download", "PHASE_TYPE_RUNNING"))
	for _, action := range []string{"pause", "resume", "delete"} {
		if err := c.Control(ctx, task, action); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(actions, ",") != "pause,running,delete" {
		t.Fatalf("wrong commands: %v", actions)
	}
	if err := c.DeleteRecord(ctx, "task-a"); err != nil {
		t.Fatal(err)
	}
	if deleteCalls != 1 {
		t.Fatal("record deletion was not sent")
	}
	task.Status = "error"
	if err := c.Control(ctx, task, "resume"); err == nil {
		t.Fatal("failed task was silently resumed")
	}
	task.Params["target"] = "other"
	if err := c.Control(ctx, task, "pause"); err == nil {
		t.Fatal("foreign task accepted")
	}
}
func TestTaskLookupDoesNotExposeOtherTasks(t *testing.T) {
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, map[string]any{"tasks": []any{taskFixture("different", "user#download-url", "PHASE_TYPE_COMPLETE")}})
	})
	_, err := c.Task(ctx, "missing")
	errorKind(t, err, "not_found")
	_, err = c.Task(ctx, "one,two")
	errorKind(t, err, "invalid")
}

func TestBusinessErrorsAndUntrustedMessages(t *testing.T) {
	cases := []struct{ body, kind string }{
		{`{"error":"invalid_token","error_description":"secret-token"}`, "unauthorized"},
		{`{"code":123,"message":"https://secret:password@upstream?pan_auth=secret-token"}`, "upstream"},
		{`{"HttpStatus":403}`, "unauthorized"},
		{`{"error":{"message":"secret-token"}}`, "upstream"},
		{`{"success":false}`, "upstream"},
		{`{"error":"task_not_found"}`, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+tc.body[:8], func(t *testing.T) {
			_, err := decodeResponse([]byte(tc.body), false)
			errorKind(t, err, tc.kind)
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") {
				t.Fatal("upstream message leaked credentials")
			}
		})
	}
	for _, body := range []string{`{}`, `{"code":0}`, `{"HttpStatus":0}`, `{"error":""}`, `{"success":true}`} {
		if _, err := decodeResponse([]byte(body), true); err != nil {
			t.Error(err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestNetworkErrorsAreRedactedAndMutationOutcomeUnknown(t *testing.T) {
	c, err := New(Config{BaseURL: "http://example.org", HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("failed %s secret-password", r.URL.String())
	})}})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []bool{false, true} {
		_, err = c.exchange(ctx, "POST", "/drive/v1/task", "super-secret-token", nil, []byte("{}"), "application/json", mutation)
		kind := "unavailable"
		if mutation {
			kind = "unknown_outcome"
		}
		errorKind(t, err, kind)
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "http") {
			t.Fatal("network error leaked URL/auth data")
		}
	}
}
func TestCrossOriginRedirectDoesNotLeakCredentials(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("cross-origin redirect followed") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := New(Config{BaseURL: source.URL, Username: "basic-user", Password: "basic-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.exchange(ctx, "GET", "/", "pan-secret", nil, nil, "", false)
	errorKind(t, err, "upstream")
	if calls.Load() != 0 {
		t.Fatal("credentials crossed origins")
	}
}
func TestHTTPBasicAuthAndRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != "password" {
			t.Error("Basic Auth missing")
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, Username: "user", Password: "password", HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.exchange(ctx, "POST", "/drive/v1/task", "token", nil, []byte("{}"), "application/json", true)
	errorKind(t, err, "unknown_outcome")
}
func TestResponseLimitsAndMalformedMutation(t *testing.T) {
	c, err := New(Config{BaseURL: "http://example.org", HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(io.LimitReader(infiniteByteReader{}, maxResponseBytes+1)), Header: make(http.Header)}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.exchange(ctx, "POST", "/drive/v1/task", "token", nil, nil, "", true)
	errorKind(t, err, "unknown_outcome")
	_, err = decodeResponse([]byte("<html>token</html>"), true)
	errorKind(t, err, "unknown_outcome")
}

type infiniteByteReader struct{}

func (infiniteByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestDirectoryQueriesAndCreation(t *testing.T) {
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		switch {
		case path == "/drive/v1/files/root":
			if r.URL.Query().Get("space") != "local-device" {
				t.Error("directory check lost device")
			}
			jsonReply(w, map[string]any{"file": directoryFixture("root")})
		case path == "/drive/v1/files" && r.Method == "GET":
			if r.URL.Query().Get("page_token") != "cursor+/" || len(r.URL.Query()["with"]) != 2 {
				t.Error("directory pagination/context missing")
			}
			jsonReply(w, map[string]any{"files": []any{directoryFixture("root")}, "next_page_token": "next"})
		case path == "/drive/v1/files" && r.Method == "POST":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["parent_id"] != "root" || body["name"] != "new folder" || body["space"] != "local-device" || body["kind"] != "drive#folder" {
				t.Error("bad directory creation body")
			}
			jsonReply(w, map[string]any{"file": directoryFixture("new")})
		default:
			http.NotFound(w, r)
		}
	})
	page, err := c.Directories(ctx, "", "cursor+/", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Directories) != 1 || !page.Directories[0].Writable || page.NextPageToken != "next" {
		t.Fatal("bad directory page")
	}
	dir, err := c.CreateDirectory(ctx, "root", "new folder")
	if err != nil || dir.ID != "new" {
		t.Fatalf("create dir: %v %+v", err, dir)
	}
	for _, name := range []string{"", "..", "a/b", "a\\b"} {
		if _, err = c.CreateDirectory(ctx, "root", name); err == nil {
			t.Fatal("unsafe directory name accepted")
		}
	}
}

func TestCreateTaskNestedSelectionAndMissingSize(t *testing.T) {
	writes := 0
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, apiPrefix) {
		case "/drive/v1/files/root":
			jsonReply(w, directoryFixture("root"))
		case "/drive/v1/resource/list":
			jsonReply(w, map[string]any{"list_id": "resolved", "list": map[string]any{"resources": []any{map[string]any{"id": "resource", "name": "bundle", "file_count": "3", "dir": map[string]any{"resources": []any{map[string]any{"name": "a", "file_index": 0}, map[string]any{"name": "nested", "file_count": 2, "dir": map[string]any{"resources": []any{map[string]any{"name": "b", "file_index": 7}, map[string]any{"name": "c", "file_index": 12}}}}}}}}}})
		case "/drive/v1/task":
			writes++
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			params := paramsValue(body["params"])
			if scalar(body["file_size"]) != "0" || params["sub_file_index"] != "0,7" || params["target"] != "local-device" || params["parent_folder_id"] != "root" || params["total_file_count"] != "3" {
				t.Errorf("bad creation fields: %s", mustJSON(body))
			}
			jsonReply(w, map[string]any{"task": map[string]string{"id": "created"}})
		default:
			http.NotFound(w, r)
		}
	})
	task, err := c.CreateTask(ctx, CreateRequest{URL: "magnet:?xt=urn:btih:abcdef", DestinationID: "root", FileIndices: []int{7, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "created" || task.Type != "user#download-url" || task.DestinationID != "root" || task.Params["sub_file_index"] != "0,7" || task.Status != "unknown" || writes != 1 {
		t.Fatalf("bad creation result %+v", task)
	}
	_, err = c.CreateTask(ctx, CreateRequest{URL: "https://example.org/file", DestinationID: "root", FileIndices: []int{}})
	errorKind(t, err, "invalid")
	if writes != 1 {
		t.Fatal("empty selection created a task")
	}
}
func TestDefaultAllDoesNotInventFileIndices(t *testing.T) {
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, apiPrefix) {
		case "/drive/v1/files/root":
			jsonReply(w, directoryFixture("root"))
		case "/drive/v1/resource/list":
			jsonReply(w, map[string]any{"list": map[string]any{"resources": []any{map[string]any{"name": "single.bin"}}}})
		case "/drive/v1/task":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, exists := paramsValue(body["params"])["sub_file_index"]; exists {
				t.Error("all files should omit selection when indices are absent")
			}
			jsonReply(w, map[string]any{"task": map[string]string{"id": "created"}})
		default:
			http.NotFound(w, r)
		}
	})
	task, err := c.CreateTask(ctx, CreateRequest{URL: "https://example.org/file", DestinationID: "root"})
	if err != nil || task.ID != "created" {
		t.Fatalf("default-all failed: %+v %v", task, err)
	}
}
func TestSelectionRejectsIncompleteOrAmbiguousTrees(t *testing.T) {
	zero, seven := 0, 7
	for _, tc := range []struct {
		res     Resolution
		indices []int
	}{
		{Resolution{Complete: false, Resources: []Resource{{FileIndex: &zero}}}, []int{0}},
		{Resolution{Complete: true, Resources: []Resource{{}}}, []int{0}},
		{Resolution{Complete: true, Resources: []Resource{{FileIndex: &zero}, {FileIndex: &zero}}}, []int{0}},
		{Resolution{Complete: true, Resources: []Resource{{FileIndex: &zero}}}, []int{7}},
		{Resolution{Complete: true, Resources: []Resource{{FileIndex: &seven}}}, []int{7, 7}},
	} {
		if _, err := selectedIndices(tc.res, tc.indices); err == nil {
			t.Error("unsafe file selection accepted")
		}
	}
	raw := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(`{"name":"bundle","file_count":2,"dir":{"resources":[{"name":"one","file_index":0}],"next_page_token":"more"}}`), &raw)
	_, _, complete, err := parseResource(raw)
	if err != nil || complete {
		t.Fatal("resource pagination was silently accepted as complete")
	}
}
func TestDestinationReadOnlyOrForeignRejected(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
				d := directoryFixture("root")
				if foreign {
					d["space"] = "other"
				} else {
					d["params"] = map[string]string{"is_write": "false"}
				}
				jsonReply(w, d)
			})
			_, err := c.CreateTask(ctx, CreateRequest{URL: "https://example.org/a", DestinationID: "root"})
			errorKind(t, err, "invalid")
		})
	}
}
func TestTorrentUploadPreservesOriginalBytes(t *testing.T) {
	torrent := []byte("d8:announce19:https://tracker.test4:infodee")
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, apiPrefix) {
		case "/device/btinfo":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			f, h, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if h.Filename != "nested.torrent" || !bytes.Equal(data, torrent) || r.FormValue("pan-auth") == "" {
				t.Error("torrent bytes/filename/auth were lost")
			}
			jsonReply(w, map[string]string{"url": "magnet:?xt=urn:btih:abcdef"})
		case "/drive/v1/resource/list":
			jsonReply(w, map[string]any{"list_id": "torrent-resource", "list": map[string]any{"resources": []any{map[string]any{"name": "file.bin", "file_index": 0}}}})
		default:
			http.NotFound(w, r)
		}
	})
	resolution, err := c.UploadTorrent(ctx, "nested.torrent", bytes.NewReader(torrent))
	if err != nil || resolution.ID != "torrent-resource" {
		t.Fatalf("upload: %+v %v", resolution, err)
	}
	if _, err = c.UploadTorrent(ctx, "bad.exe", bytes.NewReader(torrent)); err == nil {
		t.Fatal("non-torrent filename accepted")
	}
}
func TestRecreatePreservesSavedParametersWithoutDeleting(t *testing.T) {
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, apiPrefix) {
		case "/drive/v1/files/root":
			jsonReply(w, directoryFixture("root"))
		case "/drive/v1/task":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			params := paramsValue(body["params"])
			if scalar(body["type"]) != "user#download" || params["file_id"] != "cloud-file" || params["sub_file_index"] != "0,7" || params["parent_folder_id"] != "root" {
				t.Error("retry lost saved fields")
			}
			jsonReply(w, map[string]any{"task": map[string]string{"id": "replacement"}})
		default:
			t.Errorf("retry must not delete or re-resolve source: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	fixture := taskFixture("old", "user#download", "PHASE_TYPE_ERROR")
	fixture["params"] = map[string]string{"target": "local-device", "file_id": "cloud-file", "sub_file_index": "0,7", "parent_folder_id": "root", "total_file_count": "2"}
	task, err := c.RecreateTask(ctx, rawTask(t, fixture))
	if err != nil || task.ID != "replacement" {
		t.Fatalf("retry failed %+v %v", task, err)
	}
}
func TestEscapedFileIDAndCursor(t *testing.T) {
	id := "abc/def+=?"
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.EscapedPath(), url.PathEscape(id)) {
			t.Errorf("file ID not safely encoded: %s", r.URL.EscapedPath())
		}
		jsonReply(w, directoryFixture(id))
	})
	if _, err := c.requireDirectory(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestMutationRedirectNeverReplaysRequest(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		http.Redirect(w, r, apiPrefix+"/replayed", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.exchange(ctx, http.MethodPost, "/drive/v1/task", "token", nil, []byte("{}"), "application/json", true)
	errorKind(t, err, "unknown_outcome")
	if writes.Load() != 1 {
		t.Fatal("a redirect automatically replayed a mutation")
	}
}

func TestReadAuthenticationRetryIsBounded(t *testing.T) {
	var calls atomic.Int32
	c, b := newBackend(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusUnauthorized) })
	_, err := c.ListTasks(ctx, ListOptions{})
	errorKind(t, err, "unauthorized")
	if calls.Load() != 2 || b.tokens.Load() != 2 {
		t.Fatal("read auth recovery must stop after one refresh")
	}
}

func TestLiveEmptyTaskEnvelope(t *testing.T) {
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, map[string]any{"HttpStatus": 200, "expires_in": 60})
	})
	page, err := c.ListTasks(ctx, ListOptions{Status: "active"})
	if err != nil || len(page.Tasks) != 0 || page.Tasks == nil || page.NextPageToken != "" || page.ExpiresIn != 60 {
		t.Fatalf("empty live envelope was not normalized: %+v %v", page, err)
	}
	_, err = c.Task(ctx, "missing")
	errorKind(t, err, "not_found")
}

func TestMissingTaskArrayRequiresSuccessfulEnvelope(t *testing.T) {
	for _, body := range []string{`{}`, `{"expires_in":60}`, `{"HttpStatus":200}`, `{"HttpStatus":200,"expires_in":"invalid"}`} {
		t.Run(body, func(t *testing.T) {
			c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
			_, err := c.Task(ctx, "missing")
			errorKind(t, err, "upstream")
		})
	}
}

func TestNetworkReasonHasFixedRedactedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{io.EOF, "eof"},
		{fmt.Errorf("secret-token context: %w", io.ErrUnexpectedEOF), "unexpected_eof"},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "request_canceled"},
		{errors.New("secret URL http://user:secret@server?pan_auth=secret"), "transport_error"},
	} {
		wrapped := &url.Error{Op: "Post", URL: "http://user:secret@server?pan_auth=secret", Err: tc.err}
		if reason := networkReason(wrapped); reason != tc.want {
			t.Errorf("reason=%s want=%s", reason, tc.want)
		}
		err := networkFailure(true, wrapped)
		if !strings.Contains(err.Message, "("+tc.want+")") || strings.Contains(err.Message, "secret") || strings.Contains(err.Message, "http:") {
			t.Fatal("network message omitted safe reason or exposed unsafe context")
		}
	}
}

func TestPreparedRetrySurvivesDeletedOldTaskWithoutScopeFields(t *testing.T) {
	deleted := false
	reads, directoryReads, writes := 0, 0, 0
	original := taskFixture("old", "user#download-url", "PHASE_TYPE_ERROR")
	delete(original, "space")
	original["params"] = map[string]string{"url": "https://example.org/original.bin", "parent_folder_id": "root", "total_file_count": "1"}
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, apiPrefix) {
		case "/drive/v1/tasks":
			reads++
			if deleted {
				jsonReply(w, map[string]any{"HttpStatus": 200, "expires_in": 60})
			} else {
				jsonReply(w, map[string]any{"tasks": []any{original}})
			}
		case "/drive/v1/files/root":
			directoryReads++
			jsonReply(w, directoryFixture("root"))
		case "/drive/v1/task":
			writes++
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			params := paramsValue(body["params"])
			if params["url"] != "https://example.org/original.bin" || params["parent_folder_id"] != "root" || params["target"] != "local-device" {
				t.Error("prepared retry payload was modified through caller metadata")
			}
			jsonReply(w, map[string]any{"task": map[string]string{"id": "replacement"}})
		default:
			t.Error("unexpected retry request")
			http.NotFound(w, r)
		}
	})
	observed, err := c.Task(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareRetry(ctx, observed)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Params["target"] != "" || prepared.Params["target"] != "local-device" {
		t.Fatal("retry snapshot did not bind scope without mutating the original")
	}
	deleted = true
	beforeReads, beforeDirectoryReads := reads, directoryReads
	prepared.Params["url"] = "https://example.org/modified.bin"
	prepared.DestinationID = "different"
	replacement, err := c.RecreateTask(ctx, prepared)
	if err != nil || replacement.ID != "replacement" {
		t.Fatalf("prepared recreation failed: %+v %v", replacement, err)
	}
	if reads != beforeReads || directoryReads != beforeDirectoryReads || writes != 1 {
		t.Fatal("prepared retry queried deleted ID or repeated preflight")
	}
}

func TestPrepareRetryRejectsInvalidMetadataBeforeDeletion(t *testing.T) {
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("retry validation must never write")
		}
		jsonReply(w, directoryFixture("root"))
	})
	for _, mutate := range []func(*Task){
		func(task *Task) { task.URL = "file:///not-a-download" },
		func(task *Task) { task.Name = "../unsafe" },
		func(task *Task) { task.Params["total_file_count"] = "not-a-number" },
		func(task *Task) { task.Params["sub_file_index"] = "0-3,invalid" },
		func(task *Task) { task.Type = "user#download"; task.Params["file_id"] = "" },
	} {
		task := rawTask(t, taskFixture("old", "user#download-url", "PHASE_TYPE_ERROR"))
		mutate(&task)
		_, err := c.PrepareRetry(ctx, task)
		errorKind(t, err, "invalid")
	}
}

func TestPreparedRetryRejectsChangedDevice(t *testing.T) {
	writes := 0
	c, _ := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writes++
		}
		jsonReply(w, directoryFixture("root"))
	})
	prepared, err := c.PrepareRetry(ctx, rawTask(t, taskFixture("old", "user#download-url", "PHASE_TYPE_ERROR")))
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.device.ID = "different-device"
	c.mu.Unlock()
	_, err = c.RecreateTask(ctx, prepared)
	errorKind(t, err, "unavailable")
	if writes != 0 {
		t.Fatal("retry was sent to a different device")
	}
}

func TestSavedSelectionValidation(t *testing.T) {
	for _, value := range []string{"", "0", "0,7", "0-3,7-9", "-1", "--1,"} {
		if !validSavedSelection(value) {
			t.Errorf("rejected supported saved selection %q", value)
		}
	}
	for _, value := range []string{" ", "0,", "a", "2-1", "0-1-2", "-2", "1;2"} {
		if validSavedSelection(value) {
			t.Errorf("accepted unsafe saved selection %q", value)
		}
	}
}
