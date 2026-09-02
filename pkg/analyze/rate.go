package analyze

import (
	"time"

	"gta/pkg/event"

	"github.com/expr-lang/expr/vm"
)

type RateAgg struct {
	countAgg *CountAgg
}

func NewRateAgg(window time.Duration, output string) *RateAgg {
	return &RateAgg{countAgg: NewCountAgg(window, output)}
}

func (a *RateAgg) WithGroupBy(progs []*vm.Program) *RateAgg {
	a.countAgg.WithGroupBy(progs)
	return a
}
func (a *RateAgg) WithGroupByNames(names []string) *RateAgg {
	a.countAgg.WithGroupByNames(names)
	return a
}

func (a *RateAgg) Add(ev event.TimestampedEvent, ctx map[string]any) error {
	return a.countAgg.Add(ev, ctx)
}

func (a *RateAgg) Flush() []event.Metric { return a.finalize(a.countAgg.Flush()) }

func (a *RateAgg) FinalFlush() []event.Metric { return a.finalize(a.countAgg.FinalFlush()) }

func (a *RateAgg) finalize(metrics []event.Metric) []event.Metric {
	secs := a.countAgg.Window().Seconds()
	for i := range metrics {
		metrics[i].Value = metrics[i].Value / secs
	}
	return metrics
}

func (a *RateAgg) Window() time.Duration { return a.countAgg.Window() }
