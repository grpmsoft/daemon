package daemon

import "sync"

// Group manages multiple concurrent actors. When any actor returns,
// all others are interrupted. Modeled after oklog/run.Group but
// implemented from scratch with zero external dependencies.
type Group struct {
	actors []actor
}

type actor struct {
	execute   func() error
	interrupt func(error)
}

// Add registers an actor pair. execute is started in a goroutine by Run.
// interrupt is called when another actor's execute returns, with that
// actor's error. interrupt must cause execute to return promptly.
func (g *Group) Add(execute func() error, interrupt func(error)) {
	g.actors = append(g.actors, actor{execute: execute, interrupt: interrupt})
}

// Run starts all actors concurrently. It blocks until all actors have stopped.
// The first actor whose execute returns triggers interrupt on all others
// (including itself). Returns the error from the first actor that returned.
func (g *Group) Run() error {
	if len(g.actors) == 0 {
		return nil
	}

	// First return wins.
	errCh := make(chan error, len(g.actors))
	for _, a := range g.actors {
		go func() {
			errCh <- a.execute()
		}()
	}

	// Wait for the first actor to finish.
	firstErr := <-errCh

	// Interrupt all actors (including the one that already returned).
	for _, a := range g.actors {
		a.interrupt(firstErr)
	}

	// Wait for the remaining actors to finish.
	var wg sync.WaitGroup
	wg.Add(len(g.actors) - 1)
	for range len(g.actors) - 1 {
		go func() {
			<-errCh
			wg.Done()
		}()
	}
	wg.Wait()

	return firstErr
}
