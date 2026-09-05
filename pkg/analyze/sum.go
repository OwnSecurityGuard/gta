package analyze

import (
	"time"

	"github.com/expr-lang/expr/vm"
	"gametrace/pkg/event"
)

type SumAgg struct {
	window       time.Duration
	output       string
	groupBy      []*vm.Program
	groupByNames []string
	value        *vm.Program
	state        map[string]float64
	groups       map[string]map[string]string
	wstart       time.Time
	ready        []event.Metric
}

func NewSumAgg(window time.Duration, output string) *SumAgg {
	return &SumAgg{
		window: window,
		output: output,
		state:  map[string]float64{},
		groups: map[string]map[string]string{},
		wstart: time.Now().Truncate(window),
	}
}

func (a *SumAgg) WithGroupBy(progs []*vm.Program) *SumAgg { a.groupBy = progs; return a }
func (a *SumAgg) WithGroupByNames(names []string) *SumAgg { a.groupByNames = names; return a }
func (a *SumAgg) WithValue(prog *vm.Program) *SumAgg      { a.value = prog; return a }

func (a *SumAgg) Add(ev event.TimestampedEvent, ctx map[string]any) error {
	gr, err := groupKey(a.groupBy, a.groupByNames, ctx)
	if err != nil {
		return err
	}
	v, err := evalFloat(a.value, ctx)
	if err != nil {
		return err
	}
	ws := windowStart(ev.GetTimestamp(), a.window)
	if !ws.Equal(a.wstart) {
		a.emit(a.wstart)
		a.wstart = ws
		a.state = map[string]float64{}
		a.groups = map[string]map[string]string{}
	}
	a.state[gr.key] += v
	a.groups[gr.key] = gr.group
	return nil
}

func (a *SumAgg) emit(wstart time.Time) {
	for k, v := range a.state {
		a.ready = append(a.ready, event.Metric{Name: a.output, Window: wstart, Group: a.groups[k], Value: v})
	}
}

func (a *SumAgg) Flush() []event.Metric {
	out := make([]event.Metric, len(a.ready))
	copy(out, a.ready)
	a.ready = nil
	return out
}

func (a *SumAgg) FinalFlush() []event.Metric {
	out := make([]event.Metric, 0, len(a.ready)+len(a.state))
	out = append(out, a.ready...)
	a.ready = nil
	for k, v := range a.state {
		out = append(out, event.Metric{Name: a.output, Window: a.wstart, Group: a.groups[k], Value: v})
	}
	a.state = map[string]float64{}
	a.groups = map[string]map[string]string{}
	return out
}

func (a *SumAgg) Window() time.Duration { return a.window }
