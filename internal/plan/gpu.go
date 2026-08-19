package plan

// AssignDevices decides which of a node's granted devices each local rank sees
// as CUDA_VISIBLE_DEVICES (REQ-GPU-002).
//
// The default follows SLURM: without an explicit per-task binding request, tasks
// are NOT bound to a subset, so every local rank sees the node's whole granted
// set. SLURM only restricts a task when asked -- --gpus-per-task implies
// --gpu-bind=per_task -- and frameworks that pick their device by local rank
// (JAX's local_device_ids=[SLURM_LOCALID], torch's LOCAL_RANK) depend on that
// full view: masking every rank down to one device makes rank i>0 index past the
// end of its own visible list.
//
// gpusPerTask>0 is an explicit request: local rank i gets [i*g,(i+1)*g).
// autoDivide restores the shim's legacy behavior (gpu.bind: per-task) of
// splitting the grant evenly when no explicit request was made; it is opt-in.
// With autoDivide and fewer devices than ranks, every rank shares the whole list
// and shared is true so the caller can warn once. An empty device list yields
// empty per-rank sets and shared false (a GPU-less node is not "sharing").
// Returned slices are views into devices; callers that mutate must copy.
func AssignDevices(devices []int, localTasks, gpusPerTask int, autoDivide bool) (perRank [][]int, shared bool) {
	if localTasks < 1 {
		localTasks = 1
	}
	perRank = make([][]int, localTasks)

	g := gpusPerTask
	if g <= 0 {
		if !autoDivide {
			// SLURM's default: no binding, the whole grant stays visible.
			for i := range perRank {
				perRank[i] = devices
			}
			return perRank, false
		}
		g = len(devices) / localTasks
		// g==0 with at least one device means fewer GPUs than tasks: every rank
		// shares the whole list. With no devices at all there is nothing to
		// share, so fall through to the loop, which assigns each rank an empty
		// set (a GPU-less node is not "sharing").
		if g == 0 && len(devices) > 0 {
			shared = true
			for i := range perRank {
				perRank[i] = devices
			}
			return perRank, shared
		}
	}

	for i := 0; i < localTasks; i++ {
		lo := i * g
		hi := lo + g
		switch {
		case lo >= len(devices):
			perRank[i] = []int{}
		case hi > len(devices):
			perRank[i] = devices[lo:]
		default:
			perRank[i] = devices[lo:hi]
		}
	}
	return perRank, shared
}
