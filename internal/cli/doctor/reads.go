package doctor

import (
	"context"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// clusterReads reads each cluster object at most once per doctor run. Several
// checks need the same queue, exec host or configuration; asked separately,
// they cost a qconf fork each, and the per-host ones grow with the cluster
// (thousands of forks against qmaster at 1000 hosts, for a read-only report).
//
// It lives for one run only: every finding is exactly as fresh as when each
// check read the cluster itself. Errors are kept too, so an object that could
// not be read is not asked for again.
type clusterReads struct {
	ctx   context.Context
	admin *gedata.Admin

	queues    map[string]queueRead
	execHosts map[string]execHostRead

	global     *confRead
	localHosts map[string]bool // hosts with a local configuration
	localErr   error
	listed     bool
	locals     map[string]confRead
}

type queueRead struct {
	queue gedata.Queue
	err   error
}

type execHostRead struct {
	host gedata.ExecHost
	err  error
}

type confRead struct {
	conf gedata.ClusterConf
	err  error
}

func newClusterReads(ctx context.Context, admin *gedata.Admin) *clusterReads {
	return &clusterReads{
		ctx:       ctx,
		admin:     admin,
		queues:    map[string]queueRead{},
		execHosts: map[string]execHostRead{},
		locals:    map[string]confRead{},
	}
}

func (c *clusterReads) queue(name string) (gedata.Queue, error) {
	r, ok := c.queues[name]
	if !ok {
		r.queue, r.err = c.admin.Queue(c.ctx, name)
		c.queues[name] = r
	}
	return r.queue, r.err
}

func (c *clusterReads) execHost(name string) (gedata.ExecHost, error) {
	r, ok := c.execHosts[name]
	if !ok {
		r.host, r.err = c.admin.ExecHost(c.ctx, name)
		c.execHosts[name] = r
	}
	return r.host, r.err
}

func (c *clusterReads) globalConf() (gedata.ClusterConf, error) {
	if c.global == nil {
		conf, err := c.admin.GlobalConf(c.ctx)
		c.global = &confRead{conf: conf, err: err}
	}
	return c.global.conf, c.global.err
}

// localConf returns the host's local configuration; found is false when the
// host has none, which is normal (the global one applies). Which hosts have
// one comes from a single qconf -sconfl, so a host without one costs nothing.
func (c *clusterReads) localConf(host string) (conf gedata.ClusterConf, found bool, err error) {
	if c.listErr() != nil {
		return gedata.ClusterConf{}, false, c.localErr
	}
	if !c.localHosts[host] {
		return gedata.ClusterConf{}, false, nil
	}
	r, ok := c.locals[host]
	if !ok {
		r.conf, r.err = c.admin.HostConf(c.ctx, host)
		c.locals[host] = r
	}
	return r.conf, true, r.err
}

// listErr is the error of the one qconf -sconfl, nil when it succeeded. A
// caller that would report the same failure per host reports it once instead.
func (c *clusterReads) listErr() error {
	if !c.listed {
		c.listed = true
		names, err := c.admin.HostsWithLocalConf(c.ctx)
		if err != nil {
			c.localErr = err
		} else {
			c.localHosts = map[string]bool{}
			for _, n := range names {
				c.localHosts[n] = true
			}
		}
	}
	return c.localErr
}
