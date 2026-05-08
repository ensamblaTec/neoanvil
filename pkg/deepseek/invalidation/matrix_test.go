package invalidation

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ensamblatec/neoanvil/pkg/deepseek/cache"
	"github.com/ensamblatec/neoanvil/pkg/deepseek/session"
)

// --- helpers ---

func openDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "test.db"), 0600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newStore(t *testing.T) *session.ThreadStore {
	t.Helper()
	s, err := session.NewThreadStore(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeEventBus is an in-memory EventBusSubscriber for tests.
type fakeEventBus struct {
	mu      sync.Mutex
	subs    []chan EventFileMutated
	expired []EventDeepSeekThreadExpired
}

func (f *fakeEventBus) Subscribe(ctx context.Context) <-chan EventFileMutated {
	ch := make(chan EventFileMutated, 8)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

func (f *fakeEventBus) Publish(evt EventDeepSeekThreadExpired) {
	f.mu.Lock()
	f.expired = append(f.expired, evt)
	f.mu.Unlock()
}

func (f *fakeEventBus) emit(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- EventFileMutated{Path: path}
	}
}

// --- tests ---

func TestMutationExpiresThread(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "auth.go")
	os.WriteFile(filePath, []byte("package auth"), 0600) //nolint:errcheck

	store := newStore(t)
	thread, err := store.Create([]string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	bus := &fakeEventBus{}
	ctx := t.Context()

	m := New(store, cache.NewTracker(), bus, nil, 30000, 15*time.Minute, 5*time.Minute)
	m.SubscribeToMutations(ctx)

	bus.emit(filePath)
	time.Sleep(50 * time.Millisecond) // let goroutine process

	got, err := store.Get(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.ThreadStatusExpired {
		t.Errorf("thread status = %s, want expired", got.Status)
	}
}

func TestMutationOnUnrelatedFileDoesNotExpire(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.go")
	otherPath := filepath.Join(dir, "other.go")
	os.WriteFile(authPath, []byte("pkg"), 0600) //nolint:errcheck

	store := newStore(t)
	thread, _ := store.Create([]string{authPath}) //nolint:errcheck

	bus := &fakeEventBus{}
	ctx := t.Context()

	m := New(store, cache.NewTracker(), bus, nil, 30000, 15*time.Minute, 5*time.Minute)
	m.SubscribeToMutations(ctx)

	bus.emit(otherPath) // different file
	time.Sleep(50 * time.Millisecond)

	got, _ := store.Get(thread.ID) //nolint:errcheck
	if got.Status != session.ThreadStatusActive {
		t.Errorf("unrelated mutation should not expire thread")
	}
}

func TestPressureTriggersDistillation(t *testing.T) {
	store := newStore(t)
	thread, _ := store.Create(nil) //nolint:errcheck

	distilled := false
	distillFn := func(_ context.Context, _ []session.Message) (string, error) {
		distilled = true
		return "summary", nil
	}

	m := New(store, cache.NewTracker(), nil, distillFn, 10, 15*time.Minute, 5*time.Minute)

	// Push token count above threshold.
	store.Append(thread.ID, session.Message{Role: "user", Content: "big", TokensUsed: 20}) //nolint:errcheck

	m.CheckPressure(context.Background(), thread.ID)

	if !distilled {
		t.Error("distillFn not called when token count exceeds threshold")
	}
}

func TestPressureSkippedWhenBelowThreshold(t *testing.T) {
	store := newStore(t)
	thread, _ := store.Create(nil) //nolint:errcheck

	called := false
	distillFn := func(_ context.Context, _ []session.Message) (string, error) {
		called = true
		return "summary", nil
	}

	m := New(store, cache.NewTracker(), nil, distillFn, 30000, 15*time.Minute, 5*time.Minute)

	store.Append(thread.ID, session.Message{Role: "user", Content: "x", TokensUsed: 5}) //nolint:errcheck

	m.CheckPressure(context.Background(), thread.ID)

	if called {
		t.Error("distillFn must not be called when below threshold")
	}
}

func TestReaperExpiresOldThread(t *testing.T) {
	store := newStore(t)
	thread, _ := store.Create(nil) //nolint:errcheck

	// Simulate a thread that has been idle longer than the TTL by setting a very short TTL.
	m := New(store, cache.NewTracker(), nil, nil, 30000, 10*time.Millisecond, 5*time.Minute)

	time.Sleep(30 * time.Millisecond) // let TTL expire

	// Run reap manually via StartReaper + short tick.
	ctx, cancel := context.WithCancel(context.Background())
	m.reaperInterval = 10 * time.Millisecond
	m.StartReaper(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	got, _ := store.Get(thread.ID) //nolint:errcheck
	if got.Status != session.ThreadStatusExpired {
		t.Errorf("old thread should be expired by reaper")
	}
}

func TestReaperSkipsActiveThread(t *testing.T) {
	store := newStore(t)
	thread, _ := store.Create(nil) //nolint:errcheck

	m := New(store, cache.NewTracker(), nil, nil, 30000, 10*time.Minute, 5*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	m.reaperInterval = 10 * time.Millisecond
	m.StartReaper(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()

	got, _ := store.Get(thread.ID) //nolint:errcheck
	if got.Status != session.ThreadStatusActive {
		t.Errorf("recently active thread must not be reaped")
	}
}

func TestReaperPublishesExpiredEvent(t *testing.T) {
	store := newStore(t)
	store.Create(nil) //nolint:errcheck

	bus := &fakeEventBus{}
	m := New(store, cache.NewTracker(), bus, nil, 30000, 1*time.Millisecond, 5*time.Minute)

	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	m.reaperInterval = 10 * time.Millisecond
	m.StartReaper(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()

	bus.mu.Lock()
	n := len(bus.expired)
	bus.mu.Unlock()

	if n == 0 {
		t.Error("reaper should publish EventDeepSeekThreadExpired for expired threads")
	}
}
