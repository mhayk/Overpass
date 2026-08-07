package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

// ProblemBase is the URI namespace clients branch on.
//
// A stable URI, never the title. Titles get reworded; a client keyed on one
// breaks silently the day someone improves the wording.
const ProblemBase = "https://overpass.dev/problems/"

// Problem is RFC 9457 Problem Details, with the two Overpass extensions the
// contract declares. Chosen over a bespoke envelope because inventing a third
// error shape for the third service is how APIs become inconsistent.
type Problem struct {
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	Status        int            `json:"status"`
	Detail        string         `json:"detail,omitempty"`
	ReasonCode    string         `json:"reason_code,omitempty"`
	Errors        []ProblemField `json:"errors,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
}

// ProblemField locates one failure by JSON Pointer.
type ProblemField struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// writeProblem renders a Problem with the right content type.
//
// application/problem+json, not application/json. A client that content-negotiates
// on it deserves to get it, and it is one line.
func (s *Server) writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	p.CorrelationID = CorrelationID(r.Context())
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	if err := encode(w, p); err != nil {
		LoggerFrom(r.Context(), s.log).Error("problem response failed", slog.Any("error", err))
	}
}

// badRequest is for a body that could not be understood at all.
//
// 400 versus 422 is the distinction the contract asks for, and it is about
// whether the SHAPE parsed. Malformed JSON, a string where a number belongs, a
// coordinate array that is not an array — the server cannot even begin, so 400.
// A well-formed body whose values are wrong is 422: understood, and refused.
func badRequest(detail string) Problem {
	return Problem{
		Type:       ProblemBase + "malformed-request",
		Title:      "Malformed request",
		Status:     http.StatusBadRequest,
		Detail:     detail,
		ReasonCode: string(domain.ReasonValidationFailed),
	}
}

// unprocessable renders a validation result.
func unprocessable(result domain.ValidationResult) Problem {
	fields := make([]ProblemField, 0, len(result.Errors))
	for _, e := range result.Errors {
		fields = append(fields, ProblemField{
			Pointer: e.Pointer,
			Code:    string(e.Code),
			Message: e.Message,
		})
	}
	return Problem{
		Type:       ProblemBase + "validation-failed",
		Title:      "Request failed validation",
		Status:     http.StatusUnprocessableEntity,
		Detail:     "The request was understood but cannot be accepted; see errors for each field.",
		ReasonCode: string(result.Primary()),
		Errors:     fields,
	}
}

// unavailable is returned when a VALID request could not be stored.
//
// Never 202 for a request we failed to persist. Acknowledging a dropped request
// is unrecoverable business damage — the customer believes an image is coming
// and nothing in the system knows about it. A 503 the client retries is a
// nuisance, and it is recoverable.
func unavailable() Problem {
	return Problem{
		Type:   ProblemBase + "not-persisted",
		Title:  "Request could not be stored",
		Status: http.StatusServiceUnavailable,
		Detail: "The request was valid but could not be durably stored. It has NOT been accepted; retry.",
	}
}
