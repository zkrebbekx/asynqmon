package asynqmon

import (
	"context"
	"errors"
	"sync"
)

// flakyViewStore is an in-memory viewStore whose first `failures` Put calls
// fail. It backs the background-seeding retry test.
type flakyViewStore struct {
	mu       sync.Mutex
	failures int
	tries    int
	views    map[string]View
}

func (s *flakyViewStore) List(ctx context.Context) ([]View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]View, 0, len(s.views))
	for _, v := range s.views {
		out = append(out, v)
	}
	return out, nil
}

func (s *flakyViewStore) Get(ctx context.Context, id string) (View, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.views[id]
	return v, ok, nil
}

func (s *flakyViewStore) Put(ctx context.Context, v View) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries++
	if s.failures > 0 {
		s.failures--
		return errors.New("view store unavailable")
	}
	s.views[v.ID] = v
	return nil
}

func (s *flakyViewStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.views, id)
	return nil
}

func (s *flakyViewStore) Count(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.views)), nil
}

func (s *flakyViewStore) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tries
}

func (s *flakyViewStore) all() map[string]View {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]View, len(s.views))
	for k, v := range s.views {
		out[k] = v
	}
	return out
}
