package backup

import "time"

// SetCheckpointEvery shortens how often a run notes finished files, for the
// duration of a test.
func SetCheckpointEvery(commit, checkpoint time.Duration) (restore func()) {
	c, k := commitEvery, checkpointEvery
	commitEvery, checkpointEvery = commit, checkpoint
	return func() { commitEvery, checkpointEvery = c, k }
}
