# gedata test fixtures

Real output captured from a live OCS (Open Cluster Scheduler) 9.0.10 cluster
(`lx-arm64`, qmaster + two exec hosts), not hand-written. These pin the exact
Grid Engine formats the parsers must accept. See memory `ocs-test-cluster` for
how to regenerate.

## RSMAP GPU allocation (M7 / REQ-GPU-001)

Complex + host config (`qconf_gpu_rsmap.txt`):

    gpu  gpu  RSMAP  <=  YES  YES  0  0          # complex definition
    complex_values  gpu=2(0 1)                   # on each worker: 2 devices, ids 0 and 1

Requested with `qsub -l gpu=N`. The granted map is exposed three ways:

1. `SGE_HGR_gpu` in the job's own environment on the exec host
   (`job_env_sge_hgr.txt`): space-separated **local** device ids, no host
   prefix -- `SGE_HGR_gpu=0` (gpu=1), `SGE_HGR_gpu=0 1` (gpu=2). Per SI-19 this
   is NOT trusted for multi-host jobs: each host's execd sets only its own ids.

2. `qstat -j <id>` plain (`qstat_j_gpu{1,2}_plain.txt`): one flattened line
   `resource_map  1:  gpu=<host>=(<id> <id> ...)`.

3. `qstat -xml -j <id>` (`qstat_j_gpu{1,2}.xml`): structured and host-qualified,
   the multi-host-safe source. Under `.../JB_ja_tasks/element`:
   `JAT_granted_resources_list/element/{GRU_name, GRU_amount, GRU_host,
   GRU_resource_map_list/element/RESL_value}`. One `element` per host.

IMPORTANT: this OCS build's `qstat` has **no `-json` flag** (only `-xml`), so the
JSON view REQ-GPU-001/SI-19 prefers is unavailable here. `qstat -xml -j` carries
the same host-qualified map and is the structured source we parse. The plain
summary `qstat -xml` (no `-j`) does NOT carry granted resources -- only the
detailed `-j` view does.

## Memory request (M2 / REQ)

`qstat_j_mem_plain.txt` / `qstat_j_mem.xml`: `qsub -l h_vmem=2G`. Request encoding
is identical to gpu: plain `hard_resource_list: h_vmem=2G`; XML
`qstat_l_requests/{CE_name=h_vmem, CE_stringval=2G, CE_doubleval=2147483648}`
(bytes in CE_doubleval).
