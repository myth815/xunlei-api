//go:build linux || darwin

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDurableBeginUpdateAndReplayAfterReopen(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	op, replay, err := s.Begin("caller-key", "request-hash", Operation{
		Kind: "create", Details: json.RawMessage(`{"url":"https://example.org/file"}`),
	})
	if err != nil || replay || op.ID == "" || op.Status != "planned" || op.CreatedAt.IsZero() {
		t.Fatalf("Begin = %+v, %v, %v", op, replay, err)
	}
	op.Status = "accepted"
	op.TaskID = "task-123"
	op.Result = json.RawMessage(`{"id":"task-123"}`)
	if err := s.Update(op); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, dir)
	again, replay, err := s.Begin("caller-key", "request-hash", Operation{Kind: "create"})
	if err != nil || !replay || again.ID != op.ID || again.Status != "accepted" || again.TaskID != "task-123" {
		t.Fatalf("replayed operation = %+v, %v, %v", again, replay, err)
	}
	if !bytes.Equal(again.Result, op.Result) || !bytes.Equal(again.Details, op.Details) {
		t.Fatal("operation details or result lost after reopening")
	}
	if !again.CreatedAt.Equal(op.CreatedAt) || again.UpdatedAt.Before(again.CreatedAt) {
		t.Fatal("invalid operation timestamps after reopening")
	}
	if _, _, err := s.Begin("caller-key", "changed-hash", Operation{Kind: "create"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay = %v, want ErrConflict", err)
	}
	found, err := s.Find(op.ID)
	if err != nil || found.TaskID != "task-123" {
		t.Fatalf("Find = %+v, %v", found, err)
	}
}

func TestConcurrentSameKeyHasOneDurableIntent(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	const callers = 48
	var created atomic.Int32
	var wg sync.WaitGroup
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, replay, err := s.Begin("shared-key", "same-hash", Operation{Kind: "create"})
			if err != nil {
				errs <- err
				return
			}
			if !replay {
				created.Add(1)
			}
			ids <- op.ID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	if created.Load() != 1 {
		t.Fatalf("created %d intents, want exactly one", created.Load())
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("different operation ids: %s and %s", first, id)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, dir)
	if pending := s.Pending(); len(pending) != 1 || pending[0].ID != first {
		t.Fatalf("persisted intents = %+v", pending)
	}
}

func TestEmptyKeysAreIndependentAndUnknownIDsFail(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	one, replay, err := s.Begin("", "same", Operation{Kind: "pause"})
	if err != nil || replay {
		t.Fatalf("first Begin = %v, %v", replay, err)
	}
	two, replay, err := s.Begin("", "same", Operation{Kind: "pause"})
	if err != nil || replay || one.ID == two.ID {
		t.Fatalf("second Begin = %+v, %v, %v", two, replay, err)
	}
	if _, err := s.Find("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Find missing = %v", err)
	}
	if err := s.Update(Operation{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing = %v", err)
	}
}

func TestPendingIncludesOnlyUnresolvedOperations(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	for i, status := range []string{"planned", "accepted", "unknown_outcome", "confirmed", "failed"} {
		_, _, err := s.Begin("", "", Operation{
			ID: fmt.Sprintf("id-%d", i), Kind: "pause", Status: status,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	pending := s.Pending()
	if len(pending) != 3 {
		t.Fatalf("pending = %+v", pending)
	}
	for i, op := range pending {
		if op.ID != fmt.Sprintf("id-%d", i) {
			t.Fatalf("unexpected pending order: %+v", pending)
		}
	}
}

func TestRawMessagesCannotMutateStoredData(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	details := json.RawMessage(`{"keep":true}`)
	op, _, err := s.Begin("key", "hash", Operation{Kind: "create", Details: details})
	if err != nil {
		t.Fatal(err)
	}
	details[0] = 'x'
	op.Details[0] = 'x'
	found, err := s.Find(op.ID)
	if err != nil || !json.Valid(found.Details) {
		t.Fatalf("input or return value changed the store: %+v %v", found, err)
	}
	found.Details[0] = 'x'
	pending := s.Pending()
	pending[0].Details[0] = 'x'
	replay, _, err := s.Begin("key", "hash", Operation{})
	if err != nil || !json.Valid(replay.Details) {
		t.Fatalf("read result changed the store: %+v %v", replay, err)
	}
}

func TestFileAndDirectoryPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, dir)
	if _, _, err := s.Begin("key", "hash", Operation{Kind: "delete"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		dir: 0o700, filepath.Join(dir, stateFileName): 0o600, filepath.Join(dir, ".lock"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions for %s: %v, err=%v, want=%#o", path, info, err, want)
		}
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".operations-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("leftover atomic write files: %v, %v", temps, err)
	}
}

func TestCorruptDataRefusesToOpenWithoutOverwriting(t *testing.T) {
	for _, invalid := range []string{
		`{`,
		`{}`,
		`null`,
		`{"version":1,"operations":{},"idempotency":{}} trailing`,
		`{"version":2,"operations":{},"idempotency":{}}`,
		`{"version":1,"operations":{},"idempotency":{"key":{"request_hash":"h","operation_id":"missing"}}}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, stateFileName)
			if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(dir); err == nil {
				_ = s.Close()
				t.Fatal("corrupt data was accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != invalid {
				t.Fatalf("corrupt file was changed: %q, %v", got, err)
			}
			// A failed open must release its process lock.
			if err := os.WriteFile(path, []byte(`{"version":1,"operations":{},"idempotency":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			openTestStore(t, dir)
		})
	}
}

func TestPersistenceFailureBlocksFurtherWrites(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	op, _, err := s.Begin("key", "hash", Operation{Kind: "create"})
	if err != nil {
		t.Fatal(err)
	}
	// Force rename to fail, using real filesystem behavior even when tests
	// run as root and permission-based failures would not be reliable.
	path := filepath.Join(dir, stateFileName)
	backup := filepath.Join(dir, "original.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	op.Status = "accepted"
	if err := s.Update(op); err == nil {
		t.Fatal("failed persistence did not return an error")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Begin("another-key", "hash", Operation{Kind: "create"}); err == nil {
		t.Fatal("store allowed a new intent after persistence failure")
	}
	if err := s.Update(op); err == nil {
		t.Fatal("store allowed an update after persistence failure")
	}
	if _, err := s.Find(op.ID); err == nil {
		t.Fatal("store returned possibly stale cached data after persistence failure")
	}
	if pending := s.Pending(); len(pending) != 0 {
		t.Fatalf("store exposed stale operations for reconciliation: %+v", pending)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, dir)
	op, replay, err := s.Begin("key", "hash", Operation{Kind: "create"})
	if err != nil || !replay || op.Status != "planned" {
		t.Fatalf("original durable intent changed: %+v, %v, %v", op, replay, err)
	}
}

func TestSingleInstanceLockAndClose(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	if other, err := Open(dir); !errors.Is(err, ErrLocked) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}
	runHelper(t, dir, "expect-lock")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Begin("key", "hash", Operation{Kind: "create"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Begin after Close = %v", err)
	}
	if _, err := s.Find("missing"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Find after Close = %v", err)
	}
	runHelper(t, dir, "expect-open")
}

func TestAbruptExitPreservesIntentWithoutResubmission(t *testing.T) {
	dir := t.TempDir()
	runHelper(t, dir, "persist-and-exit")
	s := openTestStore(t, dir)
	op, replay, err := s.Begin("crash-key", "request-hash", Operation{Kind: "create"})
	if err != nil || !replay || op.ID != "crash-operation" || op.Status != "planned" {
		t.Fatalf("crashed intent was not recovered: %+v, %v, %v", op, replay, err)
	}
}

func runHelper(t *testing.T, dir, action string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreSubprocessHelper$")
	cmd.Env = append(os.Environ(), "XUNLEI_STORE_TEST_DIR="+dir, "XUNLEI_STORE_TEST_ACTION="+action)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("store subprocess %s: %v\n%s", action, err, output)
	}
}

func TestStoreSubprocessHelper(t *testing.T) {
	dir, action := os.Getenv("XUNLEI_STORE_TEST_DIR"), os.Getenv("XUNLEI_STORE_TEST_ACTION")
	if action == "" {
		return
	}
	s, err := Open(dir)
	if action == "expect-lock" {
		if !errors.Is(err, ErrLocked) {
			fmt.Fprintln(os.Stderr, "expected process lock refusal:", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if action == "persist-and-exit" {
		_, _, err = s.Begin("crash-key", "request-hash", Operation{ID: "crash-operation", Kind: "create"})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// Deliberately bypass deferred cleanup and Store.Close.
		os.Exit(0)
	}
	_ = s.Close()
	os.Exit(0)
}
