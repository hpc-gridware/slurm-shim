"""submitit smoke test for slurm-shim over Open Cluster Scheduler.

Exercises the four things submitit needs from the scheduler: submit a function
and read its result, run a map_array and read every result (0-based array ids),
and surface a failure as an exception instead of hanging. Nothing here is
shim-aware -- it is plain submitit against cluster="slurm".
"""
import os
import sys

import submitit


def add(a, b):
    return a + b


def boom():
    raise RuntimeError("intentional failure")


def main():
    folder = os.environ.get("SUBMITIT_FOLDER", os.path.join(os.getcwd(), "submitit_logs"))
    ex = submitit.AutoExecutor(folder=folder, cluster="slurm")

    # submitit writes its own batch script, so (unlike the lightning/ray recipes)
    # it does not source the shim hook -- meaning `srun` is not on PATH inside the
    # non-login batch shell. Prepend the one shim-specific line that fixes it.
    setup = []
    hook = os.environ.get("SHIM_HOOK", "/opt/slurm-shim/etc/slurm-shim-source-hook.sh")
    if hook:
        setup.append(". %s" % hook)

    ex.update_parameters(
        # These are single-task jobs, so a $pe_slots partition keeps each one on one
        # host. Multi-node work (nodes=/tasks_per_node=) wants a round-robin PE
        # partition such as "batch" instead -- see the recipe README.
        slurm_partition=os.environ.get("PARTITION", "smp"),
        timeout_min=5,
        nodes=1,
        tasks_per_node=1,
        cpus_per_task=1,
        slurm_array_parallelism=2,
        slurm_setup=setup,
    )

    # 1. Single function: submit and read the result (rides on the result pickle).
    job = ex.submit(add, 2, 3)
    print("single job id:", job.job_id, flush=True)
    assert job.result() == 5, job.result()
    print("[ok] single submit -> 5", flush=True)

    # 2. Array via map_array: every task runs and returns its value; job ids are
    # 0-based (N_0..N_3), matching the files submitit reads back.
    jobs = ex.map_array(add, [1, 2, 3, 4], [10, 20, 30, 40])
    ids = [j.job_id for j in jobs]
    results = [j.result() for j in jobs]
    print("array ids:", ids, "->", results, flush=True)
    assert results == [11, 22, 33, 44], results
    assert all("_" in i for i in ids), ids
    print("[ok] map_array -> [11, 22, 33, 44]", flush=True)

    # 3. A failing function surfaces as an exception (does not hang), because
    # sacct reports a terminal state even though no success pickle is written.
    fj = ex.submit(boom)
    try:
        fj.result()
        print("[FAIL] boom did not raise")
        sys.exit(1)
    except Exception as e:  # noqa: BLE001 - any raised exception proves no hang
        print("[ok] failure surfaced:", type(e).__name__, flush=True)

    print("[result] SUBMITIT OK", flush=True)


if __name__ == "__main__":
    main()
