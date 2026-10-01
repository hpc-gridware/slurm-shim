package srun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
	"github.com/hpc-gridware/slurm-shim/internal/launch"
	"github.com/hpc-gridware/slurm-shim/internal/layout"
	"github.com/hpc-gridware/slurm-shim/internal/plan"
	"github.com/hpc-gridware/slurm-shim/internal/proto"
)

// Hot spares (sbatch --x-spares). A job with spares holds k idle granted hosts
// beyond its nodes. When an elastic step loses a remote node, srun relaunches
// that node's tasks on a spare -- same node index, same ranks, the spare's own
// GPUs -- through qrsh -inherit, which the spare's execd already accepts because
// it was granted to the job from the start. Elastic frameworks (torchrun's
// elastic agent) then re-form the group around the replacement. The Grid Engine
// job itself never leaves RUNNING.

// errNoSpare reports that every spare is used up.
var errNoSpare = errors.New("no spare left")

// errStopping reports that srun began stopping the step while a spare started.
var errStopping = errors.New("srun is stopping the step")

// elasticStep decides once per step whether a lost node is replaced by a spare:
// only in a job with spares, and per --x-elastic (then the job's SLURM_X_ELASTIC
// from sbatch, then the site's `elastic`): on, off, or auto -- elastic for
// torchrun steps, whose agent can take a replacement, and not for anything else,
// where relaunching one node's tasks would leave the rest of the program waiting
// on a peer that restarted from scratch.
func (s *supervisor) elasticStep() bool {
	if len(s.lay.Spares) == 0 {
		return false
	}
	mode := s.opt.elastic
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("SLURM_X_ELASTIC")))
	}
	if mode == "" {
		mode = s.cfg.Elastic
	}
	switch mode {
	case config.ElasticOn:
		return true
	case config.ElasticOff:
		return false
	default:
		return isTorchrun(s.opt.command)
	}
}

// isTorchrun recognizes the torchrun launcher: the torchrun script, or
// python [flags] -m torch.distributed.run.
func isTorchrun(cmd []string) bool { return torchrunOptsStart(cmd) >= 0 }

// torchrunOptsStart is the index of torchrun's first own option in cmd, or -1
// when cmd is not torchrun.
func torchrunOptsStart(cmd []string) int {
	if len(cmd) == 0 {
		return -1
	}
	if filepath.Base(cmd[0]) == "torchrun" {
		return 1
	}
	if !strings.HasPrefix(filepath.Base(cmd[0]), "python") {
		return -1
	}
	for i := 1; i+1 < len(cmd); i++ {
		if cmd[i] == "-m" && cmd[i+1] == "torch.distributed.run" {
			return i + 2
		}
	}
	return -1
}

// canReplace reports whether a stepper's end may be answered with a spare: an
// elastic step, not one srun is already stopping (kill-on-bad-exit, a forwarded
// SIGTERM, abandon), a remote node (the master runs srun itself), and none of
// its tasks reported yet -- a torchrun agent reports only at the end, and
// relaunching tasks that already finished would run them twice.
func (s *supervisor) canReplace(host string, reported int) bool {
	if !s.elastic || s.terminating.Load() || reported > 0 {
		return false
	}
	ni := s.stepNodeIndex(host)
	return ni >= 0 && s.plan.Nodes[ni].LayoutIndex != 0
}

// replace relaunches a stepper's tasks on the next spare and returns the
// spare's host and channel. hostDown says the node stopped answering (kernel
// liveness): only then is it drained. Any other early end of a stepper -- a
// crash, an execd limit -- may be a fault of the program rather than the host,
// so it gets one spare per step and never a drain; a second one fails the
// tasks instead of burning every spare on the same failure. A spare that does
// not start is consumed and drained, and the next one tried. It returns false
// when no spare took over; the caller then fails the node's tasks as without
// spares.
func (s *supervisor) replace(lost string, hostDown bool) (string, *proto.Conn, bool) {
	ni := s.stepNodeIndex(lost)
	what := "node " + lost + " lost"
	if !hostDown {
		what = "stepper on " + lost + " ended before its tasks reported"
		if s.softSwapped {
			errln(s.stderr, fmt.Sprintf("srun: error: %s again; not using another spare for it", what))
			return "", nil, false
		}
		s.softSwapped = true
	}
	for !s.terminating.Load() {
		spare, used, total, err := s.takeSpare(lost)
		if err != nil {
			errln(s.stderr, fmt.Sprintf("srun: error: %s and %v", what, spareStatus(err, used, total)))
			return "", nil, false
		}
		if hostDown {
			host := lost
			s.background(func() { s.drain(host) })
		}
		s.background(s.recordSwaps)
		c, err := s.relaunch(ni, spare)
		if err == nil {
			errln(s.stderr, fmt.Sprintf("srun: %s; tasks %s relaunched on %s (%d/%d spares used)",
				what, s.rankList(ni), spare.Host, used, total))
			return spare.Host, c, true
		}
		errln(s.stderr, fmt.Sprintf("srun: warning: spare %s did not take over from %s: %v", spare.Host, lost, err))
		if !errors.Is(err, launch.ErrHostUnusable) {
			return "", nil, false // not the spare's fault: the next one would fail the same way
		}
		// The spare holds the node index now; replace it in turn.
		lost, hostDown, what = spare.Host, true, "spare "+spare.Host+" unusable"
	}
	return "", nil, false
}

func spareStatus(err error, used, total int) string {
	if errors.Is(err, errNoSpare) {
		return fmt.Sprintf("no spare left (%d/%d used); its tasks are treated as failed", used, total)
	}
	return fmt.Sprintf("taking a spare failed (%v); its tasks are treated as failed", err)
}

// takeSpare moves the next spare into the lost node's slot in the job's layout,
// under the layout lock, so concurrent steps and every later step see the swap:
// the spare takes the lost node's index, the lost host is recorded, and the next
// srun places tasks on the spare. When another step has already replaced the same
// node, its replacement is reused instead of consuming a second spare.
func (s *supervisor) takeSpare(lost string) (spare layout.Node, used, total int, err error) {
	dir, err := stateDir()
	if err != nil {
		return spare, 0, 0, err
	}
	var updated *layout.Layout
	err = layout.Update(dir, func(l *layout.Layout) error {
		used, total = len(l.Swaps), len(l.Swaps)+len(l.Spares)
		idx := -1
		for i, n := range l.Nodes {
			if n.Host == lost {
				idx = i
			}
		}
		if idx < 0 {
			// Follow lost -> spare -> spare ...: the replacement may itself have
			// been replaced since.
			host := lost
			for range l.Swaps {
				next := ""
				for _, sw := range l.Swaps {
					if sw.Lost == host {
						next = sw.Spare
					}
				}
				if next == "" {
					break
				}
				host = next
				for _, n := range l.Nodes {
					if n.Host == host {
						spare, updated = n, l
						return nil
					}
				}
			}
			return fmt.Errorf("node %s is not part of this job", lost)
		}
		if len(l.Spares) == 0 {
			return errNoSpare
		}
		spare = l.Spares[0]
		spare.Index = idx
		l.Spares = l.Spares[1:]
		l.Nodes[idx] = spare
		l.Lost = append(l.Lost, lost)
		l.Swaps = append(l.Swaps, layout.Swap{Lost: lost, Spare: spare.Host, Step: s.stepID, Unix: time.Now().Unix()})
		used = len(l.Swaps)
		updated = l
		return nil
	})
	if err == nil {
		s.setLayout(updated)
	}
	return spare, used, total, err
}

// setLayout replaces the in-memory layout with the written file's contents;
// drain reads the job id from another goroutine.
func (s *supervisor) setLayout(l *layout.Layout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.lay = *l
}

// relaunch starts the stepper for step node ni on spare: the node's tasks get
// the spare's host and its own GPU grant (assigned to the ranks exactly as the
// step assigned the lost node's), and their output files are appended to.
func (s *supervisor) relaunch(ni int, spare layout.Node) (*proto.Conn, error) {
	s.placeOnSpare(ni, spare)
	env := proto.Envelope{
		JobID:  s.lay.Job.JobID,
		StepID: s.stepID,
		Host:   spare.Host,
		NodeID: ni,
		Dial:   s.dialAddr(s.srv.Addr(), s.remote),
	}
	h, err := s.slave.Start(context.Background(), spare.Host, env, s.token)
	if err != nil {
		return nil, err
	}
	s.addHandle(h)

	c, err := s.acceptFrom(spare.Host)
	if err != nil {
		return nil, launch.HostError(fmt.Errorf("its stepper did not connect: %w", err))
	}
	// Registered before its ranks start, and only while srun is not stopping the
	// step: a signal forwarded or an abandon run from now on reaches it. Closed
	// otherwise, so the stepper takes its orphan path and exits with a code.
	if !s.addLiveConn(c) {
		_ = c.Close()
		return nil, errStopping
	}
	s.watchRemoteHosts(map[string]*proto.Conn{spare.Host: c})

	payload, err := proto.EncodeSpec(s.relaunchSpec(ni))
	if err == nil {
		err = c.Send(proto.Frame{Type: proto.FrameSpec, Payload: payload})
	}
	if err != nil {
		_ = c.Close()
		return nil, launch.HostError(err)
	}
	return c, nil
}

// acceptFrom waits up to launch_timeout for host's stepper. A stepper of an
// earlier spare that connects late is closed and skipped, so it cannot be taken
// for this one (it then exits with a code, as any orphaned stepper).
func (s *supervisor) acceptFrom(host string) (*proto.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.LaunchTimeout.Duration)
	defer cancel()
	for {
		c, err := s.srv.Accept(ctx)
		if err != nil {
			return nil, err
		}
		if c.Host == host {
			return c, nil
		}
		_ = c.Close()
	}
}

// placeOnSpare moves step node ni onto spare: its host, and its own GPU grant
// assigned to the node's ranks exactly as the step assigned the lost node's.
func (s *supervisor) placeOnSpare(ni int, spare layout.Node) {
	s.plan.Nodes[ni] = plan.StepNode{Host: spare.Host, LayoutIndex: spare.Index, Slots: spare.Slots, GPUs: spare.GPUs}
	var local []int
	for i, r := range s.plan.Ranks {
		if r.StepNodeIndex == ni {
			local = append(local, i)
		}
	}
	perRank, _ := plan.AssignDevices(spare.GPUs, len(local), s.opt.req.GPUsPerTask, s.opt.req.AutoDivideGPUs)
	for k, i := range local {
		s.plan.Ranks[i].GPUs = perRank[k]
	}
}

// relaunchSpec is the StepSpec for step node ni after it moved to a spare. The
// step-scoped variables are recomputed, so SLURM_STEP_NODELIST names the spare
// rather than the lost host, and rank output is appended to, not truncated.
func (s *supervisor) relaunchSpec(ni int) proto.StepSpec {
	spec := s.stepSpec(s.baseEnv(), ni)
	spec.AppendOutput = true
	return spec
}

// startOrSpare starts the stepper for step node ni at launch. When a remote
// node of an elastic step refuses it -- its execd never takes the task -- the
// node moves to the next spare and the start is retried there. A host that is
// powered off or cut off usually makes qrsh hang rather than fail; that case
// still ends at launch_timeout and costs the step.
func (s *supervisor) startOrSpare(ni int, start func(node plan.StepNode) (launch.Handle, error)) (launch.Handle, error) {
	for {
		node := s.plan.Nodes[ni]
		h, err := start(node)
		// Only a host fault costs a spare: an error that would repeat on every
		// host (spawning qrsh, slots held by other steps) fails the step as is.
		if err == nil || !s.elastic || node.LayoutIndex == 0 || !errors.Is(err, launch.ErrHostUnusable) {
			return h, err
		}
		spare, used, total, terr := s.takeSpare(node.Host)
		if terr != nil {
			errln(s.stderr, fmt.Sprintf("srun: error: node %s did not start (%v) and %v", node.Host, err, spareStatus(terr, used, total)))
			return nil, err
		}
		host := node.Host
		s.background(func() { s.drain(host) })
		s.background(s.recordSwaps)
		s.placeOnSpare(ni, spare)
		errln(s.stderr, fmt.Sprintf("srun: node %s did not start (%v); tasks %s placed on %s (%d/%d spares used)",
			node.Host, err, s.rankList(ni), spare.Host, used, total))
	}
}

// recordSwaps stores the job's swaps in its job context, so squeue and scontrol
// outside the job show the replacement instead of the lost node. qalter -ac on
// a running job needs a submit host, so this is best-effort: inside the job the
// layout is the truth either way. Array tasks are skipped -- job context is
// shared by every task of the array. Calls are serialized and each writes the
// layout file's current list, so a slower, older write cannot drop a swap.
func (s *supervisor) recordSwaps() {
	if !s.remote {
		return // the local launcher (dev, tests): no Grid Engine job to record in
	}
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	dir, err := stateDir()
	if err != nil {
		return
	}
	lay, err := layout.Read(filepath.Join(dir, layout.LayoutFile))
	if err != nil || lay.Job.ArrayTaskID != nil || len(lay.Swaps) == 0 {
		return
	}
	pairs := make([]string, len(lay.Swaps))
	for i, sw := range lay.Swaps {
		pairs[i] = sw.Lost + ":" + sw.Spare
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.QstatTimeout.Duration)
	defer cancel()
	if err := gedata.RecordSwaps(ctx, s.ge(), strconv.FormatInt(lay.Job.JobID, 10), pairs); err != nil {
		s.swapRecordWarning.Do(func() {
			errln(s.stderr, "srun: warning: the swap is not visible to squeue/scontrol outside the job ("+err.Error()+")")
		})
	}
}

// drain runs the site's drain_command for a node swapped out, so the failed
// host stops receiving new work. The shim never reconfigures the cluster itself
// (spec non-goal 4): what "drain" means -- a load-sensor marker, a privileged
// qmod -d helper -- is the site's script. Failure is only a warning.
func (s *supervisor) drain(host string) {
	if len(s.cfg.DrainCommand) == 0 {
		return
	}
	s.mu.Lock()
	jobID := s.lay.Job.JobID
	s.mu.Unlock()
	repl := strings.NewReplacer("{host}", host, "{job}", strconv.FormatInt(jobID, 10), "{reason}", "slurm-shim: node lost during step")
	args := make([]string, len(s.cfg.DrainCommand))
	for i, a := range s.cfg.DrainCommand {
		args[i] = repl.Replace(a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DrainTimeout.Duration)
	defer cancel()
	_, errOut, exit, err := s.ge().Run(ctx, args[0], args[1:]...)
	if err == nil && exit != 0 {
		err = fmt.Errorf("exit %d: %s", exit, strings.TrimSpace(string(errOut)))
	}
	if err != nil {
		errln(s.stderr, fmt.Sprintf("srun: warning: drain_command for %s failed: %v", host, err))
	}
}

// background runs a swap's side effect (drain, job-context record) without
// holding up the step; launch waits for them before srun exits, so a short step
// after a swap does not cut them off. Each is bounded by its own timeout.
func (s *supervisor) background(f func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f()
	}()
}

// ge is the runner for the Grid Engine and drain commands srun runs itself.
func (s *supervisor) ge() gedata.Runner {
	if s.runner != nil {
		return s.runner
	}
	return gedata.ExecRunner{}
}

// stepNodeIndex is host's index among the step's nodes, or -1.
func (s *supervisor) stepNodeIndex(host string) int {
	for i, n := range s.plan.Nodes {
		if n.Host == host {
			return i
		}
	}
	return -1
}

// rankList names the ranks on step node ni, compressed ("4-7").
func (s *supervisor) rankList(ni int) string {
	var ranks []int
	for _, r := range s.plan.Ranks {
		if r.StepNodeIndex == ni {
			ranks = append(ranks, r.Rank)
		}
	}
	return rankRange(ranks)
}

// rankRange renders ascending ranks as "4-7" when contiguous, else "0,2,5".
func rankRange(ranks []int) string {
	if len(ranks) == 0 {
		return ""
	}
	if ranks[len(ranks)-1]-ranks[0] == len(ranks)-1 {
		if len(ranks) == 1 {
			return strconv.Itoa(ranks[0])
		}
		return fmt.Sprintf("%d-%d", ranks[0], ranks[len(ranks)-1])
	}
	parts := make([]string, len(ranks))
	for i, r := range ranks {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, ",")
}

// torchrunWarnings checks an elastic torchrun step for settings that keep
// torchrun from taking a replacement node. Advice only: the step runs either way.
func (s *supervisor) torchrunWarnings() []string {
	if !s.elastic || !isTorchrun(s.opt.command) {
		return nil
	}
	args := torchrunArgs(s.opt.command)
	var warns []string
	spares := len(s.lay.Spares)
	if n, err := strconv.Atoi(args["max-restarts"]); err != nil || n < spares {
		warns = append(warns, fmt.Sprintf("torchrun --max-restarts=%s is below the job's %d spare(s): "+
			"a node swap can cost a restart (when the workers crash before the replacement joins); "+
			"use --max-restarts=%d or more", orUnset(args["max-restarts"]), spares, spares))
	}
	if v, ok := os.LookupEnv(shareStoreVar); ok && v != "1" {
		warns = append(warns, fmt.Sprintf("%s=%s: torchrun then cannot hand a replacement node its workers' "+
			"master address, so a swap fails after the rendezvous timeout; leave it unset (srun sets it to 1)",
			shareStoreVar, v))
	}
	if b := args["rdzv-backend"]; b != "c10d" {
		warns = append(warns, fmt.Sprintf("torchrun --rdzv-backend=%s cannot take a replacement node; use --rdzv-backend=c10d",
			orUnset(b)))
	}
	if ep := args["rdzv-endpoint"]; ep != "" {
		host := ep
		if h, _, ok := strings.Cut(ep, ":"); ok {
			host = h
		}
		if master := s.lay.Rendezvous.MasterAddr; master != "" && host != master &&
			host != "$MASTER_ADDR" && host != "${MASTER_ADDR}" && host != s.lay.Nodes[0].Host {
			warns = append(warns, fmt.Sprintf("torchrun --rdzv-endpoint=%s is not on the master host %s: "+
				"losing that host ends the whole run, and only the master is never replaced", ep, master))
		}
	}
	if nn := args["nnodes"]; nn != "" && !nnodesCovers(nn, len(s.plan.Nodes)) {
		warns = append(warns, fmt.Sprintf("torchrun --nnodes=%s does not match the step's %d node(s)", nn, len(s.plan.Nodes)))
	}
	return warns
}

// shareStoreVar opts torchrun out of sharing its rendezvous TCP store with the
// workers (torch 2.4+). With sharing, rank 0 publishes the workers' MASTER_ADDR
// only in the first rendezvous round, so a replacement node joining a later
// round waits for it until the rendezvous times out: every swap would fail.
const shareStoreVar = "TORCH_DISABLE_SHARE_RDZV_TCP_STORE"

// withElasticEnv adds the torchrun settings an elastic step needs to take a
// replacement node, unless the user set them.
func (s *supervisor) withElasticEnv(env []string) []string {
	if !s.elastic || !isTorchrun(s.opt.command) {
		return env
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, shareStoreVar+"=") {
			return env
		}
	}
	return append(env, shareStoreVar+"=1")
}

// torchrunArgs reads torchrun's own options (before the training script) into
// a map keyed by the dashed name; torchrun accepts both --max-restarts and
// --max_restarts, with "=" or as the next word.
func torchrunArgs(cmd []string) map[string]string {
	args := map[string]string{}
	start := torchrunOptsStart(cmd)
	if start < 0 {
		return args
	}
	for i := start; i < len(cmd); i++ {
		a := cmd[i]
		if !strings.HasPrefix(a, "--") {
			break // the training script: what follows are its own arguments
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		name = strings.ReplaceAll(name, "_", "-")
		if !hasVal && !torchrunBoolFlags[name] && i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "--") {
			i++
			val = cmd[i]
		}
		args[name] = val
	}
	return args
}

// torchrunBoolFlags are torchrun's options that take no value, so the word after
// them is not read as their value.
var torchrunBoolFlags = map[string]bool{"standalone": true, "no-python": true, "run-path": true, "module": true}

// nnodesCovers reports whether torchrun's --nnodes (N, or an elastic MIN:MAX
// range) admits n nodes.
func nnodesCovers(nnodes string, n int) bool {
	lo, hi, isRange := strings.Cut(nnodes, ":")
	if !isRange {
		hi = lo
	}
	min, err1 := strconv.Atoi(lo)
	max, err2 := strconv.Atoi(hi)
	return err1 == nil && err2 == nil && min <= n && n <= max
}

func orUnset(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}
