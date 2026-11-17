package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestGrantKeyBatchRotationPreservesSealedCredentials(t *testing.T) {
	oldKey, newKey := testGrantKey(t), testGrantKey(t)
	testGrantEnv(t, "v1", map[string]string{"v1": oldKey, "v2": newKey})
	dir := t.TempDir()
	store, err := New(dir, initSource())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := testGrantTask(t, store)
	second := testGrantTask(t, store)
	for _, task := range []TaskRecord{first, second} {
		if _, err := store.CreateTaskGrant(ctx, task.TaskID,
			task.OwnerDigest, "private-grant-secret", expiresGrant()); err != nil {
			t.Fatal(err)
		}
	}
	store.grantKeys.active = "v2"
	before, err := store.GrantKeyUsage(ctx)
	if err != nil || before.ActiveKey != "v2" ||
		before.Total != 2 || before.ByKey["v1"] != 2 || before.Blocked != 0 {
		t.Fatalf("incorrect initial grant key inventory: %#v %v", before, err)
	}
	firstPass, err := store.RewrapGrantsBatch(ctx, 1)
	if err != nil || firstPass.Rewrapped != 1 ||
		firstPass.RemainingOldKey != 1 || firstPass.ByKey["v2"] != 1 {
		t.Fatalf("first bounded pass incorrect: %#v %v", firstPass, err)
	}
	finalPass, err := store.RewrapGrantsBatch(ctx, 1)
	if err != nil || finalPass.Rewrapped != 1 ||
		finalPass.RemainingOldKey != 0 || finalPass.ByKey["v2"] != 2 {
		t.Fatalf("second bounded pass incorrect: %#v %v", finalPass, err)
	}
	again, err := store.RewrapGrantsBatch(ctx, 1)
	if err != nil || again.Rewrapped != 0 {
		t.Fatalf("rotation was not idempotent on active key: %#v %v", again, err)
	}
	public, err := json.Marshal([]any{before, firstPass, finalPass, again})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte(oldKey)) ||
		bytes.Contains(public, []byte(newKey)) ||
		bytes.Contains(public, []byte("private-grant-secret")) ||
		bytes.Contains(public, []byte(first.TaskID)) {
		t.Fatal("key management returned sensitive credential or identity")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Original key is retired only from the *live* process keyring. The
	// rewrapped grant must remain decryptable by the newly active key.
	testGrantEnv(t, "v2", map[string]string{"v2": newKey})
	restored, err := New(dir, initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	for _, task := range []TaskRecord{first, second} {
		meta, token, err := restored.BeginTaskGrantRefresh(ctx, task.TaskID, task.OwnerDigest)
		if err != nil || token != "private-grant-secret" || meta.Generation != 2 {
			t.Fatalf("rewrapped grant not recoverable: %#v %v", meta, err)
		}
	}
}

func TestGrantKeyBatchRotationIsAtomicOnCiphertextCorruption(t *testing.T) {
	key1, key2 := testGrantKey(t), testGrantKey(t)
	testGrantEnv(t, "old", map[string]string{"old": key1, "next": key2})
	store := newGrantStore(t)
	ctx := context.Background()
	first := testGrantTask(t, store)
	second := testGrantTask(t, store)
	for _, task := range []TaskRecord{first, second} {
		if _, err := store.CreateTaskGrant(ctx, task.TaskID,
			task.OwnerDigest, "private-grant-secret", expiresGrant()); err != nil {
			t.Fatal(err)
		}
	}
	store.grantKeys.active = "next"
	if _, err := store.db.Exec(`UPDATE task_grants SET ciphertext=X'010203' WHERE task_id=?`,
		second.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RewrapGrantsBatch(ctx, 2);
		!errors.Is(err, ErrGrantKeyUnavailable) {
		t.Fatalf("corrupt grant was accepted: %v", err)
	}
	usage, err := store.GrantKeyUsage(ctx)
	if err != nil || usage.ByKey["old"] != 2 || usage.ByKey["next"] != 0 {
		t.Fatalf("partial grant rotation committed: %#v %v", usage, err)
	}
	if _, err := store.RewrapGrantsBatch(ctx, 0); err == nil {
		t.Fatal("zero-sized grant batch accepted")
	}
	if _, err := store.RewrapGrantsBatch(ctx, 65); err == nil {
		t.Fatal("unbounded grant batch accepted")
	}
}

func TestGrantKeyRotationBlocksWorkingOrIndeterminateGrants(t *testing.T) {
	key1, key2 := testGrantKey(t), testGrantKey(t)
	testGrantEnv(t, "old", map[string]string{"old": key1, "next": key2})
	store := newGrantStore(t)
	ctx := context.Background()
	working := testGrantTask(t, store)
	if _, err := store.CreateTaskGrant(ctx, working.TaskID,
		working.OwnerDigest, "working-token", expiresGrant()); err != nil {
		t.Fatal(err)
	}
	working.Status = "working"
	if err := store.TaskPut(working); err != nil {
		t.Fatal(err)
	}
	indeterminate := testGrantTask(t, store)
	if _, err := store.CreateTaskGrant(ctx, indeterminate.TaskID,
		indeterminate.OwnerDigest, "refresh-token", expiresGrant()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginTaskGrantRefresh(ctx,
		indeterminate.TaskID, indeterminate.OwnerDigest); err != nil {
		t.Fatal(err)
	}
	store.grantKeys.active = "next"
	usage, err := store.GrantKeyUsage(ctx)
	if err != nil || usage.Total != 2 || usage.Blocked != 2 {
		t.Fatalf("blocked credentials not inventoried: %#v %v", usage, err)
	}
	rotation, err := store.RewrapGrantsBatch(ctx, 32)
	if err != nil || rotation.Rewrapped != 0 ||
		rotation.RemainingOldKey != 2 || rotation.Blocked != 2 {
		t.Fatalf("working/refreshing grants were rotated: %#v %v", rotation, err)
	}
	if _, err := store.RewrapTaskGrant(ctx, working.TaskID, working.OwnerDigest);
		!errors.Is(err, ErrGrantUnavailable) {
		t.Fatalf("individual rewrap bypassed working-task fence: %v", err)
	}
	// Non-executable erased records no longer require an old live key.
	if err := store.RevokeTaskGrant(ctx, indeterminate.TaskID,
		indeterminate.OwnerDigest); err != nil {
		t.Fatal(err)
	}
	usage, err = store.GrantKeyUsage(ctx)
	if err != nil || usage.Total != 1 || usage.Blocked != 1 {
		t.Fatalf("revocation did not erase tracked ciphertext: %#v %v", usage, err)
	}
}

func TestGrantKeyBatchRotationRefusesObservers(t *testing.T) {
	key1, key2 := testGrantKey(t), testGrantKey(t)
	testGrantEnv(t, "old", map[string]string{"old": key1, "next": key2})
	dir := t.TempDir()
	leader, err := New(dir, initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	if err := leader.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	task := testGrantTask(t, leader)
	if _, err := leader.CreateTaskGrant(context.Background(),
		task.TaskID, task.OwnerDigest, "secret", expiresGrant()); err != nil {
		t.Fatal(err)
	}
	observer, err := New(dir, initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	observer.grantKeys.active = "next"
	if _, err := observer.RewrapGrantsBatch(context.Background(), 1);
		!errors.Is(err, ErrExecutionLeaseHeld) {
		t.Fatalf("observer rewrapped live worker's credentials: %v", err)
	}
	// A concurrent observer can read only aggregate key inventory.
	usage, err := observer.GrantKeyUsage(context.Background())
	if err != nil || usage.ByKey["old"] != 1 {
		t.Fatalf("observer inventory failed: %#v %v", usage, err)
	}
}
