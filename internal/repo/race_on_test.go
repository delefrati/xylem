//go:build race

package repo_test

// raceEnabled shrinks heavy tests under -race (about 6x slower per changeset).
const raceEnabled = true
