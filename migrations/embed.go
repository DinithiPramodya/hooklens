// Package migrations holds the SQL migration files and embeds them into the
// binary.
//
// The directory is a Go package purely so that //go:embed can reach the .sql
// files -- embed can only see files in or below its own package directory. The
// payoff is that `hooklens migrate up` needs nothing on disk but the binary
// itself, which matters at deploy time: no separate tool to install, no
// migrations directory to copy alongside, and no way for the binary and its
// migrations to be different versions.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
