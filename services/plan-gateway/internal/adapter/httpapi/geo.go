package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
	"github.com/mhayk/overpass/services/plan-gateway/internal/render"
)

// The geo renderings, and the conditional-request machinery they need.
//
// A globe polls the same horizon every few seconds and the answer only changes
// when a plan is committed. Without a validator, every poll re-sends an
// unchanged document — which is the difference between a live view and a merely
// expensive one.

// MediaTypeGeoJSON is what RFC 7946 registers. Not application/json: deck.gl
// and every other reader dispatch on it, and a proxy that transforms JSON will
// happily reorder a coordinate array.
const MediaTypeGeoJSON = "application/geo+json"

// footprintLimit bounds the collection.
//
// An unbounded geometry endpoint is a denial of service waiting for a client to
// ask for a year of coverage.
//
// 250, not 500. A realistic 33-vertex swath footprint renders to about 1.2 kB,
// measured — so 500 features is a 600 kB response, and a globe that has to
// parse 600 kB before drawing anything reads as broken however correct it is.
// See docs/payload-budget.md, which is generated from the measurement rather
// than written alongside it.
const footprintLimit = 250

func (s *Server) planCZML(w http.ResponseWriter, r *http.Request) {
	bucketStart, err := time.Parse(time.RFC3339, chi.URLParam(r, "bucket_start"))
	if err != nil {
		s.problem(w, r, badRequest("bucket_start must be an RFC 3339 timestamp"))
		return
	}
	var version *int
	if raw := r.URL.Query().Get("plan_version"); raw != "" {
		v, convErr := strconv.Atoi(raw)
		if convErr != nil || v < 1 {
			s.problem(w, r, badRequest("plan_version must be a positive integer"))
			return
		}
		version = &v
	}

	satelliteID := chi.URLParam(r, "satellite_id")
	plan, err := s.reads.Plan(r.Context(), satelliteID, bucketStart, version)
	switch {
	case errors.Is(err, port.ErrNotFound):
		s.problem(w, r, notFound("no plan for this satellite and bucket"))
		return
	case err != nil:
		s.unavailable(w, r, err)
		return
	}

	// The plan's own committed_at, not the global cursor. A CZML document is a
	// rendering of ONE plan, and tying its validator to a cursor that moves
	// whenever any unrelated stream advances would invalidate it constantly for
	// no change in content.
	cursor := port.Cursor{LastEventAt: plan.CommittedAt}
	document, err := render.PlanCZML(plan, cursor, s.now())
	if err != nil {
		s.log.Error("rendering CZML", slog.Any("error", err))
		s.problem(w, r, problemDoc{
			Type:   ProblemBase + "unrenderable-geometry",
			Title:  "Cannot render this plan",
			Status: http.StatusUnprocessableEntity,
			Detail: "A footprint in this plan is not valid GeoJSON. The plan itself is readable at /v1/plans.",
		})
		return
	}

	s.writeConditional(w, r, "application/json", document)
}

// constellationCZML serves the orbit tracks, independent of any plan.
//
// The globe's satellites. `planCZML` renders the orbit of the one satellite its
// plan belongs to; this renders the constellation, which exists whether or not
// anything has been scheduled. A viewer who sees satellites appear only once a
// plan is committed has been told something false about the system.
func (s *Server) constellationCZML(w http.ResponseWriter, r *http.Request) {
	start, err := timeParam(r, "window_start")
	if err != nil || start == nil {
		s.problem(w, r, badRequest("window_start is required and must be RFC 3339"))
		return
	}
	end, err := timeParam(r, "window_end")
	if err != nil || end == nil {
		s.problem(w, r, badRequest("window_end is required and must be RFC 3339"))
		return
	}
	if !end.After(*start) {
		s.problem(w, r, badRequest("window_end must be after window_start"))
		return
	}
	// An unbounded window over a table that grows by eighty thousand rows a day
	// is a denial of service waiting for a client to ask for a year. Bounded
	// rather than truncated: a track silently cut in half draws an orbit that
	// stops in mid-air, which is worse than being told to narrow the window.
	if end.Sub(*start) > maxConstellationWindow {
		s.problem(w, r, badRequest(
			"window is longer than "+maxConstellationWindow.String()+
				"; ask for a narrower one rather than receiving a truncated orbit"))
		return
	}

	tracks, cursor, err := s.reads.Constellation(
		r.Context(), r.URL.Query().Get("satellite_id"), *start, *end)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}

	document, err := render.ConstellationCZML(tracks, *start, *end, cursor, s.now())
	if err != nil {
		s.log.Error("rendering the constellation", slog.Any("error", err))
		s.unavailable(w, r, err)
		return
	}

	s.writeConditional(w, r, "application/json", document)
}

// maxConstellationWindow bounds the orbit query.
//
// Two days. The ephemeris sweep publishes a rolling day ahead (ADR-0016), so a
// window wider than that is asking for samples that do not exist yet — and the
// globe scrubs a plan bucket, which is three hours.
const maxConstellationWindow = 48 * time.Hour

func (s *Server) footprintsGeoJSON(w http.ResponseWriter, r *http.Request) {
	start, err := timeParam(r, "window_start")
	if err != nil || start == nil {
		s.problem(w, r, badRequest("window_start is required and must be RFC 3339"))
		return
	}
	end, err := timeParam(r, "window_end")
	if err != nil || end == nil {
		s.problem(w, r, badRequest("window_end is required and must be RFC 3339"))
		return
	}
	if !end.After(*start) {
		s.problem(w, r, badRequest("window_end must be after window_start"))
		return
	}

	limit := intParam(r, "limit")
	if limit <= 0 || limit > footprintLimit {
		limit = footprintLimit
	}

	// One more than asked for, so truncation is DETECTED rather than assumed.
	// Comparing len(result) == limit cannot distinguish "exactly limit rows
	// exist" from "more were cut", and reporting truncated on an exact fit
	// would make a client narrow a window that did not need narrowing.
	items, cursor, err := s.reads.Acquisitions(r.Context(), port.AcquisitionQuery{
		SatelliteID: r.URL.Query().Get("satellite_id"),
		WindowStart: *start,
		WindowEnd:   *end,
		RequestID:   r.URL.Query().Get("request_id"),
		Statuses:    r.URL.Query()["status"],
		Limit:       limit + 1,
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}

	truncated := len(items) > limit
	if truncated {
		items = items[:limit]
	}

	collection, err := render.FootprintsGeoJSON(items, truncated, cursor, s.now())
	if err != nil {
		s.log.Error("rendering GeoJSON", slog.Any("error", err))
		s.unavailable(w, r, err)
		return
	}

	s.writeConditional(w, r, MediaTypeGeoJSON, collection)
}

// writeConditional serves a rendered document, or 304 if the client has it.
//
// The tag is computed over the rendered BYTES, which is why both renderers
// marshal through ordered structs rather than maps: Go randomises map iteration
// per run, so a map-rendered document would produce a different tag on every
// request and the validator would never match.
func (s *Server) writeConditional(w http.ResponseWriter, r *http.Request, mediaType string, body []byte) {
	tag := etag(body)
	w.Header().Set("ETag", tag)

	// Cached, but always revalidated. no-cache is not "do not cache" — it means
	// "check with me first", which is exactly the contract: the client may keep
	// the bytes forever and must ask whether they are still current.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Accept")

	if matches(r.Header.Get("If-None-Match"), tag) {
		// No body and no Content-Type. A 304 carrying a payload defeats the
		// entire point of asking.
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if _, err := w.Write(body); err != nil {
		s.log.Error("writing geo response", slog.Any("error", err))
	}
}

// etag is a strong validator over the exact bytes.
//
// Strong, not weak: these documents are generated deterministically, so equal
// tags really do mean identical octets. A weak tag would permit "semantically
// equivalent", which buys nothing here and makes the guarantee harder to reason
// about.
func etag(body []byte) string {
	sum := sha256.Sum256(body)
	// Half the digest. 128 bits of a SHA-256 is far past any collision concern
	// for a cache validator, and it halves a header sent on every response.
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// matches implements RFC 9110 §13.1.2 for the cases that can occur here.
//
// A list, because a client legitimately holds several representations and sends
// them all. `*` matches anything the server has, which for a 200-able response
// is always true.
func matches(ifNoneMatch, tag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		// W/ prefixes are stripped before comparing. Only strong tags are ever
		// issued here, but a proxy is allowed to weaken one in transit, and
		// refusing to match then would silently disable the whole mechanism.
		if strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}
