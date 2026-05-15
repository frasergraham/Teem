package messaging

import (
	"context"
	"errors"
	"testing"
)

type recordingNotifier struct {
	calls []Message
	err   error
}

func (r *recordingNotifier) Notify(_ context.Context, msg Message) error {
	r.calls = append(r.calls, msg)
	return r.err
}

func TestMultiNotifier_FansOut(t *testing.T) {
	a := &recordingNotifier{}
	b := &recordingNotifier{}
	m := MultiNotifier{a, b}
	if err := m.Notify(context.Background(), Message{Title: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(a.calls) != 1 || len(b.calls) != 1 {
		t.Fatalf("fan-out: a=%d b=%d", len(a.calls), len(b.calls))
	}
}

func TestMultiNotifier_JoinsErrors(t *testing.T) {
	a := &recordingNotifier{err: errors.New("a-bad")}
	b := &recordingNotifier{err: errors.New("b-bad")}
	err := MultiNotifier{a, b}.Notify(context.Background(), Message{})
	if err == nil || !errors.Is(err, a.err) || !errors.Is(err, b.err) {
		t.Fatalf("expected joined error; got %v", err)
	}
}

func TestMultiNotifier_NilEntriesSkipped(t *testing.T) {
	b := &recordingNotifier{}
	if err := (MultiNotifier{nil, b, nil}).Notify(context.Background(), Message{}); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 1 {
		t.Fatalf("expected 1 call on non-nil entry, got %d", len(b.calls))
	}
}
