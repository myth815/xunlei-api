// Package live_test exercises a running API and a real, logged-in Xunlei device.
// Every write is opt-in. Ordinary go test runs never contact a live service.
package live_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const maxFixtureBytes = 64 << 20

type operation struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	TaskID string          `json:"task_id"`
	Result json.RawMessage `json:"result"`
}
type operationEnvelope struct {
	Operation operation `json:"operation"`
}
type directory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type task struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	DestinationID string            `json:"destination_id"`
	SizeBytes     *int64            `json:"size_bytes"`
	Params        map[string]string `json:"params"`
}
type ownedTask struct {
	id, name, destination string
	cleaned               bool
}
type harness struct {
	base, key, parent, source, runID, mount, expectedSHA string
	timeout                                              time.Duration
	client                                               *http.Client
	tasks                                                []*ownedTask
}

func load(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("XUNLEI_API_LIVE_WRITE") != "1" {
		t.Skip("live writes disabled; set XUNLEI_API_LIVE_WRITE=1 only for a dedicated fixture")
	}
	config := map[string]string{}
	for _, name := range []string{"XUNLEI_API_TEST_URL", "XUNLEI_API_TEST_KEY", "XUNLEI_API_TEST_PARENT_ID", "XUNLEI_API_TEST_SOURCE_URL"} {
		config[name] = os.Getenv(name)
		if config[name] == "" {
			t.Fatalf("%s is required when live writes are enabled", name)
		}
	}
	base, err := url.Parse(config["XUNLEI_API_TEST_URL"])
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "http" && base.Scheme != "https") {
		t.Fatal("XUNLEI_API_TEST_URL must be an HTTP(S) API base without credentials/query/fragment")
	}
	source, err := url.Parse(config["XUNLEI_API_TEST_SOURCE_URL"])
	if err != nil || source.Host == "" || (source.Scheme != "http" && source.Scheme != "https") {
		t.Fatal("the live fixture source must be a small controlled HTTP(S) file")
	}
	timeout := 5 * time.Minute
	if value := os.Getenv("XUNLEI_API_TEST_TIMEOUT"); value != "" {
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout < 30*time.Second || timeout > 30*time.Minute {
			t.Fatal("XUNLEI_API_TEST_TIMEOUT must be between 30s and 30m")
		}
	}
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		t.Fatal("cannot generate a unique fixture name")
	}
	h := &harness{base: strings.TrimRight(base.String(), "/"), key: config["XUNLEI_API_TEST_KEY"], parent: config["XUNLEI_API_TEST_PARENT_ID"], source: config["XUNLEI_API_TEST_SOURCE_URL"], runID: "xunlei-api-live-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random), timeout: timeout, mount: os.Getenv("XUNLEI_API_TEST_MOUNT_PARENT"), expectedSHA: strings.ToLower(os.Getenv("XUNLEI_API_TEST_SHA256")), client: &http.Client{Timeout: 70 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if h.mount != "" && !filepath.IsAbs(h.mount) {
		t.Fatal("XUNLEI_API_TEST_MOUNT_PARENT must be an absolute path")
	}
	if h.expectedSHA != "" {
		decoded, err := hex.DecodeString(h.expectedSHA)
		if err != nil || len(decoded) != sha256.Size {
			t.Fatal("XUNLEI_API_TEST_SHA256 must be a SHA-256 hex digest")
		}
		if h.mount == "" {
			t.Fatal("SHA-256 verification also requires XUNLEI_API_TEST_MOUNT_PARENT")
		}
	}
	t.Cleanup(func() { h.cleanup(t) })
	return h
}

// Never include response bodies, URL origins, source URLs, or keys in errors.
// A failing upstream can reflect credentials or user task metadata in its body.
func (h *harness) request(ctx context.Context, method, path, key string, body any, out any) (int, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return 0, errors.New("cannot encode fixture request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("cannot construct fixture request")
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	if key != "" {
		req.Header.Set("Idempotency-Key", h.runID+":"+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, errors.New("API connection failed or timed out; inspect the original operation before retrying")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, errors.New("cannot read API response")
	}
	if out != nil && len(payload) > 0 {
		if json.Unmarshal(payload, out) != nil {
			return resp.StatusCode, errors.New("API returned an unexpected response")
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
func (h *harness) mutate(t *testing.T, method, path, key string, body any) operation {
	t.Helper()
	call, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var envelope operationEnvelope
	_, err := h.request(call, method, path, key, body, &envelope)
	if err != nil {
		t.Fatalf("fixture mutation %s failed: %v (operation ID: %s)", key, err, envelope.Operation.ID)
	}
	op := envelope.Operation
	if op.ID == "" || (op.Status != "accepted" && op.Status != "confirmed") {
		t.Fatalf("fixture mutation %s was not accepted (operation ID: %s, state: %s)", key, op.ID, op.Status)
	}
	return op
}
func (h *harness) getTask(ctx context.Context, id string) (task, int, error) {
	var result task
	status, err := h.request(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(id), "", nil, &result)
	return result, status, err
}
func (h *harness) waitTask(t *testing.T, owned *ownedTask, statuses ...string) task {
	t.Helper()
	call, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	for {
		value, status, err := h.getTask(call, owned.id)
		if err == nil {
			h.assertOwned(t, owned, value)
			if value.SizeBytes != nil && *value.SizeBytes > maxFixtureBytes {
				t.Fatal("fixture exceeds the 64 MiB live-test safety limit")
			}
			for _, wanted := range statuses {
				if value.Status == wanted {
					return value
				}
			}
			if value.Status == "error" {
				t.Fatal("dedicated fixture entered the error state")
			}
			if value.Status == "complete" {
				t.Fatal("fixture completed before the requested control state; supply a slower fixture")
			}
		} else if status != 404 {
			t.Fatalf("reading the dedicated fixture failed: %v", err)
		}
		if err := wait(call); err != nil {
			t.Fatal("timed out waiting for the dedicated fixture task state")
		}
	}
}
func wait(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (h *harness) assertOwned(t *testing.T, owned *ownedTask, value task) {
	t.Helper()
	if value.ID != owned.id || value.Name != owned.name || value.DestinationID != owned.destination {
		t.Fatal("returned task does not match this test's unique name and directory; it will not be controlled or deleted")
	}
}

func (h *harness) create(t *testing.T, suffix string) (*ownedTask, string) {
	t.Helper()
	var device struct {
		Online   bool `json:"online"`
		LoggedIn bool `json:"logged_in"`
	}
	if _, err := h.request(context.Background(), http.MethodGet, "/v1/device", "", nil, &device); err != nil || !device.Online || !device.LoggedIn {
		t.Fatal("live device must be online and logged in")
	}
	name := h.runID + "-" + suffix
	dirOp := h.mutate(t, http.MethodPost, "/v1/directories", suffix+":directory", map[string]string{"parent_id": h.parent, "name": name})
	var dir directory
	if json.Unmarshal(dirOp.Result, &dir) != nil || dir.ID == "" {
		t.Fatal("directory creation returned no directory ID")
	}
	if dir.Name != name {
		t.Fatal("directory creation did not return the unique requested name")
	}
	fixtureName := "fixture-" + suffix + ".bin"
	body := map[string]string{"url": h.source, "destination_id": dir.ID, "name": fixtureName}
	created := h.mutate(t, http.MethodPost, "/v1/tasks", suffix+":create", body)
	var value task
	if json.Unmarshal(created.Result, &value) != nil || value.ID == "" || created.TaskID != value.ID {
		t.Fatal("task creation returned no consistent task ID")
	}
	owned := &ownedTask{id: value.ID, name: fixtureName, destination: dir.ID}
	h.tasks = append(h.tasks, owned)
	h.assertOwned(t, owned, value)
	// Replaying exactly the same key and request must return the original
	// operation/task. No new key is generated after an uncertain request.
	repeated := h.mutate(t, http.MethodPost, "/v1/tasks", suffix+":create", body)
	if repeated.ID != created.ID || repeated.TaskID != created.TaskID {
		t.Fatal("same-key request created a second operation or task")
	}
	t.Logf("dedicated fixture directory: %s; task: %s; create operation: %s", name, owned.id, created.ID)
	return owned, name
}
func (h *harness) waitAbsent(t *testing.T, id string) {
	t.Helper()
	call, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	for {
		_, status, err := h.getTask(call, id)
		if status == 404 {
			return
		}
		if err != nil {
			t.Fatalf("checking task-record deletion failed: %v", err)
		}
		if wait(call) != nil {
			t.Fatal("task record did not disappear before the timeout")
		}
	}
}

// The optional mount is used only for read-only checks under the fresh random
// directory. Never trust params.real_path and never remove files via the mount.
func (h *harness) fileDigest(dirName, fileName string) (string, int64, error) {
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		return "", 0, errors.New("cannot open configured fixture mount parent")
	}
	defer root.Close()
	info, err := root.Lstat(dirName)
	if err != nil {
		return "", 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", 0, errors.New("fixture directory is not a regular directory")
	}
	relative := filepath.Join(dirName, fileName)
	info, err = root.Lstat(relative)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFixtureBytes {
		return "", 0, errors.New("fixture file must be regular and at most 64 MiB")
	}
	file, err := root.Open(relative)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxFixtureBytes+1))
	if err != nil || size > maxFixtureBytes {
		return "", 0, errors.New("cannot hash the fixture within the size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func TestLiveTaskLifecycle(t *testing.T) {
	h := load(t)
	owned, dirName := h.create(t, "keep-files")
	h.waitTask(t, owned, "pending", "running")
	h.mutate(t, http.MethodPost, "/v1/tasks/"+url.PathEscape(owned.id)+"/pause", "pause", nil)
	h.waitTask(t, owned, "paused")
	h.mutate(t, http.MethodPost, "/v1/tasks/"+url.PathEscape(owned.id)+"/resume", "resume", nil)
	h.waitTask(t, owned, "pending", "running")
	h.waitTask(t, owned, "complete")
	before := ""
	if h.mount != "" {
		digest, size, err := h.fileDigest(dirName, owned.name)
		if err != nil {
			t.Fatal("cannot verify the completed fixture in the configured read-only mount")
		}
		if h.expectedSHA != "" && digest != h.expectedSHA {
			t.Fatal("fixture SHA-256 differs from the expected digest")
		}
		before = digest
		t.Logf("completed fixture verified: %d bytes, SHA-256 %s", size, digest)
	}
	h.mutate(t, http.MethodDelete, "/v1/tasks/"+url.PathEscape(owned.id)+"?delete_files=false", "keep-files:delete-record", nil)
	h.waitAbsent(t, owned.id)
	owned.cleaned = true
	if h.mount != "" {
		after, _, err := h.fileDigest(dirName, owned.name)
		if err != nil || before != after {
			t.Fatal("record-only deletion did not preserve the fixture file and its digest")
		}
		t.Log("record-only deletion preserved the downloaded file and SHA-256")
	} else {
		t.Log("task record disappeared; physical file preservation was not independently checked (no mount configured)")
	}
}

func TestLiveDeleteFiles(t *testing.T) {
	if os.Getenv("XUNLEI_API_LIVE_DELETE_FILES") != "1" {
		t.Skip("file deletion disabled; set XUNLEI_API_LIVE_DELETE_FILES=1 for a separate disposable fixture")
	}
	h := load(t)
	owned, dirName := h.create(t, "delete-files")
	h.waitTask(t, owned, "complete")
	if h.mount != "" {
		if _, _, err := h.fileDigest(dirName, owned.name); err != nil {
			t.Fatal("fixture must exist in the mount before requesting file deletion")
		}
	}
	h.mutate(t, http.MethodDelete, "/v1/tasks/"+url.PathEscape(owned.id)+"?delete_files=true", "delete-files:delete", nil)
	call, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	reported := false
	for {
		value, status, err := h.getTask(call, owned.id)
		if status == 404 {
			reported = true
			owned.cleaned = true
		} else if err != nil {
			t.Fatalf("checking file-delete task failed: %v", err)
		} else {
			h.assertOwned(t, owned, value)
			reported = value.Params["is_deleted"] == "true"
		}
		physicallyAbsent := false
		if h.mount != "" {
			_, _, err := h.fileDigest(dirName, owned.name)
			physicallyAbsent = errors.Is(err, os.ErrNotExist)
			if err != nil && !physicallyAbsent {
				t.Fatal("filesystem verification failed without proving fixture deletion")
			}
		}
		if h.mount != "" && physicallyAbsent {
			t.Log("dedicated fixture file deletion verified through the read-only mount")
			return
		}
		if h.mount == "" && reported {
			t.Log("upstream reported task/file deletion; physical filesystem deletion remains unverified")
			return
		}
		if wait(call) != nil {
			t.Fatal("file-delete request was accepted, but the dedicated fixture deletion could not be verified before the timeout")
		}
	}
}

func (h *harness) cleanup(t *testing.T) {
	t.Helper()
	for _, owned := range h.tasks {
		if owned.cleaned {
			continue
		}
		call, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		func() {
			defer cancel()
			value, status, err := h.getTask(call, owned.id)
			if status == 404 {
				return
			}
			if err != nil {
				t.Errorf("cleanup could not query dedicated task %s; inspect it manually", owned.id)
				return
			}
			if value.ID != owned.id || value.Name != owned.name || value.DestinationID != owned.destination {
				t.Errorf("cleanup refused mismatched task %s; no command sent", owned.id)
				return
			}
			if value.Status == "running" || value.Status == "pending" {
				var response operationEnvelope
				_, err = h.request(call, http.MethodPost, "/v1/tasks/"+url.PathEscape(owned.id)+"/pause", "cleanup-pause:"+owned.id, nil, &response)
				if err != nil {
					t.Errorf("cleanup could not pause dedicated task %s; no record deletion attempted", owned.id)
					return
				}
				for {
					value, status, err = h.getTask(call, owned.id)
					if status == 404 {
						return
					}
					if err != nil {
						t.Errorf("cleanup could not verify pause for dedicated task %s", owned.id)
						return
					}
					if value.ID != owned.id || value.Name != owned.name || value.DestinationID != owned.destination {
						t.Errorf("cleanup refused changed task metadata for %s", owned.id)
						return
					}
					if value.Status == "paused" || value.Status == "complete" || value.Status == "error" {
						break
					}
					if wait(call) != nil {
						t.Errorf("cleanup pause timed out for dedicated task %s", owned.id)
						return
					}
				}
			}
			var response operationEnvelope
			_, err = h.request(call, http.MethodDelete, "/v1/tasks/"+url.PathEscape(owned.id)+"?delete_files=false", "cleanup-record:"+owned.id, nil, &response)
			if err != nil || response.Operation.Status == "failed" || response.Operation.Status == "unknown_outcome" {
				t.Errorf("cleanup could not remove dedicated task record %s; files were left untouched", owned.id)
				return
			}
			t.Logf("cleanup requested removal of dedicated task record %s; downloaded files and test directory were retained", owned.id)
		}()
	}
}
