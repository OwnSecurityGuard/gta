package analyze

import (
	"time"

	"github.com/expr-lang/expr/vm"
	"gta/pkg/event"
)

type CountAgg struct {
	window       time.Duration
	output       string
	groupBy      []*vm.Program
	groupByNames []string
	state        map[string]int64
	groups       map[string]map[string]string
	wstart       time.Time
	ready        []event.Metric
}

func NewCountAgg(window time.Duration, output string) *CountAgg {
	return &CountAgg{
		window: window,
		output: output,
		state:  map[string]int64{},
		groups: map[string]map[string]string{},
		wstart: time.Now().Truncate(window),
	}
}

func (a *CountAgg) WithGroupBy(progs []*vm.Program) *CountAgg { a.groupBy = progs; return a }
func (a *CountAgg) WithGroupByNames(names []string) *CountAgg { a.groupByNames = names; return a }

func (a *CountAgg) Add(ev event.TimestampedEvent, ctx map[string]any) error {
	gr, err := groupKey(a.groupBy, a.groupByNames, ctx)
	if err != nil {
		return err
	}
	ws := windowStart(ev.GetTimestamp(), a.window)
	if !ws.Equal(a.wstart) {
		a.emit(a.wstart)
		a.wstart = ws
		a.state = map[string]int64{}
		a.groups = map[string]map[string]string{}
	}
	a.state[gr.key]++
	a.groups[gr.key] = gr.group
	return nil
}

func (a *CountAgg) emit(wstart time.Time) {
	for k, v := range a.state {
		a.ready = append(a.ready, event.Metric{Name: a.output, Window: wstart, Group: a.groups[k], Value: float64(v)})
	}
}

// Flush 只输出已关闭窗口的指标，不清空当前窗口。
func (a *CountAgg) Flush() []event.Metric {
	out := make([]event.Metric, len(a.ready))
	copy(out, a.ready)
	a.ready = nil
	return out
}

// FinalFlush 输出已关闭窗口 + 当前窗口，并清空状态。
func (a *CountAgg) FinalFlush() []event.Metric {
	out := make([]event.Metric, 0, len(a.ready)+len(a.state))
	out = append(out, a.ready...)
	a.ready = nil
	for k, v := range a.state {
		out = append(out, event.Metric{Name: a.output, Window: a.wstart, Group: a.groups[k], Value: float64(v)})
	}
	a.state = map[string]int64{}
	a.groups = map[string]map[string]string{}
	return out
}

func (a *CountAgg) Window() time.Duration { return a.window }
