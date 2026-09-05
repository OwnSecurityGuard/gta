// Package quality implements plugin.verify's statistical half (design §5).
//
// The SDK's contract checker owns single-message protocol self-consistency
// (input_id echo, done lifecycle, payload non-empty) — it can run offline in a
// plugin's own unit tests. This package owns the batch statistical quality:
// given a whole decode corpus it produces the gt-side QualityStats and merges
// them with the SDK violations into a single plugindev.VerifyResult + verdict.
//
// The split follows the design's dividing line: "can a plugin author run it
// offline with only the SDK?" — yes for the checker, no for the corpus-level
// statistics, so the latter lives here in gametrace.
package quality

import (
	"math"
	"regexp"

	sdkcontract "github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	sdkpb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"

	"gametrace/pkg/plugindev"
)

// DecodeIO is one decode input/output pair reduced to the fields the verify
// pass needs. It is deliberately transport-agnostic: the Runtime Plane builds
// it from a real session's decode results, or a test harness builds it by hand.
// plugin.verify merges these into a plugindev.VerifyResult.
type DecodeIO struct {
	InputID     string // echoed input_id (raw_packet_id in practice)
	Done        bool   // final response flag
	EventType   string // first emitted event_type, "" if none
	SchemaID    string // first emitted schema_id, "" if none
	PayloadLen  int    // len of the emitted payload (non-empty check)
	DecodeError string // non-empty => the response carried a decode error
	Correlated  bool   // any emitted event carried a correlation_key
	Payload     []byte // undecoded bytes, only for the entropy estimate
}

// versionSuffix matches a ".vN" schema version suffix, e.g. game.login.v1.
var versionSuffix = regexp.MustCompile(`\.[vV]\d+$`)

// packetAgg folds every DecodeIO belonging to one input packet into a single
// verdict-relevant record. A packet normally produces N event responses plus a
// terminating done-response; counting those separately would report the empty
// done-response as an "unknown" packet and halve the versioned-schema ratio.
type packetAgg struct {
	decodeErr  bool     // any response for this packet carried a decode error
	hasEvent   bool     // any response emitted an event_type/schema_id
	correlated bool     // any emitted event carried a correlation key
	versioned  bool     // any emitted schema_id had a .vN suffix
	payloads   [][]byte // raw bytes seen, for the entropy estimate
}

// Verify runs the SDK contract checker over every DecodeIO and merges the
// results with gt-side statistical quality into a single VerifyResult.
func Verify(corpus []DecodeIO) *plugindev.VerifyResult {
	res := &plugindev.VerifyResult{Verdict: "pass", Quality: &plugindev.QualityStats{}}
	if len(corpus) == 0 {
		// Nothing to verify is not a pass: the AI supplied an empty corpus.
		res.Verdict = "warn"
		return res
	}

	// 1) SDK contract violations — single-message protocol self-consistency.
	viol := map[string]*plugindev.Violation{}
	var order []string
	for _, io := range corpus {
		req := &sdkpb.DecodeRequest{InputId: io.InputID}
		resp := &sdkpb.DecodeResponseV2{
			InputId:        io.InputID,
			Done:           io.Done,
			EventType:      io.EventType,
			SchemaId:       io.SchemaID,
			PayloadMsgpack: make([]byte, io.PayloadLen),
		}
		if err := sdkcontract.CheckDecodeResponse(req, resp); err != nil {
			v, ok := err.(sdkcontract.Violation)
			if !ok {
				continue
			}
			if _, seen := viol[v.RuleID]; !seen {
				viol[v.RuleID] = &plugindev.Violation{RuleID: v.RuleID}
				order = append(order, v.RuleID)
			}
			e := viol[v.RuleID]
			e.Count++
			if e.Sample == "" {
				e.Sample = v.Message
			}
			if spec, ok := sdkcontract.Default().RuleByID(v.RuleID); ok {
				e.Topic = spec.Topic
				e.Severity = string(spec.Severity)
				e.Statement = spec.Statement
				e.DocRef = spec.DocRef
			}
		}
	}
	for _, id := range order {
		res.Violations = append(res.Violations, viol[id])
	}

	// 2) gt-side quality statistics, aggregated per packet (InputID) so that a
	//    packet's terminating done-response is not double-counted as "unknown".
	q := res.Quality
	agg := map[string]*packetAgg{}
	var addPacket func(io DecodeIO)
	addPacket = func(io DecodeIO) {
		a := agg[io.InputID]
		if a == nil {
			a = &packetAgg{}
			agg[io.InputID] = a
		}
		if io.DecodeError != "" {
			a.decodeErr = true
		}
		if io.EventType != "" || io.SchemaID != "" {
			a.hasEvent = true
		}
		if io.Correlated {
			a.correlated = true
		}
		if versionSuffix.MatchString(io.SchemaID) {
			a.versioned = true
		}
		if len(io.Payload) > 0 {
			a.payloads = append(a.payloads, io.Payload)
		}
	}
	for _, io := range corpus {
		addPacket(io)
	}

	q.TotalInputs = len(agg)
	var unknown, correlated, decodeErrors, versioned int
	var entropySum float64
	entropyN := 0
	for _, a := range agg {
		if a.decodeErr {
			decodeErrors++
			continue
		}
		if !a.hasEvent {
			unknown++
		}
		if a.correlated {
			correlated++
		}
		if a.versioned {
			versioned++
		}
		for _, p := range a.payloads {
			entropySum += shannonBits(p)
			entropyN++
		}
	}
	q.UnknownInputs = unknown
	q.CorrelatedInputs = correlated
	q.DecodeErrors = decodeErrors
	if q.TotalInputs > 0 {
		q.UnknownRatio = float64(unknown) / float64(q.TotalInputs)
		withSchema := q.TotalInputs - decodeErrors
		if withSchema > 0 {
			q.SchemaVersionedRatio = float64(versioned) / float64(withSchema)
		}
	}
	if entropyN > 0 {
		q.EntropyEstimate = entropySum / float64(entropyN)
	}

	// 3) verdict.
	res.Verdict = verdict(res, q)
	return res
}

// RecomputeVerdict re-evaluates the verdict of a VerifyResult after the caller
// appended extra violations (e.g. semantic-contract CheckEvent results) to it.
// Callers must have finished mutating res.Violations / res.Quality beforehand.
func RecomputeVerdict(res *plugindev.VerifyResult) {
	if res == nil {
		return
	}
	if res.Quality == nil {
		res.Quality = &plugindev.QualityStats{}
	}
	res.Verdict = verdict(res, res.Quality)
}

// verdict merges SDK violations with statistical signals into pass|warn|fail.
func verdict(res *plugindev.VerifyResult, q *plugindev.QualityStats) string {
	hasErr, hasWarn := false, false
	for _, v := range res.Violations {
		switch v.Severity {
		case string(sdkcontract.SeverityError):
			hasErr = true
		case string(sdkcontract.SeverityWarn):
			hasWarn = true
		}
	}
	allErrored := q.TotalInputs > 0 && q.DecodeErrors == q.TotalInputs
	allUnknown := q.UnknownRatio >= plugindev.AllUnknownRatioThreshold
	// High entropy + majority undecodable looks encrypted/compressed.
	suspectEnc := q.UnknownRatio >= plugindev.EncryptionUnknownRatioThreshold &&
		q.EntropyEstimate >= plugindev.HighEntropyThreshold
	// Low schema versioning is a quality smell (schema-id-versioned rule).
	lowVersioning := q.TotalInputs > 0 && q.SchemaVersionedRatio < 0.5

	if hasErr || allErrored || allUnknown {
		return "fail"
	}
	if hasWarn || suspectEnc || lowVersioning {
		return "warn"
	}
	return "pass"
}

// shannonBits returns the Shannon entropy of b in bits/byte (0..8).
func shannonBits(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var freq [256]float64
	for _, c := range b {
		freq[c]++
	}
	var h float64
	n := float64(len(b))
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := f / n
		h -= p * math.Log2(p)
	}
	return h
}
