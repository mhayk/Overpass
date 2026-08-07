package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/app"
	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
	"github.com/mhayk/overpass/services/tasking-api/internal/port"
)

// maxBodyBytes caps an inbound submission.
//
// A polygon with a few thousand vertices is a large but legitimate request; a
// hundred megabytes is not, and reading it before deciding that is how a single
// client exhausts the process.
const maxBodyBytes = 1 << 20 // 1 MiB

// submitBody mirrors SubmitTaskingRequest from the OpenAPI document.
//
// Hand-written rather than the generated type because decoding needs to
// distinguish "absent" from "zero", and it needs to fail on unknown fields —
// neither of which the generated struct expresses. The shape is asserted
// against the contract by the round-trip tests in gen/go/contracttest.
type submitBody struct {
	CustomerID     *string          `json:"customer_id"`
	TargetName     *string          `json:"target_name"`
	Target         *geoJSONGeometry `json:"target"`
	Window         *timeWindow      `json:"window"`
	PriorityTier   *string          `json:"priority_tier"`
	BidCredits     *int64           `json:"bid_credits"`
	RequestedModes []string         `json:"requested_modes"`
	Constraints    *constraintsBody `json:"constraints"`
}

type timeWindow struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

type constraintsBody struct {
	LookSide        string   `json:"look_side"`
	MinIncidenceDeg *float64 `json:"min_incidence_deg"`
	MaxIncidenceDeg *float64 `json:"max_incidence_deg"`
	MaxSquintDeg    *float64 `json:"max_squint_deg"`
}

// geoJSONGeometry decodes a Point or Polygon without committing to a shape
// until the type is known — the coordinates member is a different depth for
// each, and a single struct cannot hold both.
type geoJSONGeometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

// submit handles POST /v1/tasking-requests.
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	log := LoggerFrom(r.Context(), s.log)

	// Required, not optional. Optional means the default behaviour is unsafe
	// under retry, and clients discover that in production — the customer pays
	// twice and gets one image.
	key := r.Header.Get("Idempotency-Key")
	if !domain.IdempotencyKeyValid(key) {
		s.writeProblem(w, r, badRequest(
			"Idempotency-Key header is required and must be 8-128 characters of [A-Za-z0-9._~-]"))
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.writeProblem(w, r, badRequest("request body exceeds the 1 MiB limit"))
		return
	}

	// Fingerprint BEFORE decoding, over the canonical form. A digest of the raw
	// bytes would make a retry that reserialised — which many HTTP clients do —
	// look like a different request and earn a 409.
	fingerprint, err := domain.FingerprintBody(raw)
	if err != nil {
		s.writeProblem(w, r, badRequest("request body is not valid JSON: "+err.Error()))
		return
	}

	body, problem := decodeSubmit(raw)
	if problem != nil {
		s.writeProblem(w, r, *problem)
		return
	}

	req, problem := toDomain(body)
	if problem != nil {
		s.writeProblem(w, r, *problem)
		return
	}

	result, validation, err := s.submitter.Submit(r.Context(), req, key, fingerprint)
	switch {
	case errors.Is(err, port.ErrIdempotencyConflict):
		// The key was reused with a different body. A client bug, surfaced
		// rather than swallowed — silently replaying would discard a request
		// the customer believes they submitted.
		log.Warn("idempotency key reused with a different body")
		s.writeProblem(w, r, idempotencyConflict())
		return
	case err != nil && errors.Is(err, app.ErrNotPersisted):
		// The one case where the customer must NOT be told yes.
		log.Error("submission not persisted", slog.Any("error", err))
		s.writeProblem(w, r, unavailable())
		return
	case err != nil:
		log.Error("submission failed", slog.Any("error", err))
		s.writeProblem(w, r, unavailable())
		return
	case !validation.OK():
		log.Info("submission rejected",
			slog.String("reason_code", string(validation.Primary())),
			slog.Int("field_errors", len(validation.Errors)),
		)
		s.writeProblem(w, r, unprocessable(validation))
		return
	}

	log.Info("submission accepted",
		slog.String("request_id", result.RequestID),
		slog.Bool("replayed", result.Replayed),
	)
	w.Header().Set("Location", "/v1/tasking-requests/"+result.RequestID)
	if result.Replayed {
		// So a client can tell a retry from a first submission. Without it,
		// nobody can debug a suspected double charge.
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if err := writeJSON(w, http.StatusAccepted, map[string]any{
		"request_id":   result.RequestID,
		"state":        result.State,
		"submitted_at": result.SubmittedAt.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		log.Error("accepted response failed", slog.Any("error", err))
	}
}

// decodeSubmit parses the body, refusing anything it cannot understand.
func decodeSubmit(raw []byte) (submitBody, *Problem) {
	var body submitBody

	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are an error, not a shrug. A customer who typed
	// "bid_credit" and got a 202 has been told their bid was accepted at zero.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&body); err != nil {
		p := badRequest("request body is not valid JSON for this endpoint: " + err.Error())
		return body, &p
	}
	// Exactly one JSON value. Trailing content means the client sent something
	// other than what it thinks it sent.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		p := badRequest("request body contains more than one JSON value")
		return body, &p
	}
	return body, nil
}

// toDomain converts the wire shape into the domain shape.
//
// Anything structurally impossible is a 400 here. Anything merely wrong is left
// for Validate, which reports every field at once — so this function refuses
// only what it genuinely cannot represent.
func toDomain(b submitBody) (domain.SubmitRequest, *Problem) {
	var out domain.SubmitRequest

	if b.Target == nil {
		p := badRequest("target is required")
		return out, &p
	}
	if b.Window == nil || b.Window.Start == nil || b.Window.End == nil {
		p := badRequest("window with start and end is required")
		return out, &p
	}

	target, err := decodeTarget(*b.Target)
	if err != nil {
		p := badRequest(err.Error())
		return out, &p
	}

	out = domain.SubmitRequest{
		Target:         target,
		WindowStart:    *b.Window.Start,
		WindowEnd:      *b.Window.End,
		RequestedModes: b.RequestedModes,
	}
	if b.CustomerID != nil {
		out.CustomerID = *b.CustomerID
	}
	if b.TargetName != nil {
		out.TargetName = *b.TargetName
	}
	if b.PriorityTier != nil {
		out.PriorityTier = *b.PriorityTier
	}
	if b.BidCredits != nil {
		out.BidCredits = *b.BidCredits
	}
	if b.Constraints != nil {
		out.Constraints = domain.RequestConstraints{
			LookSide:        b.Constraints.LookSide,
			MinIncidenceDeg: b.Constraints.MinIncidenceDeg,
			MaxIncidenceDeg: b.Constraints.MaxIncidenceDeg,
			MaxSquintDeg:    b.Constraints.MaxSquintDeg,
		}
	}
	return out, nil
}

func decodeTarget(g geoJSONGeometry) (domain.Target, error) {
	switch g.Type {
	case string(domain.TargetPoint):
		var pair []float64
		if err := json.Unmarshal(g.Coordinates, &pair); err != nil || len(pair) != 2 {
			return domain.Target{}, errors.New("point coordinates must be [longitude, latitude]")
		}
		// Longitude FIRST. Both bindings silently drop prefixItems, so nothing
		// upstream enforces the order — it is enforced here, once, and the
		// range check in Validate catches the swap when it pushes latitude out
		// of bounds.
		return domain.Target{
			Kind:  domain.TargetPoint,
			Point: domain.Position{Lon: pair[0], Lat: pair[1]},
		}, nil

	case string(domain.TargetPolygon):
		var rings [][][]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil || len(rings) == 0 {
			return domain.Target{}, errors.New("polygon coordinates must be an array of linear rings")
		}
		// Exterior ring only. Holes are a real GeoJSON feature and are not
		// supported here — accepting and ignoring them would image the hole.
		if len(rings) > 1 {
			return domain.Target{}, errors.New("polygon holes are not supported; supply an exterior ring only")
		}
		ring := make([]domain.Position, 0, len(rings[0]))
		for _, pair := range rings[0] {
			if len(pair) != 2 {
				return domain.Target{}, errors.New("each polygon position must be [longitude, latitude]")
			}
			ring = append(ring, domain.Position{Lon: pair[0], Lat: pair[1]})
		}
		return domain.Target{Kind: domain.TargetPolygon, Ring: ring}, nil

	default:
		return domain.Target{}, errors.New("target type must be Point or Polygon, got " + g.Type)
	}
}
