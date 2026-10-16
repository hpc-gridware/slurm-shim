/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package proto

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// SetLiveness makes the kernel detect a dead or unreachable peer HOST on this
// connection within about timeout (REQ-CHN-004). TCP keepalive probes the idle
// connection every interval, and on Linux TCP_USER_TIMEOUT bounds how long sent
// data may stay unacknowledged. When either gives up, the next Recv or Send fails
// with ETIMEDOUT and the caller's channel-loss path takes over.
//
// It is the peer's kernel that answers, not the peer process: a stopped process
// (a suspended job, a SIGSTOP, a debugger) or one that is merely slow to read
// keeps its host ACKing, so only a crashed, powered-off or partitioned host times
// out. That is what lets a suspended step survive without asking Grid Engine.
func (c *Conn) SetLiveness(timeout, interval time.Duration) error {
	tc, ok := c.nc.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("liveness needs a TCP connection, got %T", c.nc)
	}
	if timeout <= 0 || interval <= 0 {
		return fmt.Errorf("liveness needs a positive timeout and interval (got %s, %s)", timeout, interval)
	}
	// Keepalive granularity is one second; probe at least twice before giving up.
	interval = max(interval.Round(time.Second), time.Second)
	count := max(int((timeout-interval)/interval), 2)
	if err := tc.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable: true, Idle: interval, Interval: interval, Count: count,
	}); err != nil {
		return err
	}
	return setUserTimeout(tc, timeout)
}

// IsLivenessTimeout reports whether err is a connection that SetLiveness gave up
// on (the peer host stopped answering), as opposed to a peer that closed it.
func IsLivenessTimeout(err error) bool {
	return errors.Is(err, syscall.ETIMEDOUT)
}
