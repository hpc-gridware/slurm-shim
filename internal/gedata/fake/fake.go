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

// Package fake provides test doubles for the gedata Runner and Identity seams.
// The Runner records every argv and replays canned responses so integration
// specs can drive GE-dependent code without a cluster (REQ-TST-002).
package fake

import "context"

// Call records one Runner invocation.
type Call struct {
	Name string
	Args []string
}

// Response is a canned Runner result.
type Response struct {
	Stdout []byte
	Stderr []byte
	Exit   int
	Err    error
}

// Runner is a recording, replaying Runner. Calls accumulates every invocation;
// Responder (if set) supplies the result, else an empty exit-0 response is
// returned.
type Runner struct {
	Calls     []Call
	Responder func(name string, args []string) Response
}

// Run records the call and returns the configured response.
func (r *Runner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	r.Calls = append(r.Calls, Call{Name: name, Args: append([]string(nil), args...)})
	if r.Responder == nil {
		return nil, nil, 0, nil
	}
	resp := r.Responder(name, args)
	return resp.Stdout, resp.Stderr, resp.Exit, resp.Err
}

// Identity is a deterministic Identity double for golden-file specs.
type Identity struct {
	HostnameVal string
	UserName    string
	UID         int
	GID         int
	IPs         map[string]string // host -> ip
	IfaceAddr   string
}

func (i Identity) Hostname() (string, error) { return i.HostnameVal, nil }

func (i Identity) User() (string, int, int, error) { return i.UserName, i.UID, i.GID, nil }

func (i Identity) LookupIP(_ context.Context, host string) (string, error) {
	if ip, ok := i.IPs[host]; ok {
		return ip, nil
	}
	return "", nil
}

func (i Identity) InterfaceAddr(string) (string, error) { return i.IfaceAddr, nil }
