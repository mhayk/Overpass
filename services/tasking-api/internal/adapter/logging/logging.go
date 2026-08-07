// Package logging builds the structured logger.
//
// Its own package rather than a function in main, because main is a
// composition root and a composition root with logic in it is logic that
// cannot be tested. The level mapping in particular is the sort of thing that
// is silently wrong — an unrecognised value quietly meaning "info" is fine, an
// unrecognised value quietly meaning "error" is an outage nobody can see.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON logger at the requested level, tagged with the service.
//
// JSON always, including in development. A format that differs between
// environments is a format whose parsing bugs are found in production.
func New(w io.Writer, service, version, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: Level(level)})).With(
		slog.String("service", service),
		slog.String("version", version),
	)
}

// Level maps a configured name onto a slog level.
//
// Anything unrecognised is info. Config validation rejects unknown values
// before this is reached, so the fallback is a second line of defence rather
// than the policy — but it defaults to the noisier option on purpose, because
// too many logs is a nuisance and too few is an investigation that cannot
// happen.
func Level(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
