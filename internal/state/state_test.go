package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fingerprint == "" {
		t.Fatal("no fingerprint generated")
	}

	exp := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	if err := st.SetTokens(ctx, Tokens{"A", "R", exp}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSent(ctx, "a1", Sent{Messages: map[string][]int{"f1": {1, 2}, "f2": {3}}, Checksum: "x"}); err != nil {
		t.Fatal(err)
	}
	// Re-marking replaces rather than accumulates.
	if err := st.MarkSent(ctx, "a1", Sent{Messages: map[string][]int{"f1": {9}}, Checksum: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSent(ctx, "a2", Sent{Messages: map[string][]int{"f1": {4}}}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if st2.Fingerprint != st.Fingerprint {
		t.Errorf("fingerprint changed across reopen")
	}
	tk, err := st2.GetTokens(ctx)
	if err != nil || tk.AccessToken != "A" || tk.RefreshToken != "R" || !tk.Expiry.Equal(exp) {
		t.Errorf("tokens = %+v, %v", tk, err)
	}
	all, err := st2.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all["a1"].Checksum != "y" || len(all["a1"].Messages) != 1 || all["a1"].Messages["f1"][0] != 9 {
		t.Errorf("snapshot = %+v", all)
	}
	if err := st2.Forget(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st2.Has(ctx, "a1"); ok {
		t.Error("a1 should be gone")
	}
	if n, _ := st2.Count(ctx); n != 1 {
		t.Errorf("count = %d", n)
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion == 0 {
		t.Fatal("schema version not recorded")
	}
	v := st.SchemaVersion
	st.Close()

	// Reopening an up-to-date database must be a no-op.
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.SchemaVersion != v {
		t.Errorf("version changed on reopen: %d -> %d", v, st.SchemaVersion)
	}
}
