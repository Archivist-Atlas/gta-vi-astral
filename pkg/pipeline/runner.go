package pipeline

import (
	"context"
	"sync"
)

// RunParallel exécute fn sur tous les éléments de items avec au plus jobs goroutines.
func RunParallel[T any](ctx context.Context, jobs int, items []T, fn func(context.Context, int, T) error) error {
	if jobs <= 0 {
		jobs = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type task struct {
		idx  int
		item T
	}
	ch := make(chan task, jobs*2)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)

	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				if ctx.Err() != nil {
					return
				}
				if err := fn(ctx, t.idx, t.item); err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}

	for i, item := range items {
		select {
		case <-ctx.Done():
			break
		case ch <- task{idx: i, item: item}:
		}
	}
	close(ch)
	wg.Wait()
	return firstErr
}
