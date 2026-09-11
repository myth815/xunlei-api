package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myth815/xunlei-api/internal/store"
	"github.com/myth815/xunlei-api/internal/xunlei"
)

const testAPIKey = "0123456789abcdef0123456789abcdef"

type backendCall struct{ method, id, action string }

type mockBackend struct {
	mu             sync.Mutex
	tasks          map[string]xunlei.Task
	calls          []backendCall
	device         xunlei.Device
	createError    error
	controlError   error
	deleteError    error
	prepareError   error
	recreateError  error
	afterCreate    func()
	directories    map[string][]xunlei.Directory
	directoryPages map[string]map[string]xunlei.DirectoryPage
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		tasks:  map[string]xunlei.Task{},
		device: xunlei.Device{ID: "local-device", Online: true, LoggedIn: true},
		directories: map[string][]xunlei.Directory{
			"":     {{ID: "root", Name: "迅雷下载", Path: "/downloads", Writable: true}},
			"root": {{ID: "movies", Name: "电影", Writable: true}, {ID: "shows", Name: "剧集", Writable: true}},
		},
		directoryPages: map[string]map[string]xunlei.DirectoryPage{},
	}
}

func (m *mockBackend) count(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, call := range m.calls {
		if call.method == method {
			n++
		}
	}
	return n
}

func (m *mockBackend) setTask(task xunlei.Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[task.ID] = task
}

func (m *mockBackend) removeTask(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, id)
}

func (m *mockBackend) Device(context.Context) (xunlei.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "device"})
	return m.device, nil
}

func (m *mockBackend) ListTasks(context.Context, xunlei.ListOptions) (xunlei.TaskPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "list"})
	page := xunlei.TaskPage{Tasks: []xunlei.Task{}}
	for _, task := range m.tasks {
		page.Tasks = append(page.Tasks, task)
	}
	return page, nil
}

func (m *mockBackend) Task(_ context.Context, id string) (xunlei.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "task", id: id})
	if task, ok := m.tasks[id]; ok {
		return task, nil
	}
	return xunlei.Task{}, &xunlei.Error{Kind: "not_found", Message: "task not found"}
}

func (m *mockBackend) Directories(_ context.Context, parent, cursor string, _ int) (xunlei.DirectoryPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "directories", id: parent, action: cursor})
	if pages := m.directoryPages[parent]; pages != nil {
		if page, ok := pages[cursor]; ok {
			return page, nil
		}
	}
	return xunlei.DirectoryPage{Directories: append([]xunlei.Directory(nil), m.directories[parent]...)}, nil
}

func (m *mockBackend) CreateDirectory(_ context.Context, parent, name string) (xunlei.Directory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "create_directory", id: parent})
	return xunlei.Directory{ID: "new-directory", Name: name, Writable: true}, nil
}

func (m *mockBackend) Resolve(_ context.Context, url string) (xunlei.Resolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "resolve"})
	return xunlei.Resolution{ID: "resolution", URL: url, Complete: true}, nil
}

func (m *mockBackend) UploadTorrent(context.Context, string, io.Reader) (xunlei.Resolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "torrent"})
	return xunlei.Resolution{ID: "torrent-resolution", Complete: true}, nil
}

func (m *mockBackend) CreateTask(_ context.Context, request xunlei.CreateRequest) (xunlei.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "create", id: request.DestinationID})
	if m.createError != nil {
		return xunlei.Task{}, m.createError
	}
	task := xunlei.Task{ID: "created-task", Status: "pending", URL: request.URL, DestinationID: request.DestinationID}
	m.tasks[task.ID] = task
	if m.afterCreate != nil {
		m.afterCreate()
	}
	return task, nil
}

func (m *mockBackend) Control(_ context.Context, task xunlei.Task, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "control", id: task.ID, action: action})
	return m.controlError
}

func (m *mockBackend) DeleteRecord(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "delete", id: id})
	if m.deleteError != nil {
		return m.deleteError
	}
	delete(m.tasks, id)
	return nil
}

func (m *mockBackend) RecreateTask(_ context.Context, task xunlei.Task) (xunlei.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "recreate", id: task.ID})
	if m.recreateError != nil {
		return xunlei.Task{}, m.recreateError
	}
	task.ID, task.Status = "replacement-task", "pending"
	m.tasks[task.ID] = task
	return task, nil
}

func (m *mockBackend) PrepareRetry(_ context.Context, task xunlei.Task) (xunlei.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, backendCall{method: "prepare_retry", id: task.ID})
	if m.prepareError != nil {
		return xunlei.Task{}, m.prepareError
	}
	params := make(map[string]string, len(task.Params)+1)
	for key, value := range task.Params {
		params[key] = value
	}
	params["target"] = m.device.ID
	task.Params = params
	return task, nil
}

func testServer(t *testing.T, backend *mockBackend, cfg Config) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if cfg.APIKey == "" {
		cfg.APIKey = testAPIKey
	}
	s, err := New(cfg, backend, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, st
}

func request(s *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		if value == "" {
			r.Header.Del(name)
		} else {
			r.Header.Set(name, value)
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func responseOperation(t *testing.T, response *httptest.ResponseRecorder) store.Operation {
	t.Helper()
	var envelope struct {
		Operation store.Operation `json:"operation"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Operation.ID == "" {
		t.Fatalf("invalid operation response: status=%d, body=%s, err=%v", response.Code, response.Body, err)
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`"details"`)) {
		t.Fatalf("internal operation details were exposed: %s", response.Body)
	}
	return envelope.Operation
}

func TestAuthenticationRejectsMissingWrongAndQueryKeys(t *testing.T) {
	b := newMockBackend()
	s, _ := testServer(t, b, Config{})
	for _, tc := range []struct {
		path, auth string
	}{
		{"/v1/device", ""},
		{"/openapi.yaml", ""},
		{"/v1/device?api_key=" + testAPIKey, ""},
		{"/v1/device?access_token=" + testAPIKey, ""},
		{"/v1/device", "Bearer wrong-key"},
		{"/v1/device", "Basic " + testAPIKey},
	} {
		got := request(s, "GET", tc.path, "", map[string]string{"Authorization": tc.auth})
		if got.Code != 401 || got.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%s with %q: got %d %s", tc.path, tc.auth, got.Code, got.Body)
		}
	}
	if b.count("device") != 0 {
		t.Fatal("unauthenticated request reached backend")
	}
	if got := request(s, "GET", "/healthz", "", map[string]string{"Authorization": ""}); got.Code != 200 {
		t.Fatalf("health check requires authentication: %d", got.Code)
	}
	got := request(s, "GET", "/v1/device", "", nil)
	if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" || got.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("authenticated response: %d %v", got.Code, got.Header())
	}
}

func TestAPIKeyAndOriginConfiguration(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, key := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 513), " " + testAPIKey, testAPIKey + "\n"} {
		if s, err := New(Config{APIKey: key}, newMockBackend(), st); err == nil {
			s.Close()
			t.Fatalf("accepted invalid API key of length %d", len(key))
		}
	}
	for _, origin := range []string{"*", "https://*.example.org", "https://example.org/path", "https://user@example.org", "https://example.org?token=x", "null"} {
		if s, err := New(Config{APIKey: testAPIKey, AllowedOrigins: []string{origin}}, newMockBackend(), st); err == nil {
			s.Close()
			t.Fatalf("accepted invalid origin %q", origin)
		}
	}
	for _, destination := range []string{"relative/path", "/", "/迅雷下载/../其他", "/bad\\name"} {
		if s, err := New(Config{APIKey: testAPIKey, DefaultDestinationPath: destination}, newMockBackend(), st); err == nil {
			s.Close()
			t.Fatalf("accepted invalid default destination %q", destination)
		}
	}
}

func TestFriendlyDirectoryPathsAndDefaultDestination(t *testing.T) {
	b := newMockBackend()
	s, _ := testServer(t, b, Config{})

	response := request(s, "GET", "/v1/directories", "", nil)
	var root xunlei.DirectoryPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &root) != nil || root.ParentPath != "/" || len(root.Directories) != 1 || root.Directories[0].DisplayPath != "/迅雷下载" {
		t.Fatalf("root display paths: %d %s", response.Code, response.Body)
	}
	response = request(s, "GET", "/v1/directories?path=%2F%E8%BF%85%E9%9B%B7%E4%B8%8B%E8%BD%BD", "", nil)
	var children xunlei.DirectoryPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &children) != nil || children.ParentPath != "/迅雷下载" || len(children.Directories) != 2 || children.Directories[0].DisplayPath != "/迅雷下载/电影" {
		t.Fatalf("child display paths: %d %s", response.Code, response.Body)
	}

	response = request(s, "POST", "/v1/tasks", `{"url":"https://example.org/file","destination_path":"/迅雷下载/电影"}`, map[string]string{"Idempotency-Key": "path-task"})
	op := responseOperation(t, response)
	var task xunlei.Task
	if response.Code != 201 || json.Unmarshal(op.Result, &task) != nil || task.DestinationID != "movies" {
		t.Fatalf("path task: %d %+v %s", response.Code, task, response.Body)
	}

	defaultServer, _ := testServer(t, newMockBackend(), Config{DefaultDestinationPath: "/迅雷下载/剧集"})
	response = request(defaultServer, "POST", "/v1/tasks", `{"url":"https://example.org/default"}`, map[string]string{"Idempotency-Key": "default-task"})
	op = responseOperation(t, response)
	if response.Code != 201 || json.Unmarshal(op.Result, &task) != nil || task.DestinationID != "shows" {
		t.Fatalf("default path task: %d %+v %s", response.Code, task, response.Body)
	}
}

func TestDirectoryPathCreationPaginationAndValidation(t *testing.T) {
	b := newMockBackend()
	b.directoryPages[""] = map[string]xunlei.DirectoryPage{
		"":       {Directories: []xunlei.Directory{{ID: "other", Name: "其他", Writable: true}}, NextPageToken: "second"},
		"second": {Directories: []xunlei.Directory{{ID: "root", Name: "迅雷下载", Writable: true}}},
	}
	s, _ := testServer(t, b, Config{})
	response := request(s, "POST", "/v1/directories", `{"parent_path":"/迅雷下载","name":"新目录"}`, map[string]string{"Idempotency-Key": "path-directory"})
	op := responseOperation(t, response)
	var directory xunlei.Directory
	if response.Code != 201 || json.Unmarshal(op.Result, &directory) != nil || directory.DisplayPath != "/迅雷下载/新目录" {
		t.Fatalf("path directory: %d %+v %s", response.Code, directory, response.Body)
	}
	b.mu.Lock()
	lastParent := ""
	for _, call := range b.calls {
		if call.method == "create_directory" {
			lastParent = call.id
		}
	}
	b.mu.Unlock()
	if lastParent != "root" {
		t.Fatalf("friendly parent resolved to %q", lastParent)
	}

	for _, tc := range []struct {
		name, body string
	}{
		{"missing", `{"url":"https://example.org/file"}`},
		{"both", `{"url":"https://example.org/file","destination_id":"root","destination_path":"/迅雷下载"}`},
		{"relative", `{"url":"https://example.org/file","destination_path":"迅雷下载"}`},
		{"root", `{"url":"https://example.org/file","destination_path":"/"}`},
		{"dot", `{"url":"https://example.org/file","destination_path":"/迅雷下载/../其他"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := b.count("create")
			got := request(s, "POST", "/v1/tasks", tc.body, map[string]string{"Idempotency-Key": "invalid-" + tc.name})
			if got.Code != 400 || b.count("create") != before {
				t.Fatalf("invalid selector reached task creation: %d %s", got.Code, got.Body)
			}
		})
	}
	if got := request(s, "GET", "/v1/directories?parent_id=root&path=%2F%E8%BF%85%E9%9B%B7%E4%B8%8B%E8%BD%BD", "", nil); got.Code != 400 {
		t.Fatalf("two directory selectors accepted: %d %s", got.Code, got.Body)
	}

	b.directories["root"] = append(b.directories["root"], xunlei.Directory{ID: "movies-duplicate", Name: "电影", Writable: true})
	response = request(s, "POST", "/v1/tasks", `{"url":"https://example.org/file","destination_path":"/迅雷下载/电影"}`, map[string]string{"Idempotency-Key": "ambiguous"})
	if response.Code != 400 || responseOperation(t, response).Status != "failed" {
		t.Fatalf("ambiguous path accepted: %d %s", response.Code, response.Body)
	}
	response = request(s, "POST", "/v1/tasks", `{"url":"https://example.org/file","destination_path":"/迅雷下载/不存在"}`, map[string]string{"Idempotency-Key": "not-found"})
	if response.Code != 404 || responseOperation(t, response).Status != "failed" {
		t.Fatalf("missing path accepted: %d %s", response.Code, response.Body)
	}
}

func TestExactOriginAndPreflightPolicy(t *testing.T) {
	b := newMockBackend()
	s, _ := testServer(t, b, Config{AllowedOrigins: []string{"https://app.example.org"}})
	for _, origin := range []string{"null", "https://evil.example.org", "https://app.example.org.evil.test", "http://app.example.org"} {
		got := request(s, "GET", "/v1/device", "", map[string]string{"Origin": origin})
		if got.Code != 403 || got.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("origin %s got %d %v", origin, got.Code, got.Header())
		}
	}
	headers := map[string]string{
		"Origin": "https://app.example.org", "Authorization": "",
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization, Content-Type, Idempotency-Key",
	}
	got := request(s, "OPTIONS", "/v1/tasks", "", headers)
	if got.Code != 204 || got.Header().Get("Access-Control-Allow-Origin") != "https://app.example.org" {
		t.Fatalf("allowed preflight got %d %s", got.Code, got.Body)
	}
	headers["Access-Control-Request-Headers"] = "X-Unapproved-Header"
	if got := request(s, "OPTIONS", "/v1/tasks", "", headers); got.Code != 403 {
		t.Fatalf("unapproved preflight header got %d", got.Code)
	}
	headers["Access-Control-Request-Headers"] = "Authorization"
	headers["Access-Control-Request-Method"] = "PUT"
	if got := request(s, "OPTIONS", "/v1/tasks", "", headers); got.Code != 403 {
		t.Fatalf("unapproved preflight method got %d", got.Code)
	}
	if b.count("device") != 0 || b.count("create") != 0 {
		t.Fatal("origin refusal or preflight invoked the backend")
	}
	got = request(s, "GET", "/v1/device", "", map[string]string{"Origin": "https://app.example.org"})
	if got.Code != 200 || got.Header().Get("Access-Control-Allow-Origin") != "https://app.example.org" {
		t.Fatalf("allowed origin got %d %s", got.Code, got.Body)
	}
}

func TestJSONValidationAndBodyLimitBeforeBackend(t *testing.T) {
	b := newMockBackend()
	s, _ := testServer(t, b, Config{})
	for _, tc := range []struct{ name, body, contentType string }{
		{"unknown field", `{"url":"https://example.org/file","unexpected":true}`, "application/json"},
		{"two values", `{"url":"https://example.org/file"} {}`, "application/json"},
		{"malformed", `{"url":`, "application/json"},
		{"wrong type", `{"url":5}`, "application/json"},
		{"wrong media type", `{"url":"https://example.org/file"}`, "text/plain"},
		{"oversized", `{"url":"` + strings.Repeat("x", 1<<20) + `"}`, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := request(s, "POST", "/v1/resources/resolve", tc.body, map[string]string{"Content-Type": tc.contentType})
			if got.Code < 400 || got.Code >= 500 {
				t.Fatalf("invalid input got %d %s", got.Code, got.Body)
			}
		})
	}
	if b.count("resolve") != 0 {
		t.Fatal("invalid JSON reached backend")
	}
	got := request(s, "POST", "/v1/tasks", `{"url":"https://example.org/file","destination_id":"root","unknown":1}`, map[string]string{"Idempotency-Key": "key"})
	if got.Code != 400 || b.count("create") != 0 {
		t.Fatalf("unknown task fields were accepted: %d %s", got.Code, got.Body)
	}
}

func TestConcurrentIdempotencyAndRequestConflict(t *testing.T) {
	b := newMockBackend()
	s, _ := testServer(t, b, Config{})
	body := `{"url":"https://example.org/file","destination_id":"root"}`
	const callers = 24
	responses := make(chan *httptest.ResponseRecorder, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "shared"})
		}()
	}
	wg.Wait()
	close(responses)
	firstID := ""
	created := 0
	for response := range responses {
		if response.Code == 201 {
			created++
		} else if response.Code != 200 {
			t.Fatalf("concurrent response: %d %s", response.Code, response.Body)
		}
		op := responseOperation(t, response)
		if firstID == "" {
			firstID = op.ID
		}
		if op.ID != firstID {
			t.Fatal("same idempotency key returned different operations")
		}
	}
	if created != 1 || b.count("create") != 1 {
		t.Fatalf("created responses=%d, upstream creates=%d", created, b.count("create"))
	}
	got := request(s, "POST", "/v1/tasks", `{"url":"https://example.org/other","destination_id":"root"}`, map[string]string{"Idempotency-Key": "shared"})
	if got.Code != 409 || b.count("create") != 1 {
		t.Fatalf("different request with same key: %d %s", got.Code, got.Body)
	}
	got = request(s, "POST", "/v1/tasks", body, nil)
	if got.Code != 400 || b.count("create") != 1 {
		t.Fatalf("write without idempotency key: %d %s", got.Code, got.Body)
	}
	got = request(s, "POST", "/v1/tasks", `{"url":"https://example.org/file","destination_id":"root","file_indices":[]}`, map[string]string{"Idempotency-Key": "shared"})
	if (got.Code != 400 && got.Code != 409) || b.count("create") != 1 {
		t.Fatalf("empty selection was mistaken for the original all-files request: %d %s", got.Code, got.Body)
	}
}

func TestRestartedPlannedIntentIsUnknownAndNeverResubmitted(t *testing.T) {
	b := newMockBackend()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{APIKey: testAPIKey}, b, st)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"url":"https://example.org/file","destination_id":"root"}`
	response := request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "crash-key"})
	op := responseOperation(t, response)
	// Simulate the crash boundary after upstream acceptance, before its
	// result was persisted. The keyed durable intent remains planned.
	op, err = st.Find(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op.Status, op.TaskID, op.Result = "planned", "", nil
	if err := st.Update(op); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err = New(Config{APIKey: testAPIKey}, b, st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	response = request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "crash-key"})
	replayed := responseOperation(t, response)
	if response.Code != 200 || replayed.ID != op.ID || replayed.Status != "unknown_outcome" {
		t.Fatalf("restart replay: %d %+v", response.Code, replayed)
	}
	s.Reconcile(context.Background())
	if b.count("create") != 1 || b.count("list") != 0 || b.count("task") != 0 {
		t.Fatalf("restart inferred or repeated a write: creates=%d lists=%d task reads=%d", b.count("create"), b.count("list"), b.count("task"))
	}
}

func TestUnknownUpstreamOutcomeDoesNotRetry(t *testing.T) {
	b := newMockBackend()
	b.createError = &xunlei.Error{Kind: "unknown_outcome", Message: "Request sent; result unknown"}
	s, _ := testServer(t, b, Config{})
	body := `{"url":"https://example.org/file","destination_id":"root"}`
	first := request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "uncertain"})
	op := responseOperation(t, first)
	if op.Status != "unknown_outcome" || b.count("create") != 1 {
		t.Fatalf("uncertain write = %+v, calls=%d", op, b.count("create"))
	}
	replayed := request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "uncertain"})
	if replayed.Code != 200 || responseOperation(t, replayed).ID != op.ID {
		t.Fatalf("unknown outcome replay: %d %s", replayed.Code, replayed.Body)
	}
	s.Reconcile(context.Background())
	if b.count("create") != 1 {
		t.Fatal("uncertain write was repeated")
	}
}

func TestOperationDetailsNeverAppearInPublicResponse(t *testing.T) {
	b := newMockBackend()
	s, st := testServer(t, b, Config{})
	op, _, err := st.Begin("private", "hash", store.Operation{
		Kind: "retry", Status: "failed", Details: json.RawMessage(`{"private":"DO-NOT-EXPOSE-THIS-PARAMETER"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(s, "GET", "/v1/operations/"+op.ID, "", nil)
	if response.Code != 200 || bytes.Contains(response.Body.Bytes(), []byte("DO-NOT-EXPOSE")) {
		t.Fatalf("public operation leaked private details: %d %s", response.Code, response.Body)
	}
	responseOperation(t, response)
	stored, err := st.Find(op.ID)
	if err != nil || len(stored.Details) == 0 {
		t.Fatal("public response destructively removed stored recovery details")
	}
}

func TestUnknownTaskIDDoesNotControlOtherTask(t *testing.T) {
	b := newMockBackend()
	b.setTask(xunlei.Task{ID: "real-task", Status: "running"})
	s, _ := testServer(t, b, Config{})
	if response := request(s, "GET", "/v1/tasks/missing-task", "", nil); response.Code != 404 {
		t.Fatalf("missing task query: %d %s", response.Code, response.Body)
	}
	response := request(s, "POST", "/v1/tasks/missing-task/pause", "", map[string]string{"Idempotency-Key": "missing"})
	if response.Code < 400 || responseOperation(t, response).Status != "failed" || b.count("control") != 0 {
		t.Fatalf("missing task was controlled: %d %s", response.Code, response.Body)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, call := range b.calls {
		if call.method == "task" && call.id != "missing-task" {
			t.Fatalf("wrong task was queried: %+v", call)
		}
	}
}

func TestBackendTaskIDMismatchFailsClosed(t *testing.T) {
	for _, tc := range []struct{ method, path, status string }{
		{"GET", "/v1/tasks/requested", "running"},
		{"POST", "/v1/tasks/requested/pause", "running"},
		{"POST", "/v1/tasks/requested/resume", "paused"},
		{"DELETE", "/v1/tasks/requested", "running"},
		{"POST", "/v1/tasks/requested/retry", "error"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			b := newMockBackend()
			// Simulate an upstream/client bug returning another task for an
			// exact-ID query. No write may target that unrelated task.
			b.tasks["requested"] = xunlei.Task{ID: "unrelated", Status: tc.status, URL: "https://example.org/file", DestinationID: "root"}
			s, _ := testServer(t, b, Config{})
			response := request(s, tc.method, tc.path, "", map[string]string{"Idempotency-Key": "mismatch"})
			if response.Code < 400 || b.count("control") != 0 || b.count("delete") != 0 || b.count("recreate") != 0 {
				t.Fatalf("mismatched task was exposed or mutated: %d %s", response.Code, response.Body)
			}
		})
	}
	b := newMockBackend()
	b.tasks["requested"] = xunlei.Task{ID: "unrelated", Status: "paused"}
	s, st := testServer(t, b, Config{})
	op, _, err := st.Begin("pending", "hash", store.Operation{Kind: "pause", TaskID: "requested", Status: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	s.Reconcile(context.Background())
	observed, err := st.Find(op.ID)
	if err != nil || observed.Status == "confirmed" {
		t.Fatalf("another task's state confirmed the command: %+v %v", observed, err)
	}
}

func TestPauseResumeRequireObservedDesiredState(t *testing.T) {
	for _, tc := range []struct{ action, initial, desired string }{
		{"pause", "running", "paused"}, {"resume", "paused", "running"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			b := newMockBackend()
			b.setTask(xunlei.Task{ID: "task", Status: tc.initial})
			s, st := testServer(t, b, Config{})
			response := request(s, "POST", "/v1/tasks/task/"+tc.action, "", map[string]string{"Idempotency-Key": tc.action})
			op := responseOperation(t, response)
			if response.Code != 202 || op.Status != "accepted" || b.count("control") != 1 {
				t.Fatalf("initial control = %d %+v", response.Code, op)
			}
			s.Reconcile(context.Background())
			observed, err := st.Find(op.ID)
			if err != nil || observed.Status != "accepted" {
				t.Fatalf("unobserved control was confirmed: %+v %v", observed, err)
			}
			b.setTask(xunlei.Task{ID: "task", Status: tc.desired})
			s.Reconcile(context.Background())
			observed, err = st.Find(op.ID)
			if err != nil || observed.Status != "confirmed" || b.count("control") != 1 {
				t.Fatalf("desired state not confirmed: %+v %v", observed, err)
			}
		})
	}
}

func TestAlreadyDesiredStateNeedsNoControlWrite(t *testing.T) {
	for _, tc := range []struct{ action, status string }{{"pause", "paused"}, {"resume", "running"}, {"resume", "pending"}} {
		t.Run(tc.action+tc.status, func(t *testing.T) {
			b := newMockBackend()
			b.setTask(xunlei.Task{ID: "task", Status: tc.status})
			s, _ := testServer(t, b, Config{})
			response := request(s, "POST", "/v1/tasks/task/"+tc.action, "", map[string]string{"Idempotency-Key": "already"})
			if response.Code != 200 || responseOperation(t, response).Status != "confirmed" || b.count("control") != 0 {
				t.Fatalf("already satisfied control: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestTerminalAndUnknownTaskStatesRejectPauseAndResume(t *testing.T) {
	for _, status := range []string{"error", "complete", "unknown"} {
		for _, action := range []string{"pause", "resume"} {
			t.Run(status+action, func(t *testing.T) {
				b := newMockBackend()
				b.setTask(xunlei.Task{ID: "task", Status: status})
				s, _ := testServer(t, b, Config{})
				response := request(s, "POST", "/v1/tasks/task/"+action, "", map[string]string{"Idempotency-Key": "control"})
				if response.Code != 409 || responseOperation(t, response).Status != "failed" || b.count("control") != 0 {
					t.Fatalf("terminal or unknown state was controlled: %d %s", response.Code, response.Body)
				}
			})
		}
	}
}

func TestReconciliationTimeoutNeverRepeatsCommand(t *testing.T) {
	b := newMockBackend()
	b.setTask(xunlei.Task{ID: "task", Status: "running"})
	s, st := testServer(t, b, Config{OperationTimeout: time.Second})
	op, _, err := st.Begin("old-command", "hash", store.Operation{
		Kind: "pause", TaskID: "task", Status: "accepted", CreatedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Reconcile(context.Background())
	observed, err := st.Find(op.ID)
	if err != nil || observed.Status != "unknown_outcome" {
		t.Fatalf("unconfirmed timed-out operation: %+v %v", observed, err)
	}
	s.Reconcile(context.Background())
	if b.count("control") != 0 || b.count("task") != 1 {
		t.Fatalf("timed-out operation was repeated or polled forever: controls=%d reads=%d", b.count("control"), b.count("task"))
	}
}

func TestDeleteRecordAndDeleteFilesHaveDifferentConfirmation(t *testing.T) {
	for _, files := range []bool{false, true} {
		t.Run(fmt.Sprintf("files=%v", files), func(t *testing.T) {
			b := newMockBackend()
			b.setTask(xunlei.Task{ID: "task", Status: "complete"})
			s, st := testServer(t, b, Config{OperationTimeout: time.Hour})
			response := request(s, "DELETE", fmt.Sprintf("/v1/tasks/task?delete_files=%v", files), "", map[string]string{"Idempotency-Key": "delete"})
			op := responseOperation(t, response)
			if response.Code != 202 || op.Status != "accepted" {
				t.Fatalf("delete request: %d %+v", response.Code, op)
			}
			if files {
				b.removeTask("task")
				if b.count("control") != 1 || b.count("delete") != 0 {
					t.Fatal("delete files used the record-only operation")
				}
			} else if b.count("delete") != 1 || b.count("control") != 0 {
				t.Fatal("record-only deletion used a physical delete command")
			}
			s.Reconcile(context.Background())
			observed, err := st.Find(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if files {
				if observed.Status == "confirmed" || !bytes.Contains(observed.Result, []byte(`"files_deleted":null`)) {
					t.Fatalf("physical file deletion was claimed without evidence: %+v", observed)
				}
			} else if observed.Status != "confirmed" || !bytes.Contains(observed.Result, []byte(`"files_deleted":false`)) {
				t.Fatalf("record-only deletion not confirmed: %+v", observed)
			}
		})
	}
}

func TestRetryPreflightPreservesOldTaskAndReplacementUsesNewID(t *testing.T) {
	for _, tc := range []struct {
		name, status, destination, source string
		online                            bool
	}{
		{"running task", "running", "root", "magnet:?xt=urn:btih:test", true},
		{"missing source", "error", "root", "", true},
		{"missing destination", "error", "", "magnet:?xt=urn:btih:test", true},
		{"offline device", "error", "root", "magnet:?xt=urn:btih:test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newMockBackend()
			b.device.Online = tc.online
			b.setTask(xunlei.Task{ID: "old", Status: tc.status, URL: tc.source, DestinationID: tc.destination})
			s, _ := testServer(t, b, Config{})
			response := request(s, "POST", "/v1/tasks/old/retry", "", map[string]string{"Idempotency-Key": "retry"})
			if response.Code != 409 || responseOperation(t, response).Status != "failed" || b.count("delete") != 0 || b.count("recreate") != 0 {
				t.Fatalf("unsafe retry preflight: %d %s", response.Code, response.Body)
			}
		})
	}
	b := newMockBackend()
	b.setTask(xunlei.Task{ID: "old", Status: "error", URL: "magnet:?xt=urn:btih:test", DestinationID: "root"})
	s, st := testServer(t, b, Config{})
	response := request(s, "POST", "/v1/tasks/old/retry", "", map[string]string{"Idempotency-Key": "retry"})
	op := responseOperation(t, response)
	if response.Code != 201 || op.Status != "accepted" || op.PreviousTaskID != "old" || op.TaskID != "replacement-task" || b.count("delete") != 1 || b.count("recreate") != 1 {
		t.Fatalf("retry replacement = %d %+v", response.Code, op)
	}
	s.Reconcile(context.Background())
	op, err := st.Find(op.ID)
	if err != nil || op.Status != "confirmed" || op.TaskID != "replacement-task" {
		t.Fatalf("replacement not confirmed by new ID: %+v %v", op, err)
	}
	response = request(s, "POST", "/v1/tasks/old/retry", "", map[string]string{"Idempotency-Key": "retry"})
	if response.Code != 200 || b.count("recreate") != 1 || b.count("delete") != 1 {
		t.Fatalf("retry was repeated: %d %s", response.Code, response.Body)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prepared, deleted := false, false
	for _, call := range b.calls {
		switch call.method {
		case "prepare_retry":
			prepared = true
		case "delete":
			if !prepared {
				t.Fatal("retry deleted the old record before validating its replacement")
			}
			deleted = true
		case "recreate":
			if !prepared || !deleted {
				t.Fatal("retry replacement was sent before preparation and record deletion")
			}
		}
	}
}

func TestRetryPreparationFailurePreservesOldTask(t *testing.T) {
	b := newMockBackend()
	b.setTask(xunlei.Task{ID: "old", Status: "error", URL: "https://example.org/file", DestinationID: "removed-directory"})
	b.prepareError = &xunlei.Error{Kind: "invalid", Message: "Original destination is no longer writable"}
	s, _ := testServer(t, b, Config{})
	response := request(s, "POST", "/v1/tasks/old/retry", "", map[string]string{"Idempotency-Key": "prepare-fails"})
	if response.Code != 400 || responseOperation(t, response).Status != "failed" || b.count("prepare_retry") != 1 || b.count("delete") != 0 || b.count("recreate") != 0 {
		t.Fatalf("preparation failure still removed the old task: %d %s", response.Code, response.Body)
	}
	if _, err := b.Task(context.Background(), "old"); err != nil {
		t.Fatalf("old failed task was not preserved: %v", err)
	}
}

func TestRetryDoesNotRecreateAfterUnknownDeleteOutcome(t *testing.T) {
	b := newMockBackend()
	b.setTask(xunlei.Task{ID: "old", Status: "error", URL: "magnet:?xt=urn:btih:test", DestinationID: "root"})
	b.deleteError = &xunlei.Error{Kind: "unknown_outcome", Message: "Delete response was lost"}
	s, st := testServer(t, b, Config{})
	response := request(s, "POST", "/v1/tasks/old/retry", "", map[string]string{"Idempotency-Key": "retry"})
	op := responseOperation(t, response)
	if op.Status != "unknown_outcome" || b.count("delete") != 1 || b.count("recreate") != 0 {
		t.Fatalf("ambiguous deletion allowed recreation: %+v", op)
	}
	stored, err := st.Find(op.ID)
	if err != nil || !bytes.Contains(stored.Details, []byte("magnet:")) {
		t.Fatalf("original source was not retained for recovery: %+v %v", stored, err)
	}
	s.Reconcile(context.Background())
	if b.count("recreate") != 0 || b.count("delete") != 1 {
		t.Fatal("reconciliation repeated an ambiguous retry")
	}
	observed, err := st.Find(op.ID)
	if err != nil || observed.Status != "unknown_outcome" {
		t.Fatalf("existing old failed task was mistaken for a confirmed replacement: %+v %v", observed, err)
	}
}

func TestStorageFailurePreventsUpstreamWrite(t *testing.T) {
	b := newMockBackend()
	s, st := testServer(t, b, Config{})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	response := request(s, "POST", "/v1/tasks", `{"url":"https://example.org/file","destination_id":"root"}`, map[string]string{"Idempotency-Key": "write"})
	if response.Code != 503 || b.count("create") != 0 {
		t.Fatalf("failed intent persistence still caused a write: %d %s", response.Code, response.Body)
	}
}

func TestResultPersistenceFailureReturnsRecoverableOperationID(t *testing.T) {
	b := newMockBackend()
	s, st := testServer(t, b, Config{})
	// The intent is safely stored, then the store becomes unavailable after
	// the upstream accepted the request but before its result can be saved.
	b.afterCreate = func() { _ = st.Close() }
	body := `{"url":"https://example.org/file","destination_id":"root"}`
	response := request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "recover"})
	var envelope struct {
		Error struct {
			Code        string `json:"code"`
			OperationID string `json:"operation_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != 503 || envelope.Error.Code != "storage_error" || envelope.Error.OperationID == "" || b.count("create") != 1 {
		t.Fatalf("ambiguous persisted result has no recovery handle: %d %s, err=%v", response.Code, response.Body, err)
	}
	response = request(s, "POST", "/v1/tasks", body, map[string]string{"Idempotency-Key": "recover"})
	if response.Code != 503 || b.count("create") != 1 {
		t.Fatalf("persistence failure allowed resubmission: %d %s", response.Code, response.Body)
	}
}

func TestInvalidLimitsAndDeleteFlags(t *testing.T) {
	b := newMockBackend()
	s, _ := testServer(t, b, Config{})
	for _, value := range []string{"0", "201", "-1", "abc"} {
		if response := request(s, "GET", "/v1/tasks?limit="+value, "", nil); response.Code != 400 {
			t.Fatalf("invalid limit %q accepted: %d", value, response.Code)
		}
	}
	for _, query := range []string{"delete_files=1", "delete_files=TRUE", "delete_files=true&delete_files=false"} {
		if response := request(s, "DELETE", "/v1/tasks/task?"+query, "", map[string]string{"Idempotency-Key": "delete"}); response.Code != 400 {
			t.Fatalf("ambiguous delete flag accepted: %d %s", response.Code, response.Body)
		}
	}
	if b.count("list") != 0 || b.count("delete") != 0 || b.count("control") != 0 {
		t.Fatal("invalid query parameters reached backend")
	}
}

var _ Backend = (*mockBackend)(nil)
