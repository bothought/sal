package session

import (
	"fmt"
	"sync"
	"testing"

	"github.com/bothought/sal/internal/agent"
)

// TestTwoProcessesAppendWithoutLosingMessages covers two sal processes writing
// to the same conversation at once: every message is stored exactly once, in
// one contiguous order, and the database never reports a constraint failure.
func TestTwoProcessesAppendWithoutLosingMessages(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	first, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	a, _, err := first.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	b, resumed, err := second.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed || b.ID != a.ID {
		t.Fatalf("the two connections disagree: %q vs %q", a.ID, b.ID)
	}

	const each = 10
	var wg sync.WaitGroup
	errs := make(chan error, 2*each)
	for i := 0; i < each; i++ {
		for _, pair := range []struct {
			s    *Session
			text string
		}{{a, fmt.Sprintf("first-%d", i)}, {b, fmt.Sprintf("second-%d", i)}} {
			wg.Add(1)
			go func(s *Session, text string) {
				defer wg.Done()
				if err := s.AppendMessage(agent.NewUserMessage(text)); err != nil {
					errs <- err
				}
			}(pair.s, pair.text)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append failed: %v", err)
	}

	loaded, err := first.Load(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2*each {
		t.Fatalf("stored %d messages, want %d", len(loaded.Messages), 2*each)
	}
	seen := map[string]bool{}
	for _, m := range loaded.Messages {
		if seen[m.Content] {
			t.Fatalf("message stored twice: %q", m.Content)
		}
		seen[m.Content] = true
	}
	if len(seen) != 2*each {
		t.Fatalf("expected %d distinct messages, got %d", 2*each, len(seen))
	}
}
