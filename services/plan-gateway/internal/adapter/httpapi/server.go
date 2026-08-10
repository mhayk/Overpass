// Package httpapi is the REST adapter for the read models.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mhayk/overpass/services/plan-gateway/internal/domain"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// ProblemBase is the URI namespace clients branch on, shared with tasking-api.
const ProblemBase = "https://overpass.dev/problems/"

// Server routes the read API.
type Server struct {
	reads  port.Reads
	health func() error
	now    func() time.Time
	log    *slog.Logger
}

// New builds the router.
func New(reads port.Reads, health func() error, now func() time.Time, log *slog.Logger) *Server {
	return &Server{reads: reads, health: health, now: now, log: log}
}

// Routes returns the wired handler.
//
// otelhttp wraps the whole router rather than sitting inside chi's chain, so
// the server span is the OUTERMOST thing in the request and encloses routing.
// The filter, the span-name formatter and the RouteTag middleware are the same
// as tasking-api's, deliberately copied rather than varied: the two services
// must produce the same label shape, or every dashboard panel needs two
// queries instead of one.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(RouteTag)
	r.Get("/healthz", s.liveness)
	r.Get("/readyz", s.readiness)
	r.Get("/v1/plans", s.listPlans)
	r.Get("/v1/plans/{satellite_id}/{bucket_start}", s.getPlan)
	r.Get("/v1/acquisitions", s.listAcquisitions)
	r.Get("/v1/requests/{request_id}", s.getRequest)
	r.Get("/v1/requests/{request_id}/opportunities", s.listOpportunities)

	// The geo renderings, per ADR-0009: CZML for Cesium, GeoJSON for deck.gl,
	// both from the same read model so the two views cannot disagree.
	r.Get("/v1/geo/plans/{satellite_id}/{bucket_start}/czml", s.planCZML)
	r.Get("/v1/geo/satellites/czml", s.constellationCZML)
	r.Get("/v1/geo/footprints", s.footprintsGeoJSON)

	return otelhttp.NewHandler(r, "plan-gateway",
		// Probes excluded. A liveness check every five seconds is the
		// highest-volume and least interesting operation this service
		// performs; leaving it in makes the error ratio meaningless, because a
		// 100% failure rate on a real route disappears into a denominator of
		// probes.
		otelhttp.WithFilter(func(req *http.Request) bool {
			return req.URL.Path != "/healthz" && req.URL.Path != "/readyz"
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
			if route := chi.RouteContext(req.Context()); route != nil && route.RoutePattern() != "" {
				return req.Method + " " + route.RoutePattern()
			}
			return req.Method + " " + req.URL.Path
		}),
	)
}

func (s *Server) liveness(w http.ResponseWriter, _ *http.Request) {
	// Checks nothing external. A liveness probe that fails on a dependency
	// outage causes a restart loop that makes the outage worse.
	s.write(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) readiness(w http.ResponseWriter, _ *http.Request) {
	// Postgres only, and deliberately NOT projection lag. A gateway that is
	// behind still serves correct data with as_of attached; taking it out of
	// rotation would replace stale answers with no answers.
	if err := s.health(); err != nil {
		s.write(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"checks": map[string]any{
				"postgres": map[string]any{"status": "failed", "detail": err.Error()},
			},
		})
		return
	}
	s.write(w, http.StatusOK, map[string]any{
		"status": "ok",
		"checks": map[string]any{"postgres": map[string]any{"status": "ok"}},
	})
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	q := port.PlanQuery{
		SatelliteID:       r.URL.Query().Get("satellite_id"),
		IncludeSuperseded: r.URL.Query().Get("include_superseded") == "true",
		Limit:             intParam(r, "limit"),
	}
	var err error
	if q.BucketStartAfter, err = timeParam(r, "bucket_start_after"); err != nil {
		s.problem(w, r, badRequest(err.Error()))
		return
	}
	if q.BucketStartBefore, err = timeParam(r, "bucket_start_before"); err != nil {
		s.problem(w, r, badRequest(err.Error()))
		return
	}

	plans, cursor, err := s.reads.Plans(r.Context(), q)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	s.write(w, http.StatusOK, map[string]any{
		"items":     planItems(plans),
		"staleness": s.staleness(cursor),
	})
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
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

	plan, err := s.reads.Plan(r.Context(), chi.URLParam(r, "satellite_id"), bucketStart, version)
	switch {
	case errors.Is(err, port.ErrNotFound):
		s.problem(w, r, notFound("no plan for this satellite and bucket"))
		return
	case err != nil:
		s.unavailable(w, r, err)
		return
	}

	body := planBody(plan)
	body["staleness"] = s.staleness(port.Cursor{LastEventAt: plan.CommittedAt})
	s.write(w, http.StatusOK, body)
}

func (s *Server) listAcquisitions(w http.ResponseWriter, r *http.Request) {
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

	items, cursor, err := s.reads.Acquisitions(r.Context(), port.AcquisitionQuery{
		SatelliteID: r.URL.Query().Get("satellite_id"),
		WindowStart: *start,
		WindowEnd:   *end,
		RequestID:   r.URL.Query().Get("request_id"),
		Statuses:    r.URL.Query()["status"],
		Limit:       intParam(r, "limit"),
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	s.write(w, http.StatusOK, map[string]any{
		"items":     acquisitionItems(items),
		"staleness": s.staleness(cursor),
	})
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request) {
	view, err := s.reads.Request(r.Context(), chi.URLParam(r, "request_id"))
	switch {
	case errors.Is(err, port.ErrNotFound):
		s.problem(w, r, notFound("no such request"))
		return
	case err != nil:
		s.unavailable(w, r, err)
		return
	}

	body := map[string]any{
		"request_id":        view.RequestID,
		"customer_id":       view.CustomerID,
		"target_name":       view.TargetName,
		"state":             view.State,
		"window":            window(view.WindowStart, view.WindowEnd),
		"opportunity_count": view.OpportunityCount,
		"staleness":         s.staleness(port.Cursor{LastEventAt: view.LastEventAt}),
	}
	if len(view.UnfulfilmentJSON) > 0 {
		body["unfulfilment"] = json.RawMessage(view.UnfulfilmentJSON)
	}
	s.write(w, http.StatusOK, body)
}

func (s *Server) listOpportunities(w http.ResponseWriter, r *http.Request) {
	items, cursor, err := s.reads.RequestOpportunities(r.Context(), chi.URLParam(r, "request_id"))
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, o := range items {
		out = append(out, map[string]any{
			"opportunity_id":         o.OpportunityID,
			"satellite_id":           o.SatelliteID,
			"mode":                   o.Mode,
			"access_window":          window(o.AccessStart, o.AccessEnd),
			"acquisition_duration_s": o.AcquisitionDurationS,
			"orbit_number":           o.OrbitNumber,
			"quality_score":          o.QualityScore,
			"footprint":              json.RawMessage(o.FootprintGeoJSON),
			"won":                    o.Won,
		})
	}
	s.write(w, http.StatusOK, map[string]any{
		"items":     out,
		"staleness": s.staleness(cursor),
	})
}

// staleness renders how current the answer is.
//
// On every response. A read model that cannot say how far behind it is forces
// the UI to guess, and the UI guesses optimistically.
func (s *Server) staleness(c port.Cursor) map[string]any {
	st := domain.StalenessAt(c.LastEventAt, s.now())
	return map[string]any{
		"as_of":       st.AsOf.UTC().Format(time.RFC3339Nano),
		"lag_seconds": st.LagSeconds,
	}
}

func planItems(plans []port.PlanView) []map[string]any {
	out := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		out = append(out, planBody(p))
	}
	return out
}

func planBody(p port.PlanView) map[string]any {
	body := map[string]any{
		"plan_id":      p.PlanID,
		"satellite_id": p.SatelliteID,
		"bucket":       window(p.BucketStart, p.BucketEnd),
		"plan_version": p.PlanVersion,
		"superseded":   p.Superseded,
		"policy":       p.Policy,
		"committed_at": p.CommittedAt.UTC().Format(time.RFC3339Nano),
		"acquisitions": acquisitionItems(p.Acquisitions),
	}
	if p.SupersedesPlanID != nil {
		body["supersedes_plan_id"] = *p.SupersedesPlanID
	}
	if len(p.MetricsJSON) > 0 {
		body["metrics"] = json.RawMessage(p.MetricsJSON)
	}
	return body
}

func acquisitionItems(items []port.AcquisitionView) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		entry := map[string]any{
			"acquisition_id":        a.AcquisitionID,
			"request_id":            a.RequestID,
			"customer_id":           a.CustomerID,
			"satellite_id":          a.SatelliteID,
			"mode":                  a.Mode,
			"window":                window(a.WindowStart, a.WindowEnd),
			"status":                a.Status,
			"footprint":             json.RawMessage(a.FootprintGeoJSON),
			"awarded_value_credits": a.AwardedValueCredits,
		}
		if a.SlewTimeFromPreviousS != nil {
			entry["slew_time_from_previous_s"] = *a.SlewTimeFromPreviousS
		}
		if a.GapFromPreviousS != nil {
			entry["gap_from_previous_s"] = *a.GapFromPreviousS
		}
		out = append(out, entry)
	}
	return out
}

func window(start, end time.Time) map[string]string {
	return map[string]string{
		"start": start.UTC().Format(time.RFC3339Nano),
		"end":   end.UTC().Format(time.RFC3339Nano),
	}
}

func timeParam(r *http.Request, name string) (*time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil //nolint:nilnil // absent is not an error for an optional parameter
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC 3339 timestamp", name)
	}
	return &t, nil
}

func intParam(r *http.Request, name string) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return 0
	}
	return v
}

func (s *Server) write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("response encoding failed", slog.Any("error", err))
	}
}

// problem writes an RFC 9457 document.
//
// The status is a typed field rather than a key read back out of the body map.
// The first version fished it out with a type assertion, so a body built with
// `"status": 404` as an untyped constant that landed as float64 would have
// written a 200 with a 404-shaped body — the worst possible outcome, since a
// client checks the status line, not the document.
type problemDoc struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func (s *Server) problem(w http.ResponseWriter, _ *http.Request, p problemDoc) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		s.log.Error("problem encoding failed", slog.Any("error", err))
	}
}

func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	// Distinct from an empty result on purpose: "there is no plan" and "I
	// cannot tell you whether there is a plan" are different answers.
	s.log.Error("read failed", slog.Any("error", err))
	s.problem(w, r, problemDoc{
		Type:   ProblemBase + "read-model-unavailable",
		Title:  "Read model unavailable",
		Status: http.StatusServiceUnavailable,
		Detail: "The read model could not be reached. This is not an empty result.",
	})
}

func badRequest(detail string) problemDoc {
	return problemDoc{
		Type:   ProblemBase + "malformed-request",
		Title:  "Malformed request",
		Status: http.StatusBadRequest,
		Detail: detail,
	}
}

func notFound(detail string) problemDoc {
	return problemDoc{
		Type:   ProblemBase + "not-found",
		Title:  "Not found",
		Status: http.StatusNotFound,
		Detail: detail,
	}
}
