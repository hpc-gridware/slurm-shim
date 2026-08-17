package plan

// AssignDevices partitions a node's granted device list among its local ranks
// (REQ-GPU-002). With gpusPerTask>0, local rank i gets [i*g,(i+1)*g). Without it
// (gpusPerTask<=0), g = floor(devices/localTasks); if g==0 and there is at least
// one device, every rank shares the full list and shared is true so the caller
// can warn once. An empty device list yields empty per-rank sets and shared
// false (a GPU-less node is not "sharing"). Returned slices are views into
// devices; callers that mutate must copy.
func AssignDevices(devices []int, localTasks, gpusPerTask int) (perRank [][]int, shared bool) {
	if localTasks < 1 {
		localTasks = 1
	}
	perRank = make([][]int, localTasks)

	g := gpusPerTask
	if g <= 0 {
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
