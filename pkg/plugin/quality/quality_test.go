package quality

import (
	"testing"

	"gta/pkg/plugindev"
)

// highEntropy / lowEntropy 用于构造可预测的熵估计。
var (
	highEntropy = func() []byte {
		b := make([]byte, 256)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}()
	lowEntropy = []byte("AAAAAAAAAAAAAAAA")
)

// validEventIO 构造一个非终结响应（带 event_type/schema_id/payload 非空）。
func validEventIO(id string) DecodeIO {
	return DecodeIO{InputID: id, Done: false, EventType: "game.login", SchemaID: "game.login.v1", PayloadLen: 1}
}

// doneIO 构造一个终结响应，可携带原始字节供熵估计。
func doneIO(id string, payload []byte) DecodeIO {
	return DecodeIO{InputID: id, Done: true, Payload: payload}
}

func TestVerifyPass(t *testing.T) {
	corpus := []DecodeIO{
		validEventIO("p1"), doneIO("p1", lowEntropy),
		validEventIO("p2"), doneIO("p2", lowEntropy),
		validEventIO("p3"), doneIO("p3", lowEntropy),
	}
	res := Verify(corpus)
	if res.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", res.Verdict)
	}
	if len(res.Violations) != 0 {
		t.Fatalf("unexpected violations: %+v", res.Violations)
	}
	q := res.Quality
	if q == nil {
		t.Fatal("quality nil")
	}
	// 统计按输入包（InputID）聚合：3 个包各产生「事件响应 + 终结响应」，
	// 计 3 个 input，而不是 6 条 DecodeIO。
	if q.TotalInputs != 3 {
		t.Errorf("total_inputs = %d, want 3", q.TotalInputs)
	}
	if q.UnknownInputs != 0 {
		t.Errorf("unknown_inputs = %d, want 0", q.UnknownInputs)
	}
	if q.DecodeErrors != 0 {
		t.Errorf("decode_errors = %d, want 0", q.DecodeErrors)
	}
	if q.SchemaVersionedRatio != 1.0 {
		t.Errorf("schema_versioned_ratio = %v, want 1.0", q.SchemaVersionedRatio)
	}
	if q.EntropyEstimate > 1.0 {
		t.Errorf("entropy_estimate = %v, want near 0 (low entropy payload)", q.EntropyEstimate)
	}
}

func TestVerifyFailAllUnknown(t *testing.T) {
	var corpus []DecodeIO
	for i := 0; i < 10; i++ {
		id := "p" + string(rune('a'+i))
		corpus = append(corpus, doneIO(id, lowEntropy)) // decoded but no events
	}
	res := Verify(corpus)
	if res.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", res.Verdict)
	}
	q := res.Quality
	if q.UnknownInputs != 10 || q.UnknownRatio != 1.0 {
		t.Fatalf("unknown = %d / %v, want 10 / 1.0", q.UnknownInputs, q.UnknownRatio)
	}
}

func TestVerifyFailDecodeErrors(t *testing.T) {
	corpus := []DecodeIO{
		{InputID: "p1", Done: true, DecodeError: "boom", Payload: lowEntropy},
		{InputID: "p2", Done: true, DecodeError: "bang", Payload: lowEntropy},
		{InputID: "p3", Done: true, DecodeError: "crash", Payload: lowEntropy},
	}
	res := Verify(corpus)
	if res.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", res.Verdict)
	}
	if res.Quality.DecodeErrors != 3 {
		t.Errorf("decode_errors = %d, want 3", res.Quality.DecodeErrors)
	}
}

func TestVerifyViolationPayloadNonEmpty(t *testing.T) {
	// 一个非终结响应带 event_type 但 payload 为空 -> payload-non-empty 违规。
	corpus := []DecodeIO{
		{InputID: "p1", Done: false, EventType: "game.login", SchemaID: ""},
		doneIO("p1", lowEntropy),
	}
	res := Verify(corpus)
	if len(res.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(res.Violations))
	}
	v := res.Violations[0]
	if v.RuleID != "payload-non-empty" {
		t.Errorf("rule_id = %q, want payload-non-empty", v.RuleID)
	}
	if v.Severity != "error" {
		t.Errorf("severity = %q, want error", v.Severity)
	}
	if v.Count != 1 {
		t.Errorf("count = %d, want 1", v.Count)
	}
	if v.Topic != "encoding" {
		t.Errorf("topic = %q, want encoding", v.Topic)
	}
	// error 级违规 -> 整体 fail。
	if res.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", res.Verdict)
	}
}

func TestVerifyWarnEncryption(t *testing.T) {
	// 3 个全未知（高熵）+ 1 个有效（高熵）：unknown_ratio 0.75 落在 [0.5,0.95)，
	// 熵估计接近上限 -> 疑似加密，verdict warn（未到全未知 fail 阈值）。
	corpus := []DecodeIO{
		doneIO("p1", highEntropy),
		doneIO("p2", highEntropy),
		doneIO("p3", highEntropy),
		validEventIO("p4"), doneIO("p4", highEntropy),
	}
	res := Verify(corpus)
	if res.Verdict != "warn" {
		t.Fatalf("verdict = %q, want warn", res.Verdict)
	}
	q := res.Quality
	if q.UnknownRatio < 0.5 || q.UnknownRatio >= 0.95 {
		t.Errorf("unknown_ratio = %v, want in [0.5,0.95)", q.UnknownRatio)
	}
	if q.EntropyEstimate < 7.5 {
		t.Errorf("entropy_estimate = %v, want >= 7.5", q.EntropyEstimate)
	}
}

func TestVerifyWarnLowVersioning(t *testing.T) {
	// 全部有效事件（payload 非空，不触发 error 级违规）但 schema 未带版本后缀
	// -> schema-id-versioned 味道不足 -> warn。
	corpus := []DecodeIO{
		{InputID: "p1", Done: false, EventType: "game.login", SchemaID: "game.login", PayloadLen: 1},
		doneIO("p1", lowEntropy),
		{InputID: "p2", Done: false, EventType: "game.logout", SchemaID: "game.logout", PayloadLen: 1},
		doneIO("p2", lowEntropy),
	}
	res := Verify(corpus)
	if res.Verdict != "warn" {
		t.Fatalf("verdict = %q, want warn", res.Verdict)
	}
	if res.Quality.SchemaVersionedRatio != 0 {
		t.Errorf("schema_versioned_ratio = %v, want 0", res.Quality.SchemaVersionedRatio)
	}
}

func TestVerifyReassemblyQuality(t *testing.T) {
	// 5 个包都解出事件但无一关联 -> CorrelatedInputs=0（explain 会据此归因缺流重组）。
	corpus := []DecodeIO{}
	for i := 0; i < 5; i++ {
		id := "p" + string(rune('a'+i))
		corpus = append(corpus, validEventIO(id), doneIO(id, lowEntropy))
	}
	res := Verify(corpus)
	q := res.Quality
	if q.CorrelatedInputs != 0 {
		t.Errorf("correlated_inputs = %d, want 0", q.CorrelatedInputs)
	}
	if q.TotalInputs != 5 {
		t.Errorf("total_inputs = %d, want 5 (per input packet, not per DecodeIO)", q.TotalInputs)
	}
}

func TestVerifyEmptyCorpus(t *testing.T) {
	res := Verify(nil)
	if res.Verdict != "warn" {
		t.Fatalf("verdict = %q, want warn (empty corpus is not a pass)", res.Verdict)
	}
	if res.Quality == nil {
		t.Fatal("quality nil")
	}
}

// 确保 plugindev 阈值常量可被本包正确使用（编译期引用）。
func TestThresholdsReferenced(t *testing.T) {
	if plugindev.AllUnknownRatioThreshold <= 0 || plugindev.EncryptionUnknownRatioThreshold <= 0 || plugindev.HighEntropyThreshold <= 0 {
		t.Fatal("thresholds must be positive")
	}
}
