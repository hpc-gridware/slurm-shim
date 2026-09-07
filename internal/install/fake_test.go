package install_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// fakeAdmin is an in-memory cluster: reads answer from its maps, writes mutate
// them and are recorded, so specs can assert both the plan and its effect.
type fakeAdmin struct {
	pes       map[string]gedata.PE
	queues    map[string]gedata.Queue
	complexes []gedata.Complex
	hosts     []string
	calls     []string
	failOn    string // a call prefix that should error, e.g. "AddPE"
}

func newFake() *fakeAdmin {
	return &fakeAdmin{pes: map[string]gedata.PE{}, queues: map[string]gedata.Queue{}}
}

func (f *fakeAdmin) record(s string) error {
	f.calls = append(f.calls, s)
	if f.failOn != "" && len(s) >= len(f.failOn) && s[:len(f.failOn)] == f.failOn {
		return errors.New("injected failure")
	}
	return nil
}

func (f *fakeAdmin) PEs(context.Context) ([]string, error) {
	var out []string
	for n := range f.pes {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeAdmin) PE(_ context.Context, n string) (gedata.PE, error) {
	p, ok := f.pes[n]
	if !ok {
		return gedata.PE{}, fmt.Errorf("no pe %s", n)
	}
	return p, nil
}
func (f *fakeAdmin) AddPE(_ context.Context, p gedata.PE) error {
	if err := f.record("AddPE " + p.Name); err != nil {
		return err
	}
	f.pes[p.Name] = p
	return nil
}
func (f *fakeAdmin) SetPEAttr(_ context.Context, pe, attr, v string) error {
	if err := f.record("SetPEAttr " + pe + " " + attr + "=" + v); err != nil {
		return err
	}
	p := f.pes[pe]
	switch attr {
	case "start_proc_args":
		p.StartProcArgs = v
	case "control_slaves":
		p.ControlSlaves = v == "TRUE"
	}
	f.pes[pe] = p
	return nil
}
func (f *fakeAdmin) Queues(context.Context) ([]string, error) {
	var out []string
	for n := range f.queues {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeAdmin) Queue(_ context.Context, n string) (gedata.Queue, error) {
	q, ok := f.queues[n]
	if !ok {
		return gedata.Queue{}, fmt.Errorf("no queue %s", n)
	}
	return q, nil
}
func (f *fakeAdmin) SetQueueAttr(_ context.Context, q, attr, v string) error {
	if err := f.record("SetQueueAttr " + q + " " + attr + "=" + v); err != nil {
		return err
	}
	qq := f.queues[q]
	if attr == "starter_method" {
		qq.StarterMethod = v
	}
	f.queues[q] = qq
	return nil
}
func (f *fakeAdmin) AddQueueAttr(_ context.Context, q, attr, v string) error {
	if err := f.record("AddQueueAttr " + q + " " + attr + "+=" + v); err != nil {
		return err
	}
	qq := f.queues[q]
	if attr == "pe_list" {
		qq.PEList = append(qq.PEList, v)
	}
	f.queues[q] = qq
	return nil
}
func (f *fakeAdmin) Complexes(context.Context) ([]gedata.Complex, error) { return f.complexes, nil }
func (f *fakeAdmin) ExecHosts(context.Context) ([]string, error)         { return f.hosts, nil }
