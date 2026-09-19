package state

import (
	"context"
	"os"
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

func TestImportLegacyV2(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	if err := os.WriteFile(legacy, []byte(`{
	  "version": 2,
	  "device_fingerprint": "fp-legacy",
	  "skylight_tokens": {"access_token": "A", "refresh_token": "R", "expiry": "2030-01-01T00:00:00Z"},
	  "sent": {"asset-1": {"messages": {"111": [5, 6]}, "checksum": "c", "sent_at": "2026-01-02T03:04:05Z"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Fingerprint != "fp-legacy" {
		t.Errorf("fingerprint = %q", st.Fingerprint)
	}
	tk, _ := st.GetTokens(ctx)
	if tk.RefreshToken != "R" || tk.Expiry.Year() != 2030 {
		t.Errorf("tokens = %+v", tk)
	}
	rec, ok, _ := st.Get(ctx, "asset-1")
	if !ok || rec.Checksum != "c" || len(rec.Messages["111"]) != 2 || rec.SentAt.Year() != 2026 {
		t.Errorf("record = %+v ok=%v", rec, ok)
	}
	if _, err := os.Stat(legacy + ".imported"); err != nil {
		t.Errorf("legacy file not renamed: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file still present")
	}
}

func TestImportLegacyV1(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{
	  "version": 1,
	  "device_fingerprint": "fp1",
	  "skylight_tokens": {},
	  "sent": {"x": {"message_ids": [7, 8], "frame_ids": ["111", "222"], "sent_at": "2026-01-01T00:00:00Z"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, ok, _ := st.Get(ctx, "x")
	if !ok || rec.Messages["111"][0] != 7 || rec.Messages["222"][0] != 8 {
		t.Errorf("v1 import = %+v ok=%v", rec, ok)
	}
}

func TestNoImportWhenDBPopulated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	st, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	fp := st.Fingerprint
	st.Close()

	// A stray legacy file must not be imported over an initialized database.
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"device_fingerprint":"other","sent":{"z":{"messages":{}}}}`), 0o600)
	st, err = Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Fingerprint != fp {
		t.Errorf("fingerprint overwritten by legacy import")
	}
	if n, _ := st.Count(ctx); n != 0 {
		t.Errorf("legacy records imported into populated db")
	}
}
