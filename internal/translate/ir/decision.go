package ir

import "encoding/json"

// Decision models: typed questions scored against shared evidence in one
// non-autoregressive pass — System One / Jev (Ollama's POST /v1/systemone)
// and the OpenAI Decisions API (POST /v1/decisions). This is a non-chat
// operation: no messages, no streaming, no tools, input-token-only billing.
// It gets its own request/response pair beside Request/Response rather than
// stretching the chat IR; both protocol surfaces translate through this hub
// (ADR-004's codec-pair rule applied to decisions).

// DecisionRequest is one decision operation: a state (the shared evidence)
// plus the typed questions.
type DecisionRequest struct {
	// Model is the routing identity; the chain carries the wire name.
	Model string

	// State is the shared evidence. A plain string rides as-is; a JSON
	// object or array rides as its serialized JSON text (the systemone
	// `state` contract: "A nonempty string, or an object or array
	// serialized as JSON text").
	State string

	// Images attach to every question, in request order. The wire
	// encodings differ (bare base64 on systemone, data URLs on the OpenAI
	// Decisions API) — the codecs own that.
	Images []DecisionImage

	// Questions are scored independently: answers are not fed into later
	// questions (both references say so explicitly).
	Questions []DecisionQuestion

	// KeepAlive mirrors systemone's keep_alive (a duration string or
	// seconds as a number), kept as raw JSON. Surfaces without the
	// concept drop it.
	KeepAlive json.RawMessage
}

// DecisionImage is one image input: base64 data plus its media type when
// known. The type is carried out of surfaces that say (a data URL) and
// sniffed from the bytes when a surface did not (systemone's bare base64) —
// a translation onto a surface that requires a media type never guesses
// when the signature is recognizable, and fails clearly when it is not.
type DecisionImage struct {
	MediaType string // e.g. "image/png"; "" when unknown
	Data      string // base64, no data-URL prefix
}

// DecisionQuestionType names the question shapes the two references share:
// systemone's choice/noul/score and the OpenAI Decisions API's
// choice/predicate/score.
type DecisionQuestionType string

const (
	// DecisionChoice picks one of the supplied options.
	DecisionChoice DecisionQuestionType = "choice"
	// DecisionScore rates on an ordered scale, lowest first.
	DecisionScore DecisionQuestionType = "score"
	// DecisionPredicate answers the probability that a condition is true
	// (systemone's `noul`).
	DecisionPredicate DecisionQuestionType = "predicate"
	// DecisionRefusal marks an answer the upstream declined to give (the
	// OpenAI Decisions API's `refusal` type). It has no systemone
	// equivalent — a refusal never gets invented into an answer.
	DecisionRefusal DecisionQuestionType = "refusal"
)

// DecisionQuestion is one typed question.
type DecisionQuestion struct {
	// Name keys the answer; both references require it.
	Name string

	Type DecisionQuestionType

	// Instructions is the question text.
	Instructions string

	// Choices are the choice options in order — option order is payload,
	// not cosmetics: ties follow the model's option order (systemone
	// reference).
	Choices []DecisionOption

	// Levels are the score scale entries, lowest first (score only).
	Levels []DecisionLevel
}

// DecisionOption is one choice option: the value the model answers with,
// plus the description it reasons over.
type DecisionOption struct {
	Value       string
	Description string
}

// DecisionLevel is one score scale entry, lowest first.
type DecisionLevel struct {
	Label       string
	Description string
}

// DecisionResponse is one decoded answer set.
type DecisionResponse struct {
	// Model echoes the upstream's own model name.
	Model string

	// Answers carry one entry per question, in the order they decode.
	Answers []DecisionAnswer

	// Usage is nil when the upstream reported none: systemone always
	// reports input+output tokens; the OpenAI Decisions reference
	// documents no usage object, so its codec parses one when present and
	// stays nil otherwise — the surfaces render what is known, never a
	// plausible number.
	Usage *Usage
}

// DecisionAnswer is one question's answer.
type DecisionAnswer struct {
	Name string

	// Type is the question's type, or DecisionRefusal.
	Type DecisionQuestionType

	// Choice is the winning option value (choice).
	Choice string

	// Probabilities are the per-candidate probabilities in candidate
	// order (choice and score; empty for predicate and refusal). Value is
	// the choice value, or the zero-based level index rendered as a
	// string for score; Label is the level label (score only).
	Probabilities []DecisionProbability

	// Score is the probability-weighted mean of the level indices — it
	// can fall between levels (score).
	Score float64

	// Probability is the [0,1] estimate that the condition is true
	// (predicate; systemone's `noul` — "a number, not a Boolean").
	Probability float64

	// Confidence is the distribution concentration, 1 - H(p) / ln(N) —
	// "not calibrated correctness" (both references).
	Confidence float64
}

// DecisionProbability is one candidate's probability.
type DecisionProbability struct {
	// Value identifies the candidate: the choice value, or the zero-based
	// level index rendered as a string for score.
	Value string

	// Label is the level label (score only; "" otherwise).
	Label string

	Probability float64
}
