package backup

import (
	"context"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// How often a run notes finished files in the index while it is still going.
//
// A run used to write the index once, at the end, so a backup cut off by a
// crash, a kill, a shutdown or a laptop lid lost the record of everything it
// had finished, and the next run sent every one of those files again. Noting
// them as they land means an interrupted backup carries on where it stopped.
//
// commitEvery is for storage that has a file once its upload returns (R2,
// S3): the index is written that often, in one bbolt transaction each time.
// checkpointEvery is for storage that holds writes back -- bundling's packs
// and index, Drime's manifest of dates -- where a file is only safe once the
// store has saved what it holds, which costs a request or two, so it is done
// less often. Variables so tests can shorten them.
var (
	commitEvery     = 2 * time.Second
	checkpointEvery = 15 * time.Second
)

// checkpointer notes landed files in the index during a run. Files arrive
// from the engine as they land; one goroutine decides when they are safely on
// record in storage and writes them to the index, so neither the bbolt write
// nor a store checkpoint ever holds up an upload.
//
// Only uploads are noted mid-run. Moves are left to the end of the run: the
// engine deletes a moved file's old key only once everything else is done,
// and recording the move before that would let a cut-off run forget an old
// object that is still in storage.
type checkpointer struct {
	db     *index.DB
	set    string
	client remote.Store

	mu     sync.Mutex
	landed []index.Record // uploaded, not yet in the index

	stop chan struct{}
	done chan struct{}
	last time.Time // when the store was last checkpointed
}

func newCheckpointer(db *index.DB, set string, client remote.Store) *checkpointer {
	return &checkpointer{db: db, set: set, client: client, last: time.Now()}
}

// add queues one landed file. It is called from the engine's result drain,
// so it only appends.
func (c *checkpointer) add(r index.Record) {
	c.mu.Lock()
	c.landed = append(c.landed, r)
	c.mu.Unlock()
}

// start begins noting landed files in the background.
func (c *checkpointer) start(ctx context.Context) {
	c.stop = make(chan struct{})
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		t := time.NewTicker(commitEvery)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				c.tick(ctx)
			}
		}
	}()
}

// halt stops the background goroutine and returns what has landed but is
// not in the index yet. The caller records those once the store's final
// flush has put them on record.
func (c *checkpointer) halt() []index.Record {
	if c.stop != nil {
		close(c.stop)
		<-c.done
		c.stop = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.landed
	c.landed = nil
	return out
}

// tick writes to the index what the store has on record. The files are taken
// before the store is asked, so every one of them had landed by the time the
// store says nothing is held back -- or by the time a checkpoint started.
func (c *checkpointer) tick(ctx context.Context) {
	c.mu.Lock()
	batch := c.landed
	c.landed = nil
	c.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	safe := !remote.PendingStore(c.client)
	if !safe && time.Since(c.last) >= checkpointEvery {
		c.last = time.Now()
		ok, err := remote.CheckpointStore(ctx, c.client)
		safe = ok && err == nil
	}
	// A failed index write is not an error yet: the files stay queued and
	// the end of the run writes them, or reports why it could not.
	if safe && c.db.PutMany(c.set, batch) == nil {
		return
	}
	// Not on record yet: keep them, ahead of anything that landed since.
	c.mu.Lock()
	c.landed = append(batch, c.landed...)
	c.mu.Unlock()
}
