package install

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// CloneWarnings reports what a new DefaultQueueName will not inherit from the
// queue it is cloned from (src, "" for none), so the admin sees it before
// --apply. It runs only when the plan creates the queue, and never fails the
// install: a check it cannot make is reported as such. insts are the queue
// instances (qstat -f), which name src's hosts and their states.
//
// The new queue copies src's configuration, not its runtime state or the
// objects that refer to src:
//   - hosts src has disabled or in error get enabled instances of the new queue;
//   - hosts without a slots limit (exechost complex_values, or a resource quota
//     limiting slots per host) can run jobs of both queues beyond their cores;
//   - a queue that suspends src (subordinate_list) does not suspend the new one;
//   - a resource quota naming src does not limit the new one.
func CloneWarnings(ctx context.Context, a ClusterAdmin, f Facts, src string, insts []gedata.QueueInstance) []string {
	var warns []string
	if src == "" {
		return []string{"no all.q to clone: " + DefaultQueueName + " gets @allhosts and the scheduler's queue " +
			"defaults (1 slot per host); set its slots after install (qconf -mq " + DefaultQueueName + ")"}
	}

	var hosts, closed []string
	for _, qi := range insts {
		if qi.Queue != src {
			continue
		}
		hosts = append(hosts, qi.Host)
		if strings.ContainsAny(qi.States, "dDE") {
			closed = append(closed, qi.Host+" ("+qi.States+")")
		}
	}
	if len(closed) > 0 {
		warns = append(warns, fmt.Sprintf("%s instance(s) disabled or in error: %s -- %s gets enabled instances "+
			"there; disable them too: qmod -d %s@<host>", src, listHosts(closed), DefaultQueueName, DefaultQueueName))
	}

	rqs, rqsErr := a.ResourceQuotaSets(ctx)
	if rqsErr != nil {
		warns = append(warns, "could not read resource quotas ("+rqsErr.Error()+"); check by hand that none limits "+
			src+" alone")
	}
	if !gedata.RQSLimitsHostSlots(rqs) {
		var unlimited, unread []string
		for _, h := range hosts {
			limited, err := a.HostSlotsLimited(ctx, h)
			switch {
			case err != nil:
				unread = append(unread, h)
			case !limited:
				unlimited = append(unlimited, h)
			}
		}
		if len(unlimited) > 0 {
			warns = append(warns, fmt.Sprintf("%d exec host(s) without a slots limit: %s -- jobs in %s and %s can "+
				"together exceed their cores; set it per host: qconf -mattr exechost complex_values "+
				"slots=<cores> <host>", len(unlimited), listHosts(unlimited), DefaultQueueName, src))
		}
		if len(unread) > 0 {
			warns = append(warns, "could not read the slots limit of "+listHosts(unread))
		}
	}

	for _, q := range f.Queues {
		for _, sub := range q.Subordinates {
			if name, _, _ := strings.Cut(sub, "="); name == src {
				warns = append(warns, "queue "+q.Name+" suspends "+src+" (subordinate_list) but not "+
					DefaultQueueName+"; add it there if shim jobs should yield too")
			}
		}
	}
	for _, r := range rqs {
		if r.Enabled && slices.ContainsFunc(r.Limits, func(l string) bool { return namesQueue(l, src) }) {
			warns = append(warns, "resource quota "+r.Name+" names "+src+" and does not limit "+
				DefaultQueueName+"; add it there if the limit should cover shim jobs (qconf -mrqs "+r.Name+")")
		}
	}
	return warns
}

// namesQueue reports whether a resource quota rule names queue.
func namesQueue(rule, queue string) bool {
	return slices.Contains(strings.FieldsFunc(rule, func(r rune) bool {
		return r == ' ' || r == '{' || r == '}' || r == ',' || r == '!'
	}), queue)
}

// listHosts names the first few hosts and counts the rest.
func listHosts(hosts []string) string {
	named := hosts
	if len(named) > 5 {
		named = named[:5]
	}
	list := strings.Join(named, " ")
	if len(hosts) > len(named) {
		list += fmt.Sprintf(" (and %d more)", len(hosts)-len(named))
	}
	return list
}
