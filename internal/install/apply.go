package install

import (
	"context"
	"fmt"
)

// Outcome is what happened to one Change during Apply.
type Outcome struct {
	Change Change
	Err    error // nil when applied or when nothing needed doing
}

// Report is the result of Apply. Changes are independent, so one failure does
// not stop the others; a re-run finishes what failed because the plan is
// recomputed from the cluster's actual state.
type Report struct {
	Outcomes []Outcome
}

// Applied counts the changes that mutated the cluster.
func (r Report) Applied() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Err == nil && mutating(o.Change.Kind) {
			n++
		}
	}
	return n
}

// Failed returns the outcomes with an error.
func (r Report) Failed() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Err != nil {
			out = append(out, o)
		}
	}
	return out
}

func mutating(k ChangeKind) bool {
	switch k {
	case ChangeAddPE, ChangeSetPEAttr, ChangeAddToPEList, ChangeSetStarter,
		ChangeAddComplex, ChangeAddQueue, ChangeRemoveFromPEList, ChangeDeleteQueue,
		ChangeDeletePE, ChangeDeleteComplex:
		return true
	}
	return false
}

// Apply performs the plan's mutating changes. Unchanged rows and refusals are
// skipped and reported as such, so the report lists every row the plan showed.
func Apply(ctx context.Context, a ClusterAdmin, p Plan) Report {
	var r Report
	for _, c := range p.Changes {
		var err error
		switch c.Kind {
		case ChangeAddPE:
			err = a.AddPE(ctx, c.pe)
		case ChangeSetPEAttr:
			err = a.SetPEAttr(ctx, c.Object, c.Attr, orNONE(c.New))
		case ChangeAddToPEList:
			err = a.AddQueueAttr(ctx, c.Object, "pe_list", c.New)
		case ChangeSetStarter:
			err = a.SetQueueAttr(ctx, c.Object, "starter_method", orNONE(c.New))
		case ChangeAddComplex:
			err = a.AddForcedComplex(ctx, c.Object)
		case ChangeAddQueue:
			err = a.CloneQueue(ctx, c.Old, c.Object, c.New)
		case ChangeRemoveFromPEList:
			err = a.RemoveQueueAttr(ctx, c.Object, "pe_list", c.Old)
		case ChangeDeleteQueue:
			err = a.DeleteQueue(ctx, c.Object)
		case ChangeDeletePE:
			err = a.DeletePE(ctx, c.Object)
		case ChangeDeleteComplex:
			err = a.DeleteComplex(ctx, c.Object)
		case ChangeUnchanged, ChangeRefused:
			// nothing to do; reported for completeness
		default:
			err = fmt.Errorf("unknown change kind %q", c.Kind)
		}
		if err != nil {
			err = fmt.Errorf("%s %s: %w", c.Kind, c.Object, err)
		}
		r.Outcomes = append(r.Outcomes, Outcome{Change: c, Err: err})
	}
	return r
}

// orNONE is the value Grid Engine takes for "unset".
func orNONE(v string) string {
	if v == "" {
		return "NONE"
	}
	return v
}
