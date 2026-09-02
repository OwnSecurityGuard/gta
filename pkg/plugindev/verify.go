package plugindev

// VerifyResult is the output of plugin.verify (P4). It carries the SDK contract
// violations (each tagged with a contract.yaml rule_id), the gta-side quality
// statistics computed over the real decode corpus, and an overall verdict.
//
// plugin.explain (P3b) consumes a VerifyResult to attribute decode-class
// failures — the verdict alone ("fail") is useless to the AI; what it needs is
// "all inputs came back unknown" / "framing is wrong" / "looks encrypted" /
// "looks like reassembly is missing". Those four patterns are exactly what
// explainVerify reads off this struct.
type VerifyResult struct {
	// Violations are SDK checker results (single-message protocol
	// self-consistency), each referencing a contract.yaml rule_id.
	Violations []*Violation
	// Quality is the gta-side statistical view over the whole corpus (good-vs-bad
	// judgement, which needs real traffic and therefore cannot run offline in
	// the plugin module — design §5).
	Quality *QualityStats
	// Verdict is one of "pass" | "warn" | "fail".
	Verdict string
}

// Violation is a single SDK contract rule that the verify corpus tripped.
type Violation struct {
	RuleID    string
	Topic     string
	Severity  string
	Statement string
	DocRef    string
	Count     int
	Sample    string
}

// QualityStats holds the corpus-level signals explainVerify classifies. Every
// field is optional on the wire; a nil QualityStats simply means no statistical
// evidence is available (explain then falls back to violations only).
type QualityStats struct {
	// TotalInputs is the number of DecodeRequests the corpus exercised.
	TotalInputs int
	// UnknownInputs is how many of those produced no event (the decoder emitted
	// unknown / dropped them). The ratio of the two drives the all-unknown
	// finding.
	UnknownInputs int
	// UnknownRatio is UnknownInputs/TotalInputs, cached for convenience. When
	// set (>=0) it is trusted over recomputing from the integer counts.
	UnknownRatio float64
	// CorrelatedInputs counts inputs that carried a correlation_key (or were
	// tied to a prior input via causation). Zero with many inputs suggests
	// missing stream reassembly.
	CorrelatedInputs int
	// LongPacketErrors counts decode_errors that concentrated on long packets —
	// a hint that message boundaries were guessed wrong (framing / reassembly).
	LongPacketErrors int
	// EntropyEstimate is the mean Shannon entropy of payloads in bits/byte
	// (0..8). High values with high unknown ratio suggest encryption/compression.
	EntropyEstimate float64
	// SchemaVersionedRatio is the ratio of emitted schema_ids that carry a
	// version suffix (e.g. game.login.v1). Low values trip schema-id-versioned.
	SchemaVersionedRatio float64
	// DecodeErrors is the total number of decode errors across the corpus.
	DecodeErrors int
}

// Decode-attribution thresholds (P3b). Centralised so tests and future tuning
// share one source of truth. Exported because pkg/plugin/quality (P4) reuses
// them when computing the verdict.
const (
	// AllUnknownRatioThreshold: at/above this share of undecodable inputs the
	// decoder is treated as producing all-unknown output.
	AllUnknownRatioThreshold = 0.95
	// EncryptionUnknownRatioThreshold: combined with high entropy, this share
	// of undecodable inputs triggers the suspected-encryption finding.
	EncryptionUnknownRatioThreshold = 0.5
	// HighEntropyThreshold bits/byte: payload entropy at/above this, together
	// with a majority of undecodable inputs, looks encrypted/compressed.
	HighEntropyThreshold = 7.5
)
