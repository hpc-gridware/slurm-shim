package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// MergeInto renders cfg over an existing config document, PRESERVING any key the
// Config struct does not model.
//
// Why this exists. `install --apply` used to write config.Render(cfg) over the
// file wholesale, which means the document is a round-trip through a Go struct:
// every key the running binary does not have a field for is silently dropped.
// Observed in the field on a GCP cluster --
//
//	gpu:
//	  isolation: shim
//	  gres_complex: gpu
//	  bind: none
//	  vendor: nvidia        <- deliberate, site-specific
//
// came back without `vendor`, because the installed binary predated that field.
// The indentation changed from 2 to 4 spaces at the same time, which is the tell
// that the file had been re-marshalled rather than edited.
//
// The mechanism matters more than that one key, and it is not version-specific:
// upgrade then roll back, run the installer from an older binary than the one
// that wrote the config, re-apply from the wrong node of a mixed-version
// cluster, or hand-add a key a later release will model -- all lose data, and
// all lose it SILENTLY, because the resulting config is still valid and the
// cluster still runs, with a setting the operator believes is in force.
//
// The merge is RECURSIVE because the case that bit was a nested key: `vendor`
// under `gpu:`, not a top-level one.
func MergeInto(existing []byte, cfg *Config) ([]byte, error) {
	rendered, err := Render(cfg)
	if err != nil {
		return nil, err
	}
	// Nothing to preserve: first install, or an empty file.
	if len(bytes.TrimSpace(existing)) == 0 {
		return rendered, nil
	}

	var oldDoc, newDoc yaml.Node
	if err := yaml.Unmarshal(existing, &oldDoc); err != nil {
		// Refuse rather than overwrite. A file we cannot parse may still hold
		// settings somebody meant; destroying it is the one outcome this
		// function exists to prevent.
		return nil, fmt.Errorf("existing config is not valid YAML (%w); "+
			"fix or move it rather than have the installer overwrite it", err)
	}
	if err := yaml.Unmarshal(rendered, &newDoc); err != nil {
		return nil, err
	}

	oldRoot, newRoot := docRoot(&oldDoc), docRoot(&newDoc)
	if oldRoot == nil || oldRoot.Kind != yaml.MappingNode {
		// An existing document that is not a mapping (a list, a scalar) has no
		// keys to preserve and nothing sensible to merge into.
		return rendered, nil
	}
	if newRoot == nil || newRoot.Kind != yaml.MappingNode {
		return rendered, nil
	}

	merged := mergeMapping(oldRoot, newRoot, true)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(merged); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// docRoot unwraps a DocumentNode to the value it contains.
func docRoot(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	if n.Kind == 0 {
		return nil // empty document
	}
	return n
}

// mergeMapping overlays src onto dst and returns the result.
//
// Rules, in the order they matter:
//   - a key in BOTH takes src's value -- the installer owns the keys it models
//   - a key only in dst SURVIVES -- this is the whole point
//   - when both values are mappings, recurse, so a nested unknown key (gpu.vendor)
//     is preserved even though its parent is known
//   - key order follows dst first, so an existing file keeps its shape and the
//     diff after an install stays small
//   - a key the shim RETIRED is dropped rather than preserved (see retiredKeys);
//     `top` marks the root mapping, the only level retirement applies at
func mergeMapping(dst, src *yaml.Node, top bool) *yaml.Node {
	out := &yaml.Node{
		Kind:        yaml.MappingNode,
		Tag:         dst.Tag,
		Style:       dst.Style,
		HeadComment: dst.HeadComment,
		LineComment: dst.LineComment,
		FootComment: dst.FootComment,
	}

	srcVal := map[string]*yaml.Node{}
	srcKey := map[string]*yaml.Node{}
	var srcOrder []string
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		srcVal[k.Value], srcKey[k.Value] = v, k
		srcOrder = append(srcOrder, k.Value)
	}

	seen := map[string]bool{}
	for i := 0; i+1 < len(dst.Content); i += 2 {
		dk, dv := dst.Content[i], dst.Content[i+1]
		seen[dk.Value] = true
		sv, ok := srcVal[dk.Value]
		if !ok {
			// A key the shim itself retired is dropped, not preserved -- see
			// retiredKeys. Only applies at the top level, which is where every
			// key the shim has ever retired lived.
			if top && retiredKeys[dk.Value] != "" {
				continue
			}
			// Unknown to the installer: keep the key AND its comments verbatim.
			out.Content = append(out.Content, dk, dv)
			continue
		}
		if dv.Kind == yaml.MappingNode && sv.Kind == yaml.MappingNode {
			out.Content = append(out.Content, dk, mergeMapping(dv, sv, false))
			continue
		}
		// Scalar or sequence: the installer's VALUE wins, but the existing key
		// node is reused and the old value's comments are carried across, so a
		// site annotation like
		//
		//	vendor: nvidia   # deliberate: this cluster is NVIDIA
		//
		// is not quietly deleted by an install that only changed the value.
		out.Content = append(out.Content, dk, withComments(sv, dv))
	}

	// Keys the installer models that the file did not have yet.
	for _, k := range srcOrder {
		if !seen[k] {
			out.Content = append(out.Content, srcKey[k], srcVal[k])
		}
	}
	return out
}

// withComments returns a copy of src carrying any comments that were on old, so
// replacing a value does not discard the note an operator wrote beside it. src
// is not mutated: the same node can appear under several keys.
func withComments(src, old *yaml.Node) *yaml.Node {
	if old.HeadComment == "" && old.LineComment == "" && old.FootComment == "" {
		return src
	}
	cp := *src
	if cp.HeadComment == "" {
		cp.HeadComment = old.HeadComment
	}
	if cp.LineComment == "" {
		cp.LineComment = old.LineComment
	}
	if cp.FootComment == "" {
		cp.FootComment = old.FootComment
	}
	return &cp
}

// retiredKeys are keys the shim ITSELF wrote and has since removed, mapped to
// what an operator should know about the removal.
//
// This is the one exception to MergeInto's preserve-everything rule, and the
// distinction is the point: MergeInto keeps keys it does not UNDERSTAND,
// because they may be a site setting a newer binary models. A retired key is
// one it understands exactly -- the shim put it there itself, and then dropped
// it. Keeping it would be preserving our own litter.
//
// Without this, 079 and 080 combine into a regression neither causes alone.
// Every config ever written by `install --apply` contains `launch_ramp: 64` and
// `control_port: ""`, because both had non-omitempty defaults. Once the field is
// gone, Parse reports the key as unknown -- and Parse runs on EVERY command, so
// srun, sbatch, squeue and sinfo each print a warning line, on every
// invocation, forever, with the srun one landing interleaved in job output.
// That is precisely the delivery failure the SI-51 warning was removed for.
// Before MergeInto, one `install --apply` scrubbed the key; the merge would
// have made it permanent.
var retiredKeys = map[string]string{
	"launch_ramp":  "it was never read -- no launch throttle existed to tune",
	"control_port": "superseded by control_port_base and control_port_range",
}

// retiredKeyWarning describes a key the shim has retired, or "" if the key is
// merely unrecognized. Retired keys get a migration message rather than the
// generic unknown-key warning, because the shim knows both that it wrote the
// key and how to say what replaced it.
func retiredKeyWarning(key string) string {
	reason, ok := retiredKeys[key]
	if !ok {
		return ""
	}
	return fmt.Sprintf("config key %q is obsolete and ignored: %s; "+
		"`slurm-shim install --apply` removes it", key, reason)
}
