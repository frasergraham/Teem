package notes

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNotes_WriteAndUnread(t *testing.T) {
	dir := t.TempDir()
	inbox, err := Open(filepath.Join(dir, "notes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()

	if err := inbox.Write(Note{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := inbox.Write(Note{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	got, err := inbox.Unread()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unread: %d want 2", len(got))
	}
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("order: %v", got)
	}
}

func TestNotes_MarkAllRead(t *testing.T) {
	dir := t.TempDir()
	inbox, err := Open(filepath.Join(dir, "notes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	_ = inbox.Write(Note{Text: "old", Timestamp: time.Now().Add(-time.Minute)})
	_ = inbox.Write(Note{Text: "newer"})
	if err := inbox.MarkAllRead(); err != nil {
		t.Fatal(err)
	}
	got, _ := inbox.Unread()
	if len(got) != 0 {
		t.Errorf("after MarkAllRead expected 0 unread, got %d", len(got))
	}

	// New writes after cursor surface as unread.
	_ = inbox.Write(Note{Text: "fresh"})
	got, _ = inbox.Unread()
	if len(got) != 1 || got[0].Text != "fresh" {
		t.Errorf("post-cursor unread: %+v", got)
	}
}

func TestNotes_EmptyInboxIsOK(t *testing.T) {
	inbox, err := Open(filepath.Join(t.TempDir(), "notes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	got, err := inbox.Unread()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty inbox: %d", len(got))
	}
}
