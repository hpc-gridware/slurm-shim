package launch

// Supervise exposes the signal-forwarding loop to the external specs, which
// cannot signal their own process: Ginkgo treats SIGTERM as an interrupt.
var Supervise = supervise
