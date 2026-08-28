package gedata

import (
	"context"
	"fmt"
	"strings"

	// go-clusterscheduler is the shared source of truth for OCS/GE command
	// formats; gedata is the shim's single boundary to it (callers use the
	// QueueInstance type below, never the library's). v9.1 is safe for both
	// supported releases: v9.1.FullQueueInfo is a type alias for the v9.0 struct
	// and the qstat -f columns it reads (queue@host, used/total, states) are the
	// stable classic ones, identical on OCS 9.0.10 through 9.1.5.
	qstat "github.com/hpc-gridware/go-clusterscheduler/pkg/qstat/v9.1"
)

// QueueInstance is one GE cluster-queue instance (queue@host) from qstat -f.
// States holds the raw GE state letters (e.g. "au", "d", "E"), empty when
// healthy.
type QueueInstance struct {
	Name   string // "all.q@ocs-master"
	Queue  string // "all.q"
	Host   string // "ocs-master"
	Used   int
	Total  int
	States string
}

// QueueInstances runs `qstat -f` through the runner and returns the parsed
// cluster-queue instances. Parsing is delegated to go-clusterscheduler; this
// function only adapts the result into the shim's QueueInstance type.
func QueueInstances(ctx context.Context, runner Runner) ([]QueueInstance, error) {
	out, errOut, exit, err := runner.Run(ctx, "qstat", "-f")
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat exited %d: %s", exit, strings.TrimSpace(string(errOut)))
	}
	full, err := qstat.ParseQstatFullOutput(string(out))
	if err != nil {
		return nil, err
	}
	instances := make([]QueueInstance, 0, len(full))
	for _, q := range full {
		queue, host, ok := strings.Cut(q.QueueName, "@")
		if !ok {
			continue // not a queue@host instance line
		}
		instances = append(instances, QueueInstance{
			Name:   q.QueueName,
			Queue:  queue,
			Host:   host,
			Used:   q.Used,
			Total:  q.Total,
			States: q.States,
		})
	}
	return instances, nil
}
