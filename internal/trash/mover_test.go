package trash

import (
	"context"
	"testing"
	"time"
)

// moverBackend is the fake with a server-side Move, as Drime has.
type moverBackend struct {
	*fakeBackend
	moveCalls int
}

func (m *moverBackend) Move(ctx context.Context, src, dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moveCalls++
	size := m.objects[src]
	delete(m.objects, src)
	m.objects[dst] = size
	return nil
}

// A store that can move is asked to move, not to copy: on Drime a copy takes
// seconds a file, and trashing is the common case of every backup run.
func TestMoveUsesServerMoveWhenAvailable(t *testing.T) {
	b := &moverBackend{fakeBackend: newFakeBackend()}
	b.seed("machines/pc/Docs/current/a.txt", 3)
	tr := New(b, Clock{Now: func() time.Time { return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC) }})
	res, err := tr.Move(context.Background(), "machines/pc/Docs", []string{"a.txt"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if b.moveCalls != 1 || b.copyCalls != 0 {
		t.Fatalf("move=%d copy=%d", b.moveCalls, b.copyCalls)
	}
	if len(res.Moved) != 1 || !b.has(res.Moved[0].TrashKey) || b.has("machines/pc/Docs/current/a.txt") {
		t.Fatalf("result %+v, objects %v", res, b.objects)
	}
}
